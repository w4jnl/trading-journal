package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// strategyFixture is a user with an empty library and a place for images.
func strategyWriteFixture(t *testing.T) (*Handlers, context.Context, int64) {
	t.Helper()
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	h := &Handlers{DB: conn, Queries: models.New(conn), Timezone: time.UTC, LiveData: livedata.NewNotifier(), AttachmentsDir: t.TempDir()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	return h, ctx, uid
}

// pngBase64 is a 2x2 PNG, the smallest image the type sniffer recognises.
func tinyPNGBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func mcpToolText(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty tool result %+v", res)
	}
	return res.Content[0].(mcpgo.TextContent).Text
}

// create_strategy stores the playbook, the rules and the images: an image
// the body names by ![caption](name) lands where it is named, one it does
// not name is appended; the result carries the stored URLs.
func TestMCPCreateStrategy(t *testing.T) {
	h, ctx, uid := strategyWriteFixture(t)
	img := tinyPNGBase64(t)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"name":      "ORB fade",
		"body":      "## Opening-range fade\n\nFade the first push.\n\n![the setup](setup)\n\nThen wait.",
		"checklist": []any{"Range is set", " ", "Stop above the high", "Range is set"},
		"images": []any{
			map[string]any{"name": "setup", "data": img},
			map[string]any{"name": "a later example", "data": "data:image/png;base64," + img},
		},
	}
	res, err := h.mcpCreateStrategy(ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("create = %v %s", err, mcpToolText(t, res))
	}
	var out struct {
		apiStrategy
		Images []apiStoredImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(mcpToolText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "ORB fade" || len(out.Images) != 2 || out.Images[0].Name != "setup" || out.Images[0].URL != "/api/v1/attachments/1" || out.Images[1].ID != 2 {
		t.Errorf("result = %+v", out)
	}
	if !strings.Contains(out.Body, "![the setup](/api/v1/attachments/1)") || !strings.HasSuffix(out.Body, "Then wait.\n\n![a later example](/api/v1/attachments/2)") {
		t.Errorf("body = %q", out.Body)
	}
	if len(out.Attachments) != 2 || strings.Join(out.Checklist, "|") != "Range is set|Stop above the high" {
		t.Errorf("attachments = %+v, checklist = %v", out.Attachments, out.Checklist)
	}
	// Stored as the trader's attachments, on disk and in the row's own form.
	s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: out.ID, UserID: uid})
	if err != nil || !strings.Contains(s.Body, "](/journal/attachments/1)") {
		t.Errorf("row = %+v %v", s, err)
	}
	var owner, n int64
	var file string
	if err := h.DB.QueryRow(`SELECT user_id, file_path, (SELECT count(*) FROM journal_attachments) FROM journal_attachments WHERE id = 1`).Scan(&owner, &file, &n); err != nil || owner != uid || n != 2 {
		t.Errorf("attachment row: owner %d of %d, %v", owner, n, err)
	}
	if _, err := os.Stat(filepath.Join(h.AttachmentsDir, file)); err != nil || !strings.HasSuffix(file, ".png") {
		t.Errorf("file %q: %v", file, err)
	}
	if v := h.LiveData.Version(uid); v == 0 {
		t.Error("the write did not bump live data")
	}

	// Refusals: a missing name, a taken name, a non-image, bad base64.
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"body": "x"}, "name is required"},
		{map[string]any{"name": "ORB fade"}, `"ORB fade" exists already`},
		{map[string]any{"name": "Other", "images": []any{map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("just text, not an image at all"))}}}, "only PNG, JPEG, GIF and WebP"},
		{map[string]any{"name": "Other", "images": []any{map[string]any{"data": "%%%not base64"}}}, "not base64"},
	} {
		req.Params.Arguments = c.args
		res, _ := h.mcpCreateStrategy(ctx, req)
		if !res.IsError || !strings.Contains(mcpToolText(t, res), c.want) {
			t.Errorf("%v: got %v %q, want error %q", c.args, res.IsError, mcpToolText(t, res), c.want)
		}
	}
	if rows, _ := h.Queries.ListStrategies(ctx, uid); len(rows) != 1 {
		t.Errorf("refused writes left %d strategies, want 1", len(rows))
	}
}

// update_strategy replaces what it is given and adds what is additive:
// the name and the checklist replace, append adds a paragraph, an image
// with no body lands at the end, the API image URLs an agent echoes back
// are stored in the row's own form, and the rest stays.
func TestMCPUpdateStrategy(t *testing.T) {
	h, ctx, uid := strategyWriteFixture(t)
	s, err := h.Queries.CreateStrategy(ctx, models.CreateStrategyParams{UserID: uid, Name: "Pullback", Body: "Buy the dip."})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.journalSvc().SetChecklistItems(ctx, uid, s.ID, []string{"Trend up", "Volume dries"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.CreateStrategy(ctx, models.CreateStrategyParams{UserID: uid, Name: "Breakout"}); err != nil {
		t.Fatal(err)
	}
	req := mcpgo.CallToolRequest{}
	call := func(args map[string]any) *mcpgo.CallToolResult {
		req.Params.Arguments = args
		res, err := h.mcpUpdateStrategy(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	id := float64(s.ID)
	// Append a paragraph and an image; the body, name and rules stay.
	res := call(map[string]any{"id": id, "append": "Never before 9:45.", "images": []any{map[string]any{"name": "drawing", "data": tinyPNGBase64(t)}}})
	if res.IsError {
		t.Fatalf("append: %s", mcpToolText(t, res))
	}
	var out apiStrategy
	_ = json.Unmarshal([]byte(mcpToolText(t, res)), &out)
	if out.Name != "Pullback" || out.Body != "Buy the dip.\n\nNever before 9:45.\n\n![drawing](/api/v1/attachments/1)" || strings.Join(out.Checklist, "|") != "Trend up|Volume dries" {
		t.Errorf("after append = %+v", out)
	}
	// Rename, replace the body (echoing the API image URL back) and the rules.
	res = call(map[string]any{"id": id, "name": "Pullback long", "body": "Rewritten.\n\n![drawing](/api/v1/attachments/1)", "checklist": []any{"Only one rule"}})
	if res.IsError {
		t.Fatalf("replace: %s", mcpToolText(t, res))
	}
	_ = json.Unmarshal([]byte(mcpToolText(t, res)), &out)
	if out.Name != "Pullback long" || out.Body != "Rewritten.\n\n![drawing](/api/v1/attachments/1)" || strings.Join(out.Checklist, "|") != "Only one rule" {
		t.Errorf("after replace = %+v", out)
	}
	row, _ := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: s.ID, UserID: uid})
	if row.Body != "Rewritten.\n\n![drawing](/journal/attachments/1)" {
		t.Errorf("row body = %q", row.Body)
	}
	// An empty checklist clears the rules; a missing one leaves them.
	if res := call(map[string]any{"id": id, "checklist": []any{}}); res.IsError {
		t.Fatalf("clear: %s", mcpToolText(t, res))
	} else if _ = json.Unmarshal([]byte(mcpToolText(t, res)), &out); len(out.Checklist) != 0 {
		t.Errorf("checklist not cleared: %v", out.Checklist)
	}
	// Refusals: a name in use, someone else's id, no id. (A blank name is
	// "not given" over MCP and leaves the name alone.)
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"id": id, "name": "Breakout"}, `"Breakout" exists already`},
		{map[string]any{"id": float64(999), "append": "x"}, "strategy not found"},
		{map[string]any{"append": "x"}, "id is required"},
	} {
		res := call(c.args)
		if !res.IsError || !strings.Contains(mcpToolText(t, res), c.want) {
			t.Errorf("%v: got %v %q, want error %q", c.args, res.IsError, mcpToolText(t, res), c.want)
		}
	}
	if row, _ := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: s.ID, UserID: uid}); row.Name != "Pullback long" {
		t.Errorf("a refused write changed the name: %q", row.Name)
	}
}

// The REST mirror: POST creates (201), PUT changes (200), GET shows the
// checklist, and the refusals carry the API's codes.
func TestAPIStrategyWrite(t *testing.T) {
	h, ctx, _ := strategyWriteFixture(t)
	mux := h.APIMux()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(rec, req)
		return rec
	}
	img := tinyPNGBase64(t)
	rec := do(http.MethodPost, "/api/v1/strategies", `{"name":"VWAP reclaim","body":"![mark](mark)\n\nReclaim and hold.","checklist":["Above VWAP"],"images":[{"name":"mark","data":"`+img+`"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST -> %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		apiStrategy
		Images []apiStoredImage `json:"images"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ID == 0 || out.Body != "![mark](/api/v1/attachments/1)\n\nReclaim and hold." || len(out.Images) != 1 || out.Images[0].ID != 1 || len(out.Checklist) != 1 {
		t.Errorf("created = %s", rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/strategies", `{"name":"VWAP reclaim"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate POST -> %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, "/api/v1/strategies", `{"body":"no name"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("nameless POST -> %d", rec.Code)
	}
	if rec := do(http.MethodPost, "/api/v1/strategies", `{"name":"x","images":[{"data":"`+base64.StdEncoding.EncodeToString([]byte("plain text and nothing else"))+`"}]}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text image POST -> %d %s", rec.Code, rec.Body.String())
	}
	idPath := fmt.Sprintf("/api/v1/strategies/%d", out.ID)
	rec = do(http.MethodPut, idPath, `{"append":"Size down on FOMC days.","checklist":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT -> %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasSuffix(out.Body, "Reclaim and hold.\n\nSize down on FOMC days.") || len(out.Checklist) != 0 || out.Name != "VWAP reclaim" {
		t.Errorf("updated = %s", rec.Body.String())
	}
	if rec := do(http.MethodGet, idPath, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"checklist":[]`) || !strings.Contains(rec.Body.String(), `"archivedAt":null`) {
		t.Errorf("GET -> %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPut, "/api/v1/strategies/999", `{"append":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("PUT unknown -> %d", rec.Code)
	}
	if rec := do(http.MethodPut, idPath, `{"name":`); rec.Code != http.StatusBadRequest {
		t.Errorf("PUT bad JSON -> %d", rec.Code)
	}
}
