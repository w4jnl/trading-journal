package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// SettingsPage: GET /settings - the user's API keys, AI provider and review
// language, account visibility and sync connections.
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
	// Account-visibility selector: active accounts only (retired accounts are
	// already hidden from stats, so listing them here would be misleading).
	accounts, err := h.Queries.ListAccounts(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	visAccounts := make([]models.Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Active == 1 {
			visAccounts = append(visAccounts, a)
		}
	}
	visGroups := groupAccountsForVisibility(visAccounts)

	// TopstepX API sync card: stored credentials (username only - the key is
	// never decrypted for display) + the latest sync result per account.
	var tsxCreds *models.TopstepxCredential
	var tsxSyncs []topstepxSyncRow
	if h.TopstepxSync != nil {
		if c, err := h.Queries.GetTopstepxCredentials(ctx, userID(ctx)); err == nil {
			tsxCreds = &c
			uid := userID(ctx)
			if rows, err := h.Queries.ListLastAPISyncPerAccount(ctx, models.ListLastAPISyncPerAccountParams{
				UserID: &uid, SyncType: "topstepx_api",
			}); err == nil {
				nameByID := map[int64]string{}
				for _, a := range accounts {
					nameByID[a.ID] = a.Name
				}
				for _, row := range rows {
					name := "(user-level)"
					if row.AccountID != nil {
						name = nameByID[*row.AccountID]
					}
					msg := ""
					if row.Message != nil {
						msg = *row.Message
					}
					tsxSyncs = append(tsxSyncs, topstepxSyncRow{
						AccountName: name, Status: row.Status, Message: msg, SyncedAt: row.SyncedAt,
					})
				}
			}
		}
	}

	// IBKR live sync card: the user's gateways + runtime state + last syncs.
	var ibkrGateways []ibkrGatewayRow
	var ibkrSyncs []topstepxSyncRow // same row shape as the TopstepX table
	if h.IbkrSync != nil {
		gws, err := h.Queries.ListIbkrGatewaysForUser(ctx, userID(ctx))
		if err == nil {
			states := h.IbkrSync.GatewayStates(userID(ctx))
			for _, gw := range gws {
				row := ibkrGatewayRow{Gateway: gw, State: "unknown"}
				if st, ok := states[gw.ID]; ok {
					row.State, row.LastPoll, row.LastErr, row.Accounts = st.State, st.LastPoll, st.LastErr, st.Accounts
				}
				if gw.Enabled == 0 {
					row.State = "disabled"
				}
				ibkrGateways = append(ibkrGateways, row)
			}
		}
		uid := userID(ctx)
		if rows, err := h.Queries.ListLastAPISyncPerAccount(ctx, models.ListLastAPISyncPerAccountParams{
			UserID: &uid, SyncType: "ibkr_api",
		}); err == nil {
			nameByID := map[int64]string{}
			for _, a := range accounts {
				nameByID[a.ID] = a.Name
			}
			for _, row := range rows {
				name := "(gateway)"
				if row.AccountID != nil {
					name = nameByID[*row.AccountID]
				}
				msg := ""
				if row.Message != nil {
					msg = *row.Message
				}
				ibkrSyncs = append(ibkrSyncs, topstepxSyncRow{
					AccountName: name, Status: row.Status, Message: msg, SyncedAt: row.SyncedAt,
				})
			}
		}
	}

	h.renderPage(w, r, "settings.html", map[string]any{
		"NavActive":       "settings",
		"BackHref":        validBackHref(r),
		"Keys":            keys,
		"OAuthEnabled":    h.OAuth != nil,
		"OAuthGrants":     h.oauthGrantRows(ctx),
		"NewKey":          newKey,
		"AILanguage":      h.userAILanguage(ctx),
		"AILanguages":     aiLanguageOptions,
		"AIEnabled":       h.AIJobs != nil,
		"AI":              h.aiSettings(ctx),
		"VisGroups":       visGroups,
		"VisTotal":        len(visAccounts),
		"TopstepxEnabled": h.TopstepxSync != nil,
		"TopstepxCreds":   tsxCreds,
		"TopstepxSyncs":   tsxSyncs,
		"IbkrEnabled":     h.IbkrSync != nil,
		"IbkrGateways":    ibkrGateways,
		"IbkrSyncs":       ibkrSyncs,
		"Flash":           r.URL.Query().Get("flash"),
		"Err":             r.URL.Query().Get("err"),
	})
}

// ibkrGatewayRow joins a stored gateway with its in-memory runtime state.
type ibkrGatewayRow struct {
	Gateway  models.IbkrGateway
	State    string // connected | needs-login | unreachable | unknown | disabled
	LastPoll time.Time
	LastErr  string
	Accounts []string
}

// topstepxSyncRow is one per-account "last sync" line on the settings card.
type topstepxSyncRow struct {
	AccountName string
	Status      string
	Message     string
	SyncedAt    time.Time
}

// accountVisGroup is one account-type section of the Settings visibility
// selector (e.g. "Prop · Eval"), holding the accounts of that type.
type accountVisGroup struct {
	Label    string
	Accounts []models.Account
}

// groupAccountsForVisibility buckets accounts by type in equityTypeOrder (real
// capital first, Eval last - the same hierarchy the equity breakdown uses), so
// the visibility selector reads top-to-bottom "keep, keep, ... maybe hide the
// Eval money". Unknown/blank types fall into a trailing "Other" group. Input is
// assumed name-sorted (ListAccounts orders by name), so groups stay sorted too.
func groupAccountsForVisibility(accounts []models.Account) []accountVisGroup {
	known := map[string]bool{}
	byType := map[string][]models.Account{}
	for _, o := range equityTypeOrder {
		known[o.Type] = true
	}
	for _, a := range accounts {
		slug := ""
		if a.Type != nil {
			slug = *a.Type
		}
		byType[slug] = append(byType[slug], a)
	}
	var groups []accountVisGroup
	for _, o := range equityTypeOrder {
		if accts := byType[o.Type]; len(accts) > 0 {
			groups = append(groups, accountVisGroup{Label: o.Label, Accounts: accts})
		}
	}
	var other []models.Account
	for _, a := range accounts { // preserve name order for the catch-all
		slug := ""
		if a.Type != nil {
			slug = *a.Type
		}
		if !known[slug] {
			other = append(other, a)
		}
	}
	if len(other) > 0 {
		groups = append(groups, accountVisGroup{Label: "Other", Accounts: other})
	}
	return groups
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

// SettingsAccountVisibilitySave: POST /settings/account-visibility - choose
// which accounts are counted in the Dashboard/Calendar/Analytics default
// aggregates. The form submits one include=<id> per checked (visible) account;
// any active account NOT in that set is flagged excluded_from_stats. Scoped to
// the user via SetAccountExcluded's WHERE user_id = ?.
func (h *Handlers) SettingsAccountVisibilitySave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	included := map[int64]bool{}
	for _, v := range r.PostForm["include"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			included[id] = true
		}
	}
	accounts, err := h.Queries.ListAccounts(ctx, userID(ctx))
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	uid := userID(ctx)
	for _, a := range accounts {
		if a.Active != 1 {
			continue // retired accounts aren't in the selector; leave their flag alone
		}
		want := int64(1) // excluded
		if included[a.ID] {
			want = 0
		}
		if a.ExcludedFromStats == want {
			continue // unchanged; skip the write
		}
		if err := h.Queries.SetAccountExcluded(ctx, models.SetAccountExcludedParams{
			ExcludedFromStats: want, ID: a.ID, UserID: uid,
		}); err != nil {
			h.serverError(w, fmt.Errorf("set account excluded: %w", err))
			return
		}
	}
	http.Redirect(w, r, "/settings?flash=visibility", http.StatusSeeOther)
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
