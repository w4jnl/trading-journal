package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// ImportPage renders the /import page: an upload form and the recent-imports
// list pulled from sync_log.
func (h *Handlers) ImportPage(w http.ResponseWriter, r *http.Request) {
	recent, err := h.Queries.ListSyncLogRecent(r.Context(), 20)
	if err != nil {
		h.serverError(w, fmt.Errorf("list sync_log: %w", err))
		return
	}
	h.renderPage(w, "import.html", map[string]any{
		"NavActive": "import",
		"BackHref":  validBackHref(r),
		"Recent":    recent,
	})
}

// ImportUpload handles a multipart upload of a CSV. The file is staged into
// a temporary directory (NOT the watched inbox, to avoid a race with the
// watcher) and then handed to ImportService.ImportFile. The service moves
// the file to processed/ or failed/ exactly as it does for watcher inputs.
//
// Response is the import-outcome partial — meant to be HTMX-swapped into
// the page's #outcome target.
func (h *Handlers) ImportUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil { // 32 MiB cap, plenty for CSV
		http.Error(w, "bad multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	staged, cleanup, err := stageUpload(file, header.Filename, h.UploadsDir)
	if err != nil {
		h.serverError(w, fmt.Errorf("stage upload: %w", err))
		return
	}
	defer cleanup()

	// Use a fresh context decoupled from the request so a client disconnect
	// after we've already moved the file doesn't leave state half-written.
	ctx := context.WithoutCancel(r.Context())
	outcome, err := h.ImportSvc.ImportFile(ctx, staged)
	if err != nil {
		// Hard error from ImportService (couldn't even fall back to move-to-failed).
		h.serverError(w, fmt.Errorf("import: %w", err))
		return
	}

	h.renderPartial(w, "_import_outcome.html", "import_outcome", outcome)
}

// stageUpload copies the multipart upload into a fresh temp dir under
// uploadsRoot — kept on the same filesystem as the processed/failed dirs so
// the eventual os.Rename in ImportService doesn't fail with EXDEV (macOS in
// particular puts os.MkdirTemp("", …) on /private/var which is a different
// mount than project directories under /Volumes).
func stageUpload(src io.Reader, originalName, uploadsRoot string) (string, func(), error) {
	if uploadsRoot == "" {
		return "", func() {}, fmt.Errorf("uploads root not configured")
	}
	if err := os.MkdirAll(uploadsRoot, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("ensure uploads root: %w", err)
	}
	dir, err := os.MkdirTemp(uploadsRoot, "upload-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	dest := filepath.Join(dir, sanitizeFilename(originalName))
	out, err := os.Create(dest)
	if err != nil {
		cleanup()
		return "", func() {}, err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := out.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return dest, cleanup, nil
}

// sanitizeFilename strips path separators from a user-supplied filename so the
// upload can't write anywhere outside the staging dir.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	if name == "." || name == "/" || name == "" {
		return "upload.csv"
	}
	return name
}
