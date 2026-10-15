package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Settings > AI: the user's own provider key ------------------------------

// aiTestTimeout bounds the live test call made before a key is stored. A
// reasoning model may think for a while even over a one-word answer.
const aiTestTimeout = 90 * time.Second

// aiSettingsGuard redirects with a helpful error when users may not bring
// their own key on this server. Returns false when the caller should stop.
func (h *Handlers) aiSettingsGuard(w http.ResponseWriter, r *http.Request) bool {
	if !h.aiUserKeys() {
		http.Redirect(w, r, "/settings?err="+errToQuery(errors.New(
			"own AI keys are not enabled on this server (set secrets.encryption_key in config.yaml)"))+"#ai",
			http.StatusSeeOther)
		return false
	}
	return true
}

// aiProviderForm is the Settings > AI form. Key blank = keep the stored key,
// as long as the provider (and, for custom, the URL) is unchanged.
type aiProviderForm struct {
	Provider, Model, Key, BaseURL string
}

func parseAIProviderForm(r *http.Request) aiProviderForm {
	return aiProviderForm{
		Provider: strings.TrimSpace(r.PostFormValue("provider")),
		Model:    strings.TrimSpace(r.PostFormValue("model")),
		Key:      strings.TrimSpace(r.PostFormValue("api_key")),
		BaseURL:  strings.TrimSpace(r.PostFormValue("base_url")),
	}
}

// aiResolveForm validates the form for the calling user and returns the preset
// and the key to use: the typed one, else the stored one for the same provider
// and URL. A stored key is never sent to a different endpoint than it was
// saved for. needModel is false for listing models.
func (h *Handlers) aiResolveForm(ctx context.Context, f aiProviderForm, needModel bool) (ai.Preset, string, error) {
	var p ai.Preset
	if f.Provider == ai.ProviderCustom {
		if u := auth.UserFrom(ctx); u == nil || !u.IsAdmin {
			return p, "", errors.New("custom endpoints are for admins")
		}
		if err := validAIBaseURL(f.BaseURL); err != nil {
			return p, "", err
		}
		p = ai.CustomPreset(f.BaseURL)
	} else {
		var ok bool
		if p, ok = ai.PresetByID(f.Provider); !ok {
			return p, "", errors.New("choose a provider")
		}
		f.BaseURL = ""
	}
	if needModel {
		if f.Model == "" {
			return p, "", errors.New("enter a model (Load models lists the ones your key can use)")
		}
		if !printableToken(f.Model, 200) {
			return p, "", errors.New("the model id looks wrong (no spaces, at most 200 characters)")
		}
	}
	key := f.Key
	if key != "" && !printableToken(key, 1000) {
		return p, "", errors.New("the API key looks wrong (no spaces, at most 1000 characters)")
	}
	if key == "" {
		row, err := h.Queries.GetUserAISettings(ctx, userID(ctx))
		switch {
		case err == nil && row.Provider == f.Provider && row.BaseUrl == f.BaseURL:
			if key, err = h.aiStoredKey(row); err != nil {
				return p, "", err
			}
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return p, "", fmt.Errorf("load AI settings: %w", err)
		}
	}
	if key == "" && f.Provider != ai.ProviderCustom {
		return p, "", errors.New("paste your API key")
	}
	return p, key, nil
}

// validAIBaseURL accepts an absolute http(s) URL with a host and no
// credentials (they belong in the key field).
func validAIBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("the base URL must be an http(s) URL such as http://host:8080/v1")
	}
	if u.User != nil {
		return errors.New("put credentials in the API key field, not the URL")
	}
	return nil
}

// printableToken reports whether s is 1..max printable characters without
// whitespace: model ids and API keys never contain any.
func printableToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// aiTestCall makes one tiny live completion: it proves the key, the model and
// the endpoint together, which listing models alone would not.
func aiTestCall(ctx context.Context, opts ai.Options) error {
	c, err := ai.New(opts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, aiTestTimeout)
	defer cancel()
	_, err = c.Generate(ctx, "You check that an API connection works.", "Reply with the single word OK.")
	return err
}

// aiKeyHint is what Settings shows of a stored key: its last 4 characters, or
// nothing for a key too short to spare them.
func aiKeyHint(key string) string {
	if len(key) < 12 {
		return ""
	}
	return key[len(key)-4:]
}

// SettingsAISave: POST /settings/ai - verify the provider, model and key with
// a live call, then store them (the key encrypted). Bad settings never enter
// the database.
func (h *Handlers) SettingsAISave(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadline(w) // the test call waits on the provider
	if !h.aiSettingsGuard(w, r) {
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := parseAIProviderForm(r)
	p, key, err := h.aiResolveForm(ctx, f, true)
	if err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(err)+"#ai", http.StatusSeeOther)
		return
	}
	if err := aiTestCall(ctx, p.Options(f.Model, key)); err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(fmt.Errorf("the test call to %s failed, nothing was saved: %w", p.Label, err))+"#ai", http.StatusSeeOther)
		return
	}
	var enc, nonce []byte
	if key != "" {
		if enc, nonce, err = h.AISecrets.Seal(key); err != nil {
			h.serverError(w, fmt.Errorf("encrypt ai key: %w", err))
			return
		}
	}
	baseURL := ""
	if f.Provider == ai.ProviderCustom {
		baseURL = f.BaseURL
	}
	if err := h.Queries.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{
		UserID: userID(ctx), Provider: f.Provider, Model: f.Model, BaseUrl: baseURL,
		ApiKeyEnc: enc, ApiKeyNonce: nonce, KeyHint: aiKeyHint(key),
	}); err != nil {
		h.serverError(w, fmt.Errorf("store ai settings: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=ai-saved#ai", http.StatusSeeOther)
}

// SettingsAITest: POST /settings/ai/test - re-run the live test call with the
// stored settings and refresh last_verified_at.
func (h *Handlers) SettingsAITest(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadline(w)
	if !h.aiSettingsGuard(w, r) {
		return
	}
	ctx := r.Context()
	row, err := h.Queries.GetUserAISettings(ctx, userID(ctx))
	if err != nil {
		http.Redirect(w, r, "/settings?err=no+AI+key+stored#ai", http.StatusSeeOther)
		return
	}
	opts, err := h.aiUserOptions(ctx, row)
	if err == nil {
		err = aiTestCall(ctx, opts)
	}
	if err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(fmt.Errorf("the test call failed: %w", err))+"#ai", http.StatusSeeOther)
		return
	}
	if err := h.Queries.TouchUserAISettingsVerified(ctx, userID(ctx)); err != nil {
		h.logger().Warn("touch ai verified", "err", err)
	}
	http.Redirect(w, r, "/settings?flash=ai-test-ok#ai", http.StatusSeeOther)
}

// SettingsAIDelete: POST /settings/ai/delete - forget the user's provider and
// key; reviews fall back to the server default, if there is one.
func (h *Handlers) SettingsAIDelete(w http.ResponseWriter, r *http.Request) {
	if !h.aiSettingsGuard(w, r) {
		return
	}
	ctx := r.Context()
	if err := h.Queries.DeleteUserAISettings(ctx, userID(ctx)); err != nil {
		h.serverError(w, fmt.Errorf("delete ai settings: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=ai-removed#ai", http.StatusSeeOther)
}

// SettingsAIChartSave: POST /settings/ai/chart - whether trade and group
// reviews send the trade's chart image to the model (vision models).
func (h *Handlers) SettingsAIChartSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var on int64
	if r.PostFormValue("send_chart") == "1" {
		on = 1
	}
	if err := h.Queries.SetUserAISendChart(ctx, models.SetUserAISendChartParams{AiSendChart: on, ID: userID(ctx)}); err != nil {
		h.serverError(w, fmt.Errorf("set ai send chart: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=ai-chart#ai", http.StatusSeeOther)
}

// SettingsAIAutoPlanSave: POST /settings/ai/autoplan - whether every new
// open position gets the AI's stop and target as its plan automatically.
func (h *Handlers) SettingsAIAutoPlanSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var on int64
	if r.PostFormValue("auto_plan") == "1" {
		on = 1
	}
	if err := h.Queries.SetUserAutoPlan(ctx, models.SetUserAutoPlanParams{AutoPlan: on, ID: userID(ctx)}); err != nil {
		h.serverError(w, fmt.Errorf("set auto plan: %w", err))
		return
	}
	if on == 1 {
		// What is open now gets its plan without waiting for the next fill.
		h.LiveData.Bump(userID(ctx))
	}
	http.Redirect(w, r, "/settings?flash=auto-plan#ai", http.StatusSeeOther)
}

// SettingsAIModels: POST /settings/ai/models (htmx) - list the models the
// form's provider and key can use, as the model field's suggestions. Errors
// render inline next to the field.
func (h *Handlers) SettingsAIModels(w http.ResponseWriter, r *http.Request) {
	if !h.aiUserKeys() {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := parseAIProviderForm(r)
	p, key, err := h.aiResolveForm(ctx, f, false)
	var ids []string
	if err == nil {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ids, err = ai.ListModels(lctx, p.Options("", key))
		cancel()
	}
	data := map[string]any{"Models": ids}
	if err != nil {
		data["Err"] = err.Error()
	}
	h.renderPartial(w, "_ai_models.html", "ai_models", data)
}

// aiSettingsView is the Settings > AI card's state.
type aiSettingsView struct {
	UserKeys    bool                  // users may bring their own key here
	Own         *models.UserAiSetting // the user's stored provider, nil = none
	OwnLabel    string                // Own's provider label
	ServerModel string                // the server default's model, "" = no default
	Presets     []ai.Preset
	IsAdmin     bool // may choose the custom provider
}

func (h *Handlers) aiSettings(ctx context.Context) aiSettingsView {
	v := aiSettingsView{UserKeys: h.aiUserKeys(), Presets: ai.Presets}
	if h.AI != nil {
		v.ServerModel = h.AIModel
	}
	if u := auth.UserFrom(ctx); u != nil {
		v.IsAdmin = u.IsAdmin
	}
	if v.UserKeys {
		if row, err := h.Queries.GetUserAISettings(ctx, userID(ctx)); err == nil {
			v.Own = &row
			v.OwnLabel = row.Provider
			if p, ok := aiPresetFor(row.Provider, row.BaseUrl); ok {
				v.OwnLabel = p.Label
			}
		}
	}
	return v
}
