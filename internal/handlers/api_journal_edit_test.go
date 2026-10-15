package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// journalEditFixture is the position fixture with a day entry, a device
// context and an agent context.
type journalEditFixture struct {
	*positionFixture
	device, agent context.Context
	day           models.JournalEntry
}

func newJournalEditFixture(t *testing.T) *journalEditFixture {
	t.Helper()
	fx := newPositionFixture(t)
	fx.h.AttachmentsDir = t.TempDir()
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fx.h.journalSvc().SaveDay(fx.ctx, fx.uid, day, journal.Fields{Body: "first words", Tag: strPtr("neutral"), Mistakes: []string{"chased"}}); err != nil {
		t.Fatal(err)
	}
	e, err := fx.q.GetDayJournalEntry(fx.ctx, models.GetDayJournalEntryParams{UserID: &fx.uid, EntryDate: day})
	if err != nil {
		t.Fatal(err)
	}
	return &journalEditFixture{positionFixture: fx,
		device: auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindDevice}),
		agent:  auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindAgent}), day: e}
}

func (fx *journalEditFixture) call(ctx context.Context, fn http.HandlerFunc, method string, id int64, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, "/api/v1/journal/x", strings.NewReader(body)).WithContext(ctx)
	if id > 0 {
		req.SetPathValue("id", strconv.FormatInt(id, 10))
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A device key replaces the entry as a page save does (labels replaced,
// tag cleared when absent, image refs stored in the server's form, the
// values marked as the trader's); an agent key is refused; validation
// answers 400; a stale updatedAt answers 409 with the current entry.
func TestAPIJournalUpdate(t *testing.T) {
	fx := newJournalEditFixture(t)
	if code, _ := fx.call(fx.agent, fx.h.APIJournalUpdate, http.MethodPut, fx.day.ID, `{"body": "x"}`); code != http.StatusForbidden {
		t.Errorf("agent key = %d, want 403", code)
	}
	code, out := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, fx.day.ID,
		`{"body": "edited on the phone ![](/api/v1/attachments/5)", "mood": 4, "emotions": ["calm"], "updatedAt": "`+fx.day.UpdatedAt.UTC().Format(time.RFC3339)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("put = %d: %v", code, out)
	}
	if out["tag"] != nil || out["mood"] != 4.0 || !strings.Contains(out["notes"].(string), "/api/v1/attachments/5") {
		t.Errorf("response = %v", out)
	}
	labels := out["labels"].(map[string]any)
	if len(labels["mistakes"].([]any)) != 0 || labels["emotions"].([]any)[0] != "calm" {
		t.Errorf("labels not replaced: %v", labels)
	}
	e, _ := fx.q.GetJournalEntryByID(fx.ctx, models.GetJournalEntryByIDParams{ID: fx.day.ID, UserID: &fx.uid})
	if !strings.Contains(e.Body, "/journal/attachments/5") || strings.Contains(e.Body, "/api/v1/") {
		t.Errorf("stored body = %q", e.Body)
	}
	if src := fx.h.journalSvc().EntryLabelSources(fx.ctx, fx.uid, fx.day.ID); src["emotion:calm"] == "api" {
		t.Errorf("a device edit must be the trader's, got sources %v", src)
	}

	for name, body := range map[string]string{
		"empty body":   `{"body": " "}`,
		"bad tag":      `{"body": "x", "tag": "great"}`,
		"bad mood":     `{"body": "x", "mood": 9}`,
		"bad strategy": `{"body": "x", "strategyId": 999}`,
		"bad label":    `{"body": "x", "mistakes": ["lazy"]}`,
		"bad time":     `{"body": "x", "updatedAt": "yesterday"}`,
		"not json":     `nope`,
	} {
		if code, out := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, fx.day.ID, body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v", name, code, out)
		}
	}
	code, out = fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, fx.day.ID, `{"body": "late", "updatedAt": "2020-01-01T00:00:00Z"}`)
	if code != http.StatusConflict || out["reason"] != "stale" || out["entry"].(map[string]any)["notes"] != "edited on the phone ![](/api/v1/attachments/5)" {
		t.Errorf("stale = %d %v", code, out)
	}
	if code, _ := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, 999999, `{"body": "x"}`); code != http.StatusNotFound {
		t.Errorf("unknown entry = %d", code)
	}
}

// The AI review lock refuses the save and the delete with reason
// review-pending; delete removes a day entry and answers 204.
func TestAPIJournalDeleteAndLock(t *testing.T) {
	fx := newJournalEditFixture(t)
	locked := fx.day.ID
	fx.h.journalSvc().Locked = func(id int64) bool { return id == locked }
	if code, out := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, fx.day.ID, `{"body": "x"}`); code != http.StatusConflict || out["reason"] != "review-pending" {
		t.Errorf("locked put = %d %v", code, out)
	}
	if code, out := fx.call(fx.device, fx.h.APIJournalDelete, http.MethodDelete, fx.day.ID, ""); code != http.StatusConflict || out["reason"] != "review-pending" {
		t.Errorf("locked delete = %d %v", code, out)
	}
	locked = 0
	if code, _ := fx.call(fx.agent, fx.h.APIJournalDelete, http.MethodDelete, fx.day.ID, ""); code != http.StatusForbidden {
		t.Errorf("agent delete = %d", code)
	}
	if code, _ := fx.call(fx.device, fx.h.APIJournalDelete, http.MethodDelete, fx.day.ID, ""); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if _, err := fx.q.GetJournalEntryByID(fx.ctx, models.GetJournalEntryByIDParams{ID: fx.day.ID, UserID: &fx.uid}); err == nil {
		t.Errorf("entry survived the delete")
	}
	if code, _ := fx.call(fx.device, fx.h.APIJournalDelete, http.MethodDelete, fx.day.ID, ""); code != http.StatusNotFound {
		t.Errorf("second delete = %d", code)
	}
}

// A trade entry's save goes through the trade path with checklist ticks
// and a strategy; the vocabulary lists names.
func TestAPIJournalUpdateTradeAndLabels(t *testing.T) {
	fx := newJournalEditFixture(t)
	oid := "O-9"
	if _, err := fx.q.InsertExecution(fx.ctx, models.InsertExecutionParams{AccountID: fx.acct.ID, BrokerExecID: "P9", OrderID: &oid, ImportMethod: "motivewave_csv",
		Symbol: "ESM6", SymbolRoot: "ES", Side: "sell", Qty: 3, Price: 7268, ExecTime: fx.t0.Add(40 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RebuildTradesForAccountSymbol(fx.ctx, fx.uid, fx.acct.ID, "ESM6"); err != nil {
		t.Fatal(err)
	}
	trades, _ := fx.q.ListTradesForAccount(fx.ctx, fx.acct.ID)
	if _, err := fx.h.journalSvc().SaveTrade(fx.ctx, fx.uid, trades[0].ID, journal.Fields{Body: "trade note"}); err != nil {
		t.Fatal(err)
	}
	e, err := fx.q.GetJournalEntryByTradeID(fx.ctx, &trades[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	strat, err := fx.q.CreateStrategy(fx.ctx, models.CreateStrategyParams{UserID: fx.uid, Name: "Open drive", Body: "..."})
	if err != nil {
		t.Fatal(err)
	}
	code, out := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, e.ID, `{"body": "held it", "tag": "good", "strategyId": `+strconv.FormatInt(strat.ID, 10)+`, "mistakes": ["late-exit"]}`)
	if code != http.StatusOK || out["tag"] != "good" || out["strategy"].(map[string]any)["name"] != "Open drive" {
		t.Fatalf("trade put = %d %v", code, out)
	}
	if code, out := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, e.ID, `{"body": "x", "mood": 3}`); code != http.StatusBadRequest {
		t.Errorf("mood on a trade entry = %d %v", code, out)
	}
	code, out = fx.call(fx.ctx, fx.h.APIJournalLabels, http.MethodGet, 0, "")
	if code != http.StatusOK || out["mistakes"].([]any)[0].(map[string]any)["name"] != "Chased the entry" || len(out["emotions"].([]any)) != 9 {
		t.Errorf("labels = %d %v", code, out)
	}
}

// An upload stores the image and returns the markdown line; a wrong type
// and an agent key are refused; the stored image is served to the key.
func TestAPIJournalAttachmentUpload(t *testing.T) {
	fx := newJournalEditFixture(t)
	upload := func(ctx context.Context, filename, mime string, data []byte) (int, map[string]any) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreatePart(map[string][]string{"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`}, "Content-Type": {mime}})
		_, _ = part.Write(data)
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/journal/attachments", &buf).WithContext(ctx)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		fx.h.APIJournalAttachmentUpload(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if code, _ := upload(fx.agent, "a.png", "image/png", png); code != http.StatusForbidden {
		t.Errorf("agent upload = %d", code)
	}
	if code, _ := upload(fx.device, "a.txt", "text/plain", []byte("hi")); code != http.StatusUnsupportedMediaType {
		t.Errorf("text upload = %d", code)
	}
	code, out := upload(fx.device, "IMG_1.png", "image/png", png)
	if code != http.StatusCreated || out["markdown"] != "![IMG_1.png](/api/v1/attachments/"+strconv.Itoa(int(out["id"].(float64)))+")" {
		t.Fatalf("upload = %d %v", code, out)
	}
	// Referenced from a body, the image is served to the key.
	id := int64(out["id"].(float64))
	if code, _ := fx.call(fx.device, fx.h.APIJournalUpdate, http.MethodPut, fx.day.ID, `{"body": "see ![](/api/v1/attachments/`+strconv.FormatInt(id, 10)+`)"}`); code != http.StatusOK {
		t.Fatalf("put with image = %d", code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/attachments/x", nil).WithContext(fx.device)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	fx.h.APIAttachmentServe(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("serve = %d (%d bytes)", rec.Code, rec.Body.Len())
	}
}
