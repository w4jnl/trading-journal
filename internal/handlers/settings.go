package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// aiLanguageOption is one choice in the Settings language picker. Value is what gets
// stored and fed to the AI prompt (an English language name, or "" for the model default);
// Label is what the user sees.
type aiLanguageOption struct{ Value, Label string }

// aiLanguageOptions is the curated set offered in Settings (and the server-side allow-list
// the save handler validates against, so a stored value can never be arbitrary text).
var aiLanguageOptions = []aiLanguageOption{
	{"", "English (default)"},
	{"Czech", "Czech — Čeština"},
	{"Slovak", "Slovak — Slovenčina"},
	{"Dutch", "Dutch — Nederlands"},
	{"German", "German — Deutsch"},
	{"French", "French — Français"},
	{"Spanish", "Spanish — Español"},
	{"Italian", "Italian — Italiano"},
	{"Polish", "Polish — Polski"},
	{"Portuguese", "Portuguese — Português"},
}

// allowedAILanguage reports whether v is one of the offered language values.
func allowedAILanguage(v string) bool {
	for _, o := range aiLanguageOptions {
		if o.Value == v {
			return true
		}
	}
	return false
}

// SettingsPage: GET /settings - the user's API keys + AI-review language.
func (h *Handlers) SettingsPage(w http.ResponseWriter, r *http.Request) {
	h.renderSettings(w, r, "")
}

// renderSettings renders the settings page. newKey, when non-empty, is a
// just-created plaintext key shown ONCE (passed in-memory, never via a redirect
// URL or log).
func (h *Handlers) renderSettings(w http.ResponseWriter, r *http.Request, newKey string) {
	ctx := r.Context()
	keys, err := h.Queries.ListAPIKeysForUser(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list api keys: %w", err))
		return
	}
	h.renderPage(w, r, "settings.html", map[string]any{
		"NavActive":   "settings",
		"BackHref":    validBackHref(r),
		"Keys":        keys,
		"NewKey":      newKey,
		"AILanguage":  h.userAILanguage(ctx),
		"AILanguages": aiLanguageOptions,
		"AIEnabled":   h.AIJobs != nil,
		"Flash":       r.URL.Query().Get("flash"),
		"Err":         r.URL.Query().Get("err"),
	})
}

// SettingsLanguageSave: POST /settings/language - set the language the in-app AI analyzer
// writes reviews in. Validated against the offered list; empty = the model default.
func (h *Handlers) SettingsLanguageSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	lang := strings.TrimSpace(r.PostFormValue("ai_language"))
	if !allowedAILanguage(lang) {
		http.Redirect(w, r, "/settings?err=unsupported+language", http.StatusSeeOther)
		return
	}
	if err := h.Queries.SetUserAILanguage(ctx, models.SetUserAILanguageParams{AiLanguage: lang, ID: userID(ctx)}); err != nil {
		h.serverError(w, fmt.Errorf("set ai language: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=language", http.StatusSeeOther)
}

// APIKeyCreate: POST /settings/api-keys - generate a key and show it once.
func (h *Handlers) APIKeyCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "API key"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	full, prefix, sha, err := auth.GenerateAPIKey()
	if err != nil {
		h.serverError(w, fmt.Errorf("generate api key: %w", err))
		return
	}
	if _, err := h.Queries.CreateAPIKey(ctx, models.CreateAPIKeyParams{
		UserID: userID(ctx), Name: name, KeyPrefix: prefix, KeySha256: sha,
	}); err != nil {
		h.serverError(w, fmt.Errorf("create api key: %w", err))
		return
	}
	// Render directly with the plaintext (shown once); no redirect so it never
	// lands in a URL or the access log.
	h.renderSettings(w, r, full)
}

// APIKeyRevoke: POST /settings/api-keys/{id}/revoke (soft-revoke).
func (h *Handlers) APIKeyRevoke(w http.ResponseWriter, r *http.Request) { h.apiKeyMutate(w, r, false) }

// APIKeyDelete: POST /settings/api-keys/{id}/delete (remove the row).
func (h *Handlers) APIKeyDelete(w http.ResponseWriter, r *http.Request) { h.apiKeyMutate(w, r, true) }

func (h *Handlers) apiKeyMutate(w http.ResponseWriter, r *http.Request, del bool) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(ctx)
	flash := "revoked"
	if del {
		flash = "deleted"
		err = h.Queries.DeleteAPIKey(ctx, models.DeleteAPIKeyParams{ID: id, UserID: uid})
	} else {
		err = h.Queries.RevokeAPIKey(ctx, models.RevokeAPIKeyParams{ID: id, UserID: uid})
	}
	if err != nil {
		http.Redirect(w, r, "/settings?err=could+not+update+key", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?flash="+flash, http.StatusSeeOther)
}
