package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ImportPage renders the /import page: an upload form and the recent-imports
// list pulled from sync_log.
func (h *Handlers) ImportPage(w http.ResponseWriter, r *http.Request) {
	recent, err := h.Queries.ListSyncLogRecent(r.Context(), models.ListSyncLogRecentParams{UserID: userIDPtr(r.Context()), Limit: 20})
	if err != nil {
		h.serverError(w, fmt.Errorf("list sync_log: %w", err))
		return
	}
	h.renderPage(w, r, "import.html", map[string]any{
		"NavActive": "import",
		"BackHref":  validBackHref(r),
		"Recent":    recent,
	})
}

// ImportUpload handles a multipart upload of a CSV. Stages the file under
// imports/uploads/ (same filesystem as processed/failed/ so the eventual
// rename doesn't cross device boundaries), detects the format, and either:
//   - imports immediately (normal path), or
//   - returns an "account picker" partial when the file is a TopstepX trades
//     CSV whose target account can't be inferred from the filename AND more
//     than one TopStep account already exists. The staged file stays in
//     place; the picker form re-posts to /import/assign with the chosen
//     account.
//
// Response is HTMX-swapped into the page's #outcome target.
func (h *Handlers) ImportUpload(w http.ResponseWriter, r *http.Request) {
	// Cap the request body. A CSV import is text; even a year of fills sits
	// well under 64 MiB. Without this an oversized upload would buffer to
	// disk via the form parser before the handler ever ran.
	const maxImportBytes = 64 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
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

	ctx := context.WithoutCancel(r.Context())

	// Peek at the staged file's format so we can decide whether to prompt.
	needsPicker, topstepAccounts, err := h.detectNeedsAccountPicker(ctx, staged)
	if err != nil {
		cleanup()
		h.serverError(w, fmt.Errorf("detect picker: %w", err))
		return
	}
	if needsPicker {
		// Don't cleanup — the picker form needs the staged path to survive.
		h.renderPartial(w, "_import_picker.html", "import_picker", map[string]any{
			"StagedPath":      staged,
			"OriginalName":    header.Filename,
			"TopstepAccounts": topstepAccounts,
			"FirmOptions":     []string{"", "apex", "lucid", "topstep", "amp", "ibkr"},
			"TypeOptions":     accountTypeOptions(),
		})
		return
	}

	defer cleanup()
	u := auth.MustUser(ctx)
	outcome, err := h.ImportSvc.ImportFileForUser(ctx, staged, u.ID, u.Username)
	if err != nil {
		h.serverError(w, fmt.Errorf("import: %w", err))
		return
	}
	h.renderPartial(w, "_import_outcome.html", "import_outcome", outcome)
}

// ImportAssign handles POST /import/assign — the second step of the upload
// flow when the user has picked an existing account from the picker.
// Honors a `new_*` field set instead of `account_id` to create-and-assign.
func (h *Handlers) ImportAssign(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	staged := r.FormValue("staged_path")
	if !h.stagedPathSafe(staged) {
		http.Error(w, "invalid staged_path", http.StatusBadRequest)
		return
	}

	var accountID int64
	if v := strings.TrimSpace(r.FormValue("account_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "bad account_id", http.StatusBadRequest)
			return
		}
		accountID = id
	} else if r.FormValue("new_name") != "" {
		// Create-then-assign branch.
		newID, err := h.createAccountFromForm(ctx, r)
		if err != nil {
			h.removeStagedDir(staged)
			h.serverError(w, fmt.Errorf("create account: %w", err))
			return
		}
		accountID = newID
	} else {
		http.Error(w, "must pick an account or create a new one", http.StatusBadRequest)
		return
	}

	u := auth.MustUser(ctx)
	outcome, err := h.ImportSvc.ImportFileToAccountForUser(ctx, staged, accountID, u.ID, u.Username)
	if err != nil {
		h.serverError(w, fmt.Errorf("import: %w", err))
		return
	}
	// Best-effort cleanup of the now-empty staged tempdir.
	h.removeStagedDir(staged)
	h.renderPartial(w, "_import_outcome.html", "import_outcome", outcome)
}

// detectNeedsAccountPicker reads the first line of the staged file and
// decides whether the UI needs to prompt the user for an account. Returns
// the list of existing TopStep-flavored accounts so the picker can render
// the dropdown without doing the query a second time.
//
// Fires for any TopstepX trades CSV whose target account can't be inferred
// from the filename. Even when there are zero or one existing TopStep
// accounts the picker still appears — with zero, only the "Create new"
// branch is offered; with one, the user can pick that account or create a
// new one — so we never silently route into a phantom TOPSTEP-DEFAULT
// bucket the way the importer's auto-create path used to.
func (h *Handlers) detectNeedsAccountPicker(ctx context.Context, stagedPath string) (bool, []models.Account, error) {
	// Read just enough to detect format — first KB is plenty (header row).
	f, err := os.Open(stagedPath)
	if err != nil {
		return false, nil, err
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	format := importer.DetectFormat(buf[:n])

	if format != importer.FormatTopstepXTrades {
		return false, nil, nil
	}
	if importer.ExtractTopstepAccountRef(stagedPath) != "" {
		return false, nil, nil
	}

	all, err := h.Queries.ListAccounts(ctx, auth.MustUser(ctx).ID)
	if err != nil {
		return false, nil, err
	}
	var topstep []models.Account
	for _, a := range all {
		if a.ImportSource == string(importer.FormatTopstepX) ||
			(a.Firm != nil && *a.Firm == "topstep") {
			topstep = append(topstep, a)
		}
	}
	return true, topstep, nil
}

// stagedPathSafe ensures a client-supplied staged path actually lives under
// the uploads root — defense against path traversal in the picker form.
func (h *Handlers) stagedPathSafe(p string) bool {
	if p == "" || h.UploadsDir == "" {
		return false
	}
	root, err := filepath.Abs(h.UploadsDir)
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}
	return strings.HasPrefix(abs, root+string(filepath.Separator))
}

// removeStagedDir cleans up the temp directory holding a staged upload.
// Best-effort: failure to remove is logged but doesn't fail the request.
func (h *Handlers) removeStagedDir(stagedFile string) {
	if stagedFile == "" {
		return
	}
	if err := os.RemoveAll(filepath.Dir(stagedFile)); err != nil {
		h.logger().Warn("cleanup staged dir", "path", stagedFile, "err", err)
	}
}

// createAccountFromForm runs the same create logic AccountsCreate uses but
// returns the new account ID directly so the upload flow can chain into the
// import without a round-trip through /accounts.
func (h *Handlers) createAccountFromForm(ctx context.Context, r *http.Request) (int64, error) {
	name := strings.TrimSpace(r.FormValue("new_name"))
	firm := strings.TrimSpace(r.FormValue("new_firm"))
	typ := strings.TrimSpace(r.FormValue("new_type"))
	currency := strings.TrimSpace(r.FormValue("new_currency"))
	ref := strings.TrimSpace(r.FormValue("new_account_ref"))
	if name == "" {
		return 0, fmt.Errorf("name is required")
	}
	if !validFirms[firm] {
		return 0, fmt.Errorf("invalid firm: %q", firm)
	}
	if !validTypes[typ] {
		return 0, fmt.Errorf("invalid type: %q", typ)
	}
	if currency == "" {
		currency = "USD"
	}
	if ref == "" {
		ref = name
	}
	// The picker only fires for TopstepX trades CSVs, so this account is
	// going to receive that import next. Force import_source to topstepx_csv
	// so the (import_source, account_ref) pair stays consistent across the
	// manual-create + immediate-import cycle.
	importSrc := string(importer.FormatTopstepX)
	needsSetup := int64(0)
	if firm == "" || typ == "" {
		needsSetup = 1
	}
	created, err := h.Queries.CreateAccount(ctx, models.CreateAccountParams{
		UserID:           auth.MustUser(ctx).ID,
		Name:             name,
		Firm:             strPtrOrNil(firm),
		ImportSource:     importSrc,
		AccountRef:       ref,
		Type:             strPtrOrNil(typ),
		Currency:         currency,
		NeedsSetup:       needsSetup,
		CommissionSource: "inline", // picker only fires for TopstepX trades CSV
	})
	if err != nil {
		return 0, err
	}
	return created.ID, nil
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

