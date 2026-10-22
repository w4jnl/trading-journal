package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// A strategy written from outside the app: the REST endpoints
// POST /api/v1/strategies and PUT /api/v1/strategies/{id} and the MCP tools
// create_strategy and update_strategy share this one write path. A write
// may carry images inline (base64): each is stored as an attachment of the
// caller and lands in the playbook as ![name](/journal/attachments/N),
// either where the body refers to it by name, ![caption](name), or at the
// end of the body when nothing does. Unlike the journal writes, update
// replaces what it is given (name, body, checklist): the library is the
// trader's reference material and these tools are driven by the trader in
// a chat, so the tool declares itself destructive and the client asks.

// maxStrategyImages bounds one write; the images themselves obey the
// attachment limit (maxAttachmentBytes, allowedAttachmentMIMEs).
const maxStrategyImages = 10

// strategyImage is one inline image: the name the body may refer to and
// the decoded bytes of a type the attachment store accepts.
type strategyImage struct {
	Name string
	Mime string
	Data []byte
}

// strategyInput is one write. A nil pointer leaves the field as it is (on
// create: the name is required, the body empty); a nil Checklist leaves the
// rules as they are, an empty one clears them.
type strategyInput struct {
	Name      *string
	Body      *string
	Append    *string
	Checklist []string
	Images    []strategyImage
}

// strategyWriteError is a refusal the caller can show: its status is the
// REST code, its message the text (also the MCP tool's error).
type strategyWriteError struct {
	Status int
	Msg    string
}

func (e strategyWriteError) Error() string { return e.Msg }

func strategyBad(msg string) error {
	return strategyWriteError{Status: http.StatusBadRequest, Msg: msg}
}

// apiStoredImage tells the caller where an inline image landed.
type apiStoredImage struct {
	Name string `json:"name"`
	ID   int64  `json:"id"`
	URL  string `json:"url"`
}

// apiStrategyWritten is the response of a write: the strategy as GET
// returns it plus the images this write stored.
type apiStrategyWritten struct {
	apiStrategy
	Images []apiStoredImage `json:"images"`
}

// createStrategy stores a new strategy with its images and checklist.
func (h *Handlers) createStrategy(ctx context.Context, uid int64, in strategyInput) (apiStrategyWritten, error) {
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if name == "" {
		return apiStrategyWritten{}, strategyBad("strategy name is required")
	}
	if err := h.strategyNameFree(ctx, uid, name, 0); err != nil {
		return apiStrategyWritten{}, err
	}
	body := ""
	if in.Body != nil {
		body = apiUnrewriteAttachmentRefs(*in.Body)
	}
	body = appendStrategyText(body, in.Append)
	stored, err := h.storeStrategyImages(ctx, uid, in.Images)
	if err != nil {
		return apiStrategyWritten{}, err
	}
	body = placeStrategyImages(body, stored)
	s, err := h.Queries.CreateStrategy(ctx, models.CreateStrategyParams{UserID: uid, Name: name, Body: body})
	if err != nil {
		return apiStrategyWritten{}, strategyDBError(name, err)
	}
	if in.Checklist != nil {
		if err := h.journalSvc().SetChecklistItems(ctx, uid, s.ID, cleanChecklist(in.Checklist)); err != nil {
			return apiStrategyWritten{}, fmt.Errorf("strategy checklist: %w", err)
		}
	}
	h.strategyChanged(uid)
	return apiStrategyWritten{apiStrategy: h.apiStrategyObj(ctx, s), Images: stored}, nil
}

// updateStrategy replaces what the input carries on an existing strategy
// of the caller: the name, the body (or text appended to it), the checklist;
// images are stored and placed as on create. Nothing else changes.
func (h *Handlers) updateStrategy(ctx context.Context, uid, id int64, in strategyInput) (apiStrategyWritten, error) {
	s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: uid})
	if err != nil {
		return apiStrategyWritten{}, journal.ErrNotFound
	}
	name := s.Name
	if in.Name != nil {
		if name = strings.TrimSpace(*in.Name); name == "" {
			return apiStrategyWritten{}, strategyBad("strategy name is required")
		}
		if name != s.Name {
			if err := h.strategyNameFree(ctx, uid, name, id); err != nil {
				return apiStrategyWritten{}, err
			}
		}
	}
	body := s.Body
	if in.Body != nil {
		body = apiUnrewriteAttachmentRefs(*in.Body)
	}
	body = appendStrategyText(body, in.Append)
	stored, err := h.storeStrategyImages(ctx, uid, in.Images)
	if err != nil {
		return apiStrategyWritten{}, err
	}
	body = placeStrategyImages(body, stored)
	if err := h.Queries.UpdateStrategy(ctx, models.UpdateStrategyParams{Name: name, Body: body, ID: id, UserID: uid}); err != nil {
		return apiStrategyWritten{}, strategyDBError(name, err)
	}
	if in.Checklist != nil {
		if err := h.journalSvc().SetChecklistItems(ctx, uid, id, cleanChecklist(in.Checklist)); err != nil {
			return apiStrategyWritten{}, fmt.Errorf("strategy checklist: %w", err)
		}
	}
	h.strategyChanged(uid)
	s, err = h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: uid})
	if err != nil {
		return apiStrategyWritten{}, fmt.Errorf("reload strategy: %w", err)
	}
	return apiStrategyWritten{apiStrategy: h.apiStrategyObj(ctx, s), Images: stored}, nil
}

// strategyNameFree refuses a name another strategy of the user already
// carries (archived ones included: UNIQUE(user_id, name) covers them).
func (h *Handlers) strategyNameFree(ctx context.Context, uid int64, name string, self int64) error {
	rows, err := h.Queries.ListStrategies(ctx, uid)
	if err != nil {
		return fmt.Errorf("list strategies: %w", err)
	}
	for _, r := range rows {
		if r.ID != self && r.Name == name {
			return strategyWriteError{Status: http.StatusConflict, Msg: "a strategy named " + strconv.Quote(name) + " exists already"}
		}
	}
	return nil
}

// strategyDBError maps the insert/update failure: the UNIQUE clash is a
// conflict the caller can fix, anything else a server error.
func strategyDBError(name string, err error) error {
	if strings.Contains(err.Error(), "UNIQUE") {
		return strategyWriteError{Status: http.StatusConflict, Msg: "a strategy named " + strconv.Quote(name) + " exists already"}
	}
	return fmt.Errorf("save strategy: %w", err)
}

// strategyChanged tells open pages (the library, the pickers) after a write.
func (h *Handlers) strategyChanged(uid int64) {
	if h.LiveData != nil {
		h.LiveData.Bump(uid)
	}
}

// appendStrategyText adds a paragraph to the body.
func appendStrategyText(body string, add *string) string {
	if add == nil {
		return body
	}
	text := strings.TrimSpace(apiUnrewriteAttachmentRefs(*add))
	if text == "" {
		return body
	}
	if strings.TrimSpace(body) == "" {
		return text
	}
	return strings.TrimRight(body, "\n") + "\n\n" + text
}

// cleanChecklist trims the rules and drops blanks and repeats.
func cleanChecklist(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" && !seen[it] {
			seen[it] = true
			out = append(out, it)
		}
	}
	return out
}

// decodeStrategyImage turns one inline image (base64, a data: URL allowed)
// into bytes the attachment store accepts; the type is read from the bytes,
// never from the caller.
func decodeStrategyImage(n int, name, data string) (strategyImage, error) {
	data = strings.TrimSpace(data)
	if i := strings.Index(data, ","); i >= 0 && strings.HasPrefix(data, "data:") {
		data = data[i+1:]
	}
	data = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, data)
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(data); err == nil {
			break
		}
	}
	if err != nil {
		return strategyImage{}, strategyBad(fmt.Sprintf("image %d: not base64", n))
	}
	if len(raw) == 0 {
		return strategyImage{}, strategyBad(fmt.Sprintf("image %d: empty", n))
	}
	if len(raw) > maxAttachmentBytes {
		return strategyImage{}, strategyWriteError{Status: http.StatusRequestEntityTooLarge, Msg: fmt.Sprintf("image %d: larger than %d MiB", n, maxAttachmentBytes>>20)}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("image-%d", n)
	}
	mt := sniffAttachmentMIME(raw)
	if mt == "" {
		return strategyImage{}, strategyWriteError{Status: http.StatusUnsupportedMediaType, Msg: fmt.Sprintf("image %d (%s): not an image; only %s images are stored", n, name, attachmentTypesSentence)}
	}
	raw, err = checkAttachmentImage(mt, raw)
	if err != nil {
		return strategyImage{}, strategyBad(fmt.Sprintf("image %d (%s): %v", n, name, err))
	}
	return strategyImage{Name: name, Mime: mt, Data: raw}, nil
}

// storeStrategyImages stores the images as the user's attachments and
// returns where each landed.
func (h *Handlers) storeStrategyImages(ctx context.Context, uid int64, images []strategyImage) ([]apiStoredImage, error) {
	if len(images) > maxStrategyImages {
		return nil, strategyBad(fmt.Sprintf("at most %d images per write", maxStrategyImages))
	}
	out := make([]apiStoredImage, 0, len(images))
	for _, img := range images {
		id, err := h.storeAttachmentBytes(ctx, uid, 0, "strategy", img.Mime, img.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, apiStoredImage{Name: img.Name, ID: id, URL: "/api/v1/attachments/" + strconv.FormatInt(id, 10)})
	}
	return out, nil
}

// placeStrategyImages puts the stored images into the body: a reference by
// name, ![caption](name), becomes the attachment URL; an image the body
// does not name is appended as its own paragraph.
func placeStrategyImages(body string, stored []apiStoredImage) string {
	for _, img := range stored {
		url := "/journal/attachments/" + strconv.FormatInt(img.ID, 10)
		ref := "](" + img.Name + ")"
		if strings.Contains(body, ref) {
			body = strings.ReplaceAll(body, ref, "]("+url+")")
			continue
		}
		alt := strings.NewReplacer("[", "", "]", "", "\n", " ").Replace(img.Name)
		line := "![" + alt + "](" + url + ")"
		if strings.TrimSpace(body) == "" {
			body = line
		} else {
			body = strings.TrimRight(body, "\n") + "\n\n" + line
		}
	}
	return body
}

// ---- REST ----

// apiStrategyWrite is the JSON body of POST /api/v1/strategies and
// PUT /api/v1/strategies/{id}.
type apiStrategyWrite struct {
	Name      *string          `json:"name"`
	Body      *string          `json:"body"`
	Append    *string          `json:"append"`
	Checklist *[]string        `json:"checklist"`
	Images    []apiImageInline `json:"images"`
}

type apiImageInline struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

func (in apiStrategyWrite) input() (strategyInput, error) {
	out := strategyInput{Name: in.Name, Body: in.Body, Append: in.Append}
	if in.Checklist != nil {
		out.Checklist = *in.Checklist
		if out.Checklist == nil {
			out.Checklist = []string{}
		}
	}
	for i, img := range in.Images {
		d, err := decodeStrategyImage(i+1, img.Name, img.Data)
		if err != nil {
			return strategyInput{}, err
		}
		out.Images = append(out.Images, d)
	}
	return out, nil
}

// apiStrategyWriteBody reads the JSON; the cap leaves room for the images
// (base64 of up to ten attachments) and nothing more.
func apiStrategyWriteBody(w http.ResponseWriter, r *http.Request) (strategyInput, bool) {
	var in apiStrategyWrite
	limit := int64(maxStrategyImages)*(maxAttachmentBytes*4/3+1024) + 1<<20
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return strategyInput{}, false
	}
	out, err := in.input()
	if err != nil {
		h := strategyWriteError{}
		if errors.As(err, &h) {
			writeJSONError(w, h.Status, h.Msg)
		} else {
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return strategyInput{}, false
	}
	return out, true
}

// apiStrategyWriteError maps a write failure onto the API's codes.
func (h *Handlers) apiStrategyWriteError(w http.ResponseWriter, what string, err error) {
	var we strategyWriteError
	if errors.As(err, &we) {
		writeJSONError(w, we.Status, we.Msg)
		return
	}
	if errors.Is(err, journal.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	h.logger().Error(what, "err", err)
	writeJSONError(w, http.StatusInternalServerError, "could not save the strategy")
}

// APIStrategyCreate: POST /api/v1/strategies - a new strategy with its
// playbook, checklist and inline images.
func (h *Handlers) APIStrategyCreate(w http.ResponseWriter, r *http.Request) {
	in, ok := apiStrategyWriteBody(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out, err := h.createStrategy(ctx, userID(ctx), in)
	if err != nil {
		h.apiStrategyWriteError(w, "api strategy create", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// APIStrategyUpdate: PUT /api/v1/strategies/{id} - replace the fields the
// body carries (name, body or append, checklist) and add its images.
func (h *Handlers) APIStrategyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	in, ok := apiStrategyWriteBody(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out, err := h.updateStrategy(ctx, userID(ctx), id, in)
	if err != nil {
		h.apiStrategyWriteError(w, "api strategy update", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- MCP ----

// mcpStrategyInput reads the shared arguments of create_strategy and
// update_strategy.
func mcpStrategyInput(req mcpgo.CallToolRequest) (strategyInput, error) {
	args := req.GetArguments()
	in := strategyInput{Name: mcpOptString(req, "name"), Append: mcpOptString(req, "append")}
	if v, ok := args["body"].(string); ok {
		in.Body = &v
	}
	if _, ok := args["checklist"]; ok {
		in.Checklist = mcpStringSlice(req, "checklist")
		if in.Checklist == nil {
			in.Checklist = []string{}
		}
	}
	if raw, ok := args["images"].([]any); ok {
		for i, x := range raw {
			m, _ := x.(map[string]any)
			name, _ := m["name"].(string)
			data, _ := m["data"].(string)
			d, err := decodeStrategyImage(i+1, name, data)
			if err != nil {
				return strategyInput{}, err
			}
			in.Images = append(in.Images, d)
		}
	}
	return in, nil
}

func mcpStrategyResult(out apiStrategyWritten, err error) (*mcpgo.CallToolResult, error) {
	if err != nil {
		var we strategyWriteError
		switch {
		case errors.As(err, &we):
			return mcpgo.NewToolResultError(we.Msg), nil
		case errors.Is(err, journal.ErrNotFound):
			return mcpgo.NewToolResultError("strategy not found"), nil
		default:
			return mcpgo.NewToolResultError("could not save the strategy"), nil
		}
	}
	return mcpJSON(out)
}

func (h *Handlers) mcpCreateStrategy(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	in, err := mcpStrategyInput(req)
	if err != nil {
		return mcpStrategyResult(apiStrategyWritten{}, err)
	}
	return mcpStrategyResult(h.createStrategy(ctx, userID(ctx), in))
}

func (h *Handlers) mcpUpdateStrategy(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id, err := req.RequireInt("id")
	if err != nil {
		return mcpgo.NewToolResultError("id is required"), nil
	}
	in, err := mcpStrategyInput(req)
	if err != nil {
		return mcpStrategyResult(apiStrategyWritten{}, err)
	}
	return mcpStrategyResult(h.updateStrategy(ctx, userID(ctx), int64(id), in))
}
