// admin_users.go — /admin/users page + the inline action handlers.
// All routes here are mounted behind auth.RequireAdmin in main.go.
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// AdminUsersPage renders /admin/users — the list of users with inline
// actions. The list is the source of truth; every action returns here.
func (h *Handlers) AdminUsersPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	users, err := h.Queries.ListUsers(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list users: %w", err))
		return
	}
	flash := r.URL.Query().Get("flash")
	flashErr := r.URL.Query().Get("err")
	me := auth.MustUser(ctx)
	h.renderPage(w, r, "admin_users.html", map[string]any{
		"NavActive": "admin_users",
		"BackHref":  validBackHref(r),
		"Users":     users,
		"Me":        me,
		"Flash":     flash,
		"FlashErr":  flashErr,
	})
}

// AdminUsersCreate handles POST /admin/users — adds a new user with
// username + password + optional admin flag + optional email.
func (h *Handlers) AdminUsersCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	email := strings.TrimSpace(r.PostFormValue("email"))
	isAdmin := r.PostFormValue("is_admin") == "on" || r.PostFormValue("is_admin") == "1"

	if username == "" || password == "" {
		redirectAdminUsers(w, r, "", "username and password are required")
		return
	}
	if len(password) < 8 {
		redirectAdminUsers(w, r, "", "password must be at least 8 characters")
		return
	}
	if _, err := h.Queries.GetUserByUsername(ctx, username); err == nil {
		redirectAdminUsers(w, r, "", fmt.Sprintf("user %q already exists", username))
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.serverError(w, fmt.Errorf("lookup: %w", err))
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		h.serverError(w, fmt.Errorf("hash: %w", err))
		return
	}
	adminFlag := int64(0)
	if isAdmin {
		adminFlag = 1
	}
	var emailPtr *string
	if email != "" {
		emailPtr = &email
	}
	if _, err := h.Queries.CreateUser(ctx, models.CreateUserParams{
		Username:     username,
		Email:        emailPtr,
		PasswordHash: &hash,
		IsAdmin:      adminFlag,
		DisplayName:  &username,
	}); err != nil {
		h.serverError(w, fmt.Errorf("create user: %w", err))
		return
	}
	redirectAdminUsers(w, r, "created="+username, "")
}

// AdminUsersResetPassword handles POST /admin/users/{id}/reset.
func (h *Handlers) AdminUsersResetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	password := r.PostFormValue("password")
	if len(password) < 8 {
		redirectAdminUsers(w, r, "", "password must be at least 8 characters")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		h.serverError(w, fmt.Errorf("hash: %w", err))
		return
	}
	if err := h.Queries.SetUserPassword(ctx, models.SetUserPasswordParams{
		PasswordHash: &hash,
		ID:           id,
	}); err != nil {
		h.serverError(w, fmt.Errorf("set password: %w", err))
		return
	}
	_ = h.Queries.DeleteSessionsForUser(ctx, id)
	redirectAdminUsers(w, r, "reset", "")
}

// AdminUsersToggleAdmin handles POST /admin/users/{id}/admin.
func (h *Handlers) AdminUsersToggleAdmin(w http.ResponseWriter, r *http.Request) {
	h.toggleUserField(w, r, "admin")
}

// AdminUsersToggleActive handles POST /admin/users/{id}/active.
func (h *Handlers) AdminUsersToggleActive(w http.ResponseWriter, r *http.Request) {
	h.toggleUserField(w, r, "active")
}

func (h *Handlers) toggleUserField(w http.ResponseWriter, r *http.Request, field string) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	target := int64(0)
	if v := r.PostFormValue("value"); v == "1" || v == "on" {
		target = 1
	}

	// Safety: don't demote / deactivate the last active admin.
	if target == 0 {
		row, err := h.Queries.GetUserByID(ctx, id)
		if err != nil {
			h.serverError(w, fmt.Errorf("lookup: %w", err))
			return
		}
		if row.IsAdmin != 0 && row.IsActive != 0 {
			n, err := h.Queries.CountAdmins(ctx)
			if err != nil {
				h.serverError(w, fmt.Errorf("count admins: %w", err))
				return
			}
			if n <= 1 {
				action := "demote"
				if field == "active" {
					action = "deactivate"
				}
				redirectAdminUsers(w, r, "",
					fmt.Sprintf("cannot %s the last active admin; promote another user first", action))
				return
			}
		}
	}

	switch field {
	case "admin":
		err = h.Queries.SetUserAdmin(ctx, models.SetUserAdminParams{IsAdmin: target, ID: id})
	case "active":
		err = h.Queries.SetUserActive(ctx, models.SetUserActiveParams{IsActive: target, ID: id})
		if err == nil && target == 0 {
			_ = h.Queries.DeleteSessionsForUser(ctx, id)
		}
	}
	if err != nil {
		h.serverError(w, fmt.Errorf("toggle %s: %w", field, err))
		return
	}
	redirectAdminUsers(w, r, "toggled", "")
}

// AdminUsersDelete handles POST /admin/users/{id}/delete.
func (h *Handlers) AdminUsersDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	row, err := h.Queries.GetUserByID(ctx, id)
	if err != nil {
		h.serverError(w, fmt.Errorf("lookup: %w", err))
		return
	}
	me := auth.MustUser(ctx)
	if row.ID == me.ID {
		redirectAdminUsers(w, r, "", "you cannot delete the account you are currently signed in as")
		return
	}
	if row.IsAdmin != 0 && row.IsActive != 0 {
		n, err := h.Queries.CountAdmins(ctx)
		if err != nil {
			h.serverError(w, fmt.Errorf("count admins: %w", err))
			return
		}
		if n <= 1 {
			redirectAdminUsers(w, r, "", "cannot delete the last active admin; promote another user first")
			return
		}
	}

	opts := auth.PurgeUserOptions{
		Username:       row.Username,
		AttachmentsDir: h.AttachmentsDir,
		InboxDir:       h.siblingImportsDir("inbox"),
		ProcessedDir:   h.siblingImportsDir("processed"),
		FailedDir:      h.siblingImportsDir("failed"),
		UploadsDir:     h.UploadsDir,
	}
	stats, err := auth.PurgeUser(ctx, h.DB, row.ID, opts)
	if err != nil {
		h.serverError(w, fmt.Errorf("purge: %w", err))
		return
	}
	redirectAdminUsers(w, r,
		fmt.Sprintf("deleted=%s&deleted_accounts=%d&deleted_trades=%d",
			row.Username, stats.Accounts, stats.Trades), "")
}

// siblingImportsDir derives <importsRoot>/<leaf> by treating h.UploadsDir
// as "<importsRoot>/uploads" (the convention main.go enforces). Handlers
// don't currently hold the imports root directly, so this avoids a
// constructor change.
func (h *Handlers) siblingImportsDir(leaf string) string {
	if h.UploadsDir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.UploadsDir), leaf)
}

func redirectAdminUsers(w http.ResponseWriter, r *http.Request, flash, errMsg string) {
	v := url.Values{}
	if flash != "" {
		// flash may already be "key=value"; preserve the raw form so the
		// template can read both halves.
		// Pass through to keep URL stable.
		v.Set("flash", flash)
	}
	if errMsg != "" {
		v.Set("err", errMsg)
	}
	target := "/admin/users"
	if enc := v.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
