// library.go — /library: the per-user management UI for journal templates
// and trading strategies. Every signed-in user manages their own; builtin
// templates are read-only (clone-to-edit). Mirrors the admin_users.go
// list + inline-action + flash-redirect pattern, minus the admin gate.
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// templateScopes are the scope values a journal template may target. "any"
// means the template shows in every editor regardless of scope.
var templateScopes = map[string]bool{
	"day": true, "week": true, "trade": true, "group": true, "any": true,
}

// LibraryPage renders /library with a Templates and a Strategies tab.
// ?tab=strategies selects the strategies panel; anything else = templates.
func (h *Handlers) LibraryPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)

	builtins, err := h.Queries.ListBuiltinTemplates(ctx)
	if err != nil {
		h.serverError(w, fmt.Errorf("list builtin templates: %w", err))
		return
	}
	userTemplates, err := h.Queries.ListUserTemplates(ctx, &uid)
	if err != nil {
		h.serverError(w, fmt.Errorf("list user templates: %w", err))
		return
	}
	strategies, err := h.Queries.ListStrategies(ctx, uid)
	if err != nil {
		h.serverError(w, fmt.Errorf("list strategies: %w", err))
		return
	}

	// Strategy playbook — a single per-user markdown doc. May not exist yet.
	var playbookBody string
	if pb, err := h.Queries.GetStrategyPlaybook(ctx, uid); err == nil {
		playbookBody = pb.Body
	} else if !errors.Is(err, sql.ErrNoRows) {
		h.serverError(w, fmt.Errorf("get playbook: %w", err))
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab != "strategies" {
		tab = "templates"
	}
	h.renderPage(w, r, "library.html", map[string]any{
		"NavActive":        "library",
		"BackHref":         validBackHref(r),
		"Tab":              tab,
		"BuiltinTemplates": builtins,
		"UserTemplates":    userTemplates,
		"Strategies":       strategies,
		"ScopeOptions":     []string{"any", "day", "week", "trade", "group"},
		"PlaybookBody":     playbookBody,
		"PlaybookRendered": template.HTML(renderMarkdown(playbookBody)),
		"Flash":            r.URL.Query().Get("flash"),
		"FlashErr":         r.URL.Query().Get("err"),
	})
}

// PlaybookSave handles POST /library/playbook — upserts the user's single
// strategy playbook document.
func (h *Handlers) PlaybookSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if err := h.Queries.UpsertStrategyPlaybook(ctx, models.UpsertStrategyPlaybookParams{
		UserID: userID(ctx),
		Body:   body,
	}); err != nil {
		h.serverError(w, fmt.Errorf("save playbook: %w", err))
		return
	}
	redirectLibrary(w, r, "strategies", "saved", "")
}

// ---- Templates ----

func (h *Handlers) TemplateCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	scope := strings.TrimSpace(r.PostFormValue("scope"))
	body := r.PostFormValue("body")
	if name == "" {
		redirectLibrary(w, r, "templates", "", "template name is required")
		return
	}
	if !templateScopes[scope] {
		redirectLibrary(w, r, "templates", "", "invalid scope")
		return
	}
	uid := userID(ctx)
	if _, err := h.Queries.CreateTemplate(ctx, models.CreateTemplateParams{
		UserID: &uid,
		Name:   name,
		Scope:  scope,
		Body:   body,
	}); err != nil {
		redirectLibrary(w, r, "templates", "", "could not create template: "+err.Error())
		return
	}
	redirectLibrary(w, r, "templates", "saved", "")
}

func (h *Handlers) TemplateUpdate(w http.ResponseWriter, r *http.Request) {
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
	name := strings.TrimSpace(r.PostFormValue("name"))
	scope := strings.TrimSpace(r.PostFormValue("scope"))
	body := r.PostFormValue("body")
	if name == "" || !templateScopes[scope] {
		redirectLibrary(w, r, "templates", "", "name and a valid scope are required")
		return
	}
	uid := userID(ctx)
	// builtin = 0 guard lives in the query; this only touches the user's own.
	if err := h.Queries.UpdateUserTemplate(ctx, models.UpdateUserTemplateParams{
		Name:   name,
		Scope:  scope,
		Body:   body,
		ID:     id,
		UserID: &uid,
	}); err != nil {
		h.serverError(w, fmt.Errorf("update template: %w", err))
		return
	}
	redirectLibrary(w, r, "templates", "saved", "")
}

// TemplateClone copies any visible template (builtin or own) into a new
// editable template owned by the current user, named "<name> (copy)".
func (h *Handlers) TemplateClone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	src, err := h.Queries.GetTemplate(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			redirectLibrary(w, r, "templates", "", "template not found")
			return
		}
		h.serverError(w, fmt.Errorf("get template: %w", err))
		return
	}
	// Only builtins or the user's own may be cloned (don't leak another
	// user's template via a guessed id).
	if src.Builtin == 0 && (src.UserID == nil || *src.UserID != userID(ctx)) {
		redirectLibrary(w, r, "templates", "", "template not found")
		return
	}
	uid := userID(ctx)
	if _, err := h.Queries.CreateTemplate(ctx, models.CreateTemplateParams{
		UserID: &uid,
		Name:   src.Name + " (copy)",
		Scope:  src.Scope,
		Body:   src.Body,
	}); err != nil {
		redirectLibrary(w, r, "templates", "", "could not clone: "+err.Error())
		return
	}
	redirectLibrary(w, r, "templates", "cloned", "")
}

func (h *Handlers) TemplateDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(ctx)
	if err := h.Queries.DeleteUserTemplate(ctx, models.DeleteUserTemplateParams{
		ID:     id,
		UserID: &uid,
	}); err != nil {
		h.serverError(w, fmt.Errorf("delete template: %w", err))
		return
	}
	redirectLibrary(w, r, "templates", "deleted", "")
}

// ---- Strategies ----

func (h *Handlers) StrategyCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	body := r.PostFormValue("body")
	if name == "" {
		redirectLibrary(w, r, "strategies", "", "strategy name is required")
		return
	}
	if _, err := h.Queries.CreateStrategy(ctx, models.CreateStrategyParams{
		UserID: userID(ctx),
		Name:   name,
		Body:   body,
	}); err != nil {
		// Most likely the UNIQUE(user_id, name) constraint.
		redirectLibrary(w, r, "strategies", "", "could not create strategy (name already used?): "+err.Error())
		return
	}
	redirectLibrary(w, r, "strategies", "saved", "")
}

func (h *Handlers) StrategyUpdate(w http.ResponseWriter, r *http.Request) {
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
	name := strings.TrimSpace(r.PostFormValue("name"))
	body := r.PostFormValue("body")
	if name == "" {
		redirectLibrary(w, r, "strategies", "", "strategy name is required")
		return
	}
	if err := h.Queries.UpdateStrategy(ctx, models.UpdateStrategyParams{
		Name:   name,
		Body:   body,
		ID:     id,
		UserID: userID(ctx),
	}); err != nil {
		redirectLibrary(w, r, "strategies", "", "could not update strategy: "+err.Error())
		return
	}
	redirectLibrary(w, r, "strategies", "saved", "")
}

func (h *Handlers) StrategyDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := h.Queries.DeleteStrategy(ctx, models.DeleteStrategyParams{
		ID:     id,
		UserID: userID(ctx),
	}); err != nil {
		h.serverError(w, fmt.Errorf("delete strategy: %w", err))
		return
	}
	redirectLibrary(w, r, "strategies", "deleted", "")
}

// strategyIDValue maps an entry's *int64 strategy_id to a plain int64
// (0 = none) for the picker's selected-option comparison in templates.
func strategyIDValue(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// parseStrategyID reads the strategy_id form field into a *int64 for
// Create/UpdateJournalEntryParams. Empty / "0" -> nil. A non-empty id is
// validated to belong to the current user; an unknown or foreign id is
// treated as "none" (nil) rather than erroring, so a stale option can't
// 500 a save.
func (h *Handlers) parseStrategyID(r *http.Request) *int64 {
	raw := strings.TrimSpace(r.PostFormValue("strategy_id"))
	if raw == "" || raw == "0" {
		return nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	if _, err := h.Queries.GetStrategy(r.Context(), models.GetStrategyParams{ID: id, UserID: userID(r.Context())}); err != nil {
		return nil
	}
	return &id
}

// loadAssignedStrategy returns the strategy attached to an entry, or nil when
// none is set or it can't be loaded. For the entry view's strategy card.
func (h *Handlers) loadAssignedStrategy(r *http.Request, strategyID *int64) *models.Strategy {
	if strategyID == nil {
		return nil
	}
	s, err := h.Queries.GetStrategy(r.Context(), models.GetStrategyParams{ID: *strategyID, UserID: userID(r.Context())})
	if err != nil {
		return nil
	}
	return &s
}

func redirectLibrary(w http.ResponseWriter, r *http.Request, tab, flash, errMsg string) {
	v := url.Values{}
	if tab != "" {
		v.Set("tab", tab)
	}
	if flash != "" {
		v.Set("flash", flash)
	}
	if errMsg != "" {
		v.Set("err", errMsg)
	}
	target := "/library"
	if enc := v.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
