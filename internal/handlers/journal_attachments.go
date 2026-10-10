package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// Maximum size for a single attachment upload. Chart screenshots from a 4K
// browser are typically 1–3 MB; this leaves headroom for the occasional full
// page screen capture without letting a typo'd paste blow up disk.
const maxAttachmentBytes = 8 * 1024 * 1024 // 8 MiB

// allowedAttachmentMIMEs guards the upload endpoint: a journal attachment is
// an image, nothing else. Keeping the set narrow means the GET path can
// safely set Content-Type from the stored value without sniffing.
var allowedAttachmentMIMEs = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// JournalAttachmentUpload handles POST /journal/attachments — multipart upload
// of a single image. Returns JSON { id, url, name } so the editor JS can
// insert ![](url) at the cursor immediately. entry_id is left at 0 until the
// entry is saved (rebound later by JournalSave on first write).
func (h *Handlers) JournalAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Cap the request body so a hostile (or buggy) client can't OOM the
	// server with a streaming multipart payload.
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+512)
	if err := r.ParseMultipartForm(maxAttachmentBytes + 512); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("upload too large or malformed: %v", err))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing file field")
		return
	}
	defer file.Close()

	mt := strings.ToLower(strings.TrimSpace(header.Header.Get("Content-Type")))
	// Strip parameters (e.g., "image/jpeg; charset=binary").
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	ext, ok := allowedAttachmentMIMEs[mt]
	if !ok {
		// Fall back to filename extension — some clients (clipboard paste)
		// don't set Content-Type reliably. Trust extension only if it maps
		// to a known type.
		fallback := strings.ToLower(filepath.Ext(header.Filename))
		for canonical, canonExt := range allowedAttachmentMIMEs {
			if canonExt == fallback {
				mt = canonical
				ext = canonExt
				ok = true
				break
			}
		}
	}
	if !ok {
		writeJSONError(w, http.StatusUnsupportedMediaType, "only PNG, JPEG, GIF, and WebP images are allowed")
		return
	}

	if err := os.MkdirAll(h.AttachmentsDir, 0o755); err != nil {
		h.serverError(w, fmt.Errorf("mkdir attachments: %w", err))
		return
	}

	// 16 random bytes → 32 hex chars. Avoids collisions and keeps user-
	// supplied filenames out of the on-disk path (no traversal surface).
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		h.serverError(w, fmt.Errorf("rand: %w", err))
		return
	}
	name := hex.EncodeToString(rnd[:]) + ext
	relPath := name
	absPath := filepath.Join(h.AttachmentsDir, name)

	out, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		h.serverError(w, fmt.Errorf("create attachment: %w", err))
		return
	}
	written, err := io.Copy(out, file)
	cerr := out.Close()
	if err != nil {
		_ = os.Remove(absPath)
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("write: %v", err))
		return
	}
	if cerr != nil {
		_ = os.Remove(absPath)
		h.serverError(w, fmt.Errorf("close attachment: %w", cerr))
		return
	}

	scope := strings.TrimSpace(r.FormValue("scope"))
	entryIDStr := strings.TrimSpace(r.FormValue("entry_id"))
	uid := userID(ctx)
	var entryID int64
	if entryIDStr != "" {
		if v, err := strconv.ParseInt(entryIDStr, 10, 64); err == nil && v > 0 {
			// Client-supplied: keep it only when the entry is the caller's.
			if _, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: v, UserID: userIDPtr(ctx)}); err == nil {
				entryID = v
			}
		}
	}

	res, err := h.DB.ExecContext(ctx, `INSERT INTO journal_attachments (user_id, entry_id, entry_scope, file_path, mime, byte_size) VALUES (?, ?, ?, ?, ?, ?)`,
		uid, entryID, scope, relPath, mt, written)
	if err != nil {
		_ = os.Remove(absPath)
		h.serverError(w, fmt.Errorf("insert attachment: %w", err))
		return
	}
	id, _ := res.LastInsertId()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":   id,
		"url":  fmt.Sprintf("/journal/attachments/%d", id),
		"name": header.Filename,
		"size": written,
	})
}

// JournalAttachmentServe handles GET /journal/attachments/{id} — streams the
// stored image with the recorded Content-Type. Cache-control is generous
// because attachments are immutable once uploaded.
func (h *Handlers) JournalAttachmentServe(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	h.serveAttachmentBytes(w, r, id, userID(r.Context()))
}

// serveAttachmentBytes streams a journal-attachment image with user scoping
// and a path-traversal guard. Shared by the session route and the API route.
// Ownership is the row's user_id (stamped at upload; backfilled for older
// rows from the body that references the image): an attachment is served to
// its uploader only, never by guessing a sequential id. entry_id is
// informational - the save handlers never fill it in, and bodies are the
// source of truth for which images an entry uses.
func (h *Handlers) serveAttachmentBytes(w http.ResponseWriter, r *http.Request, id, uid int64) {
	var (
		filePath string
		mt       string
	)
	row := h.DB.QueryRowContext(r.Context(),
		`SELECT a.file_path, a.mime FROM journal_attachments a WHERE a.id = ? AND a.user_id = ?`,
		id, uid)
	if err := row.Scan(&filePath, &mt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		h.serverError(w, fmt.Errorf("load attachment: %w", err))
		return
	}

	// Defensive: even though file_path is generated server-side, refuse to
	// serve anything that climbs out of AttachmentsDir.
	abs := filepath.Join(h.AttachmentsDir, filePath)
	cleaned, err := filepath.Abs(abs)
	if err != nil {
		h.serverError(w, fmt.Errorf("abs: %w", err))
		return
	}
	rootAbs, err := filepath.Abs(h.AttachmentsDir)
	if err != nil {
		h.serverError(w, fmt.Errorf("abs root: %w", err))
		return
	}
	if !strings.HasPrefix(cleaned, rootAbs+string(filepath.Separator)) && cleaned != rootAbs {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(cleaned)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if mt == "" {
		// Fall back to mime package detection from extension.
		mt = mime.TypeByExtension(filepath.Ext(cleaned))
		if mt == "" {
			mt = "application/octet-stream"
		}
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if info, err := f.Stat(); err == nil {
		http.ServeContent(w, r, cleaned, info.ModTime(), f)
	} else {
		_, _ = io.Copy(w, f)
	}
}

// JournalRecentList handles GET /journal/recent — returns recent journal
// entries of the given scope as JSON for the editor's "Recall" dropdown.
// Lets the user reference what they wrote on prior days/weeks/trades
// without leaving the current editor.
//
//	?scope=day|week|trade|group  (required)
//	?exclude_id=<int64>          (optional — skip the entry we're editing)
//	?limit=<int>                 (default 10, max 30)
func (h *Handlers) JournalRecentList(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	switch scope {
	case "day", "week", "trade", "group":
	default:
		writeJSONError(w, http.StatusBadRequest, "scope must be one of: day, week, trade, group")
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 30 {
			limit = n
		}
	}
	excludeID := int64(0)
	if v := r.URL.Query().Get("exclude_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			excludeID = n
		}
	}
	rows, err := h.Queries.ListRecentJournalEntriesForEditor(r.Context(), models.ListRecentJournalEntriesForEditorParams{
		Scope:  scope,
		ID:     excludeID,
		UserID: userIDPtr(r.Context()),
		Limit:  int64(limit),
	})
	if err != nil {
		h.serverError(w, fmt.Errorf("list recent: %w", err))
		return
	}

	type recent struct {
		ID        int64  `json:"id"`
		EntryDate string `json:"entry_date"` // YYYY-MM-DD in trading TZ
		DateLabel string `json:"date_label"` // human-friendly
		Snippet   string `json:"snippet"`    // first ~140 chars
		Body      string `json:"body"`       // full body for preview
		Tag       string `json:"tag,omitempty"`
	}
	out := make([]recent, 0, len(rows))
	tz := h.localTZ()
	for _, row := range rows {
		d := row.EntryDate.In(tz)
		snip := previewText(row.Body, 140) // machine blocks + markdown stripped
		entry := recent{
			ID:        row.ID,
			EntryDate: d.Format("2006-01-02"),
			DateLabel: d.Format("Mon, 2 Jan 2006"),
			Snippet:   snip,
			Body:      row.Body,
		}
		if row.Tag != nil {
			entry.Tag = *row.Tag
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// JournalTemplatesList handles GET /journal/templates — returns the
// available templates as JSON. The editor JS fetches this once when the
// Templates dropdown is first opened. Scope filter is optional.
func (h *Handlers) JournalTemplatesList(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	// Templates are global (builtin=1, user_id NULL) plus per-user customs
	// (builtin=0, user_id = me). Build the WHERE around that ownership rule
	// first; an optional scope filter narrows the list further. Kept as raw SQL
	// (not sqlc) because the scope clause is conditionally appended - sqlc's
	// generated queries have a fixed parameter list and can't express an
	// optional WHERE.
	q := `SELECT id, name, scope, body, builtin FROM journal_templates
	      WHERE (builtin = 1 OR user_id = ?)`
	args := []any{userID(r.Context())}
	if scope != "" {
		q += ` AND (scope = ? OR scope = 'any')`
		args = append(args, scope)
	}
	q += ` ORDER BY builtin DESC, name`
	rows, err := h.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		h.serverError(w, fmt.Errorf("list templates: %w", err))
		return
	}
	defer rows.Close()

	type tpl struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Scope   string `json:"scope"`
		Body    string `json:"body"`
		Builtin bool   `json:"builtin"`
	}
	var out []tpl
	for rows.Next() {
		var t tpl
		var b int
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.Body, &b); err != nil {
			h.serverError(w, fmt.Errorf("scan template: %w", err))
			return
		}
		t.Builtin = b != 0
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
