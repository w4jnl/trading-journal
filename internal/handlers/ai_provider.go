package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- per-user AI provider resolution ----------------------------------------
//
// A user may run the in-app reviews on their own provider key (Settings > AI,
// user_ai_settings); everyone else uses the server-wide default (cfg.AI). Every
// AI call resolves its client here, per call, so a key saved or removed in
// Settings takes effect on the next review without a restart.

// errNoAIProvider: the user has no key of their own and the server no default.
var errNoAIProvider = errors.New("no AI provider: add your API key in Settings > AI")

// aiUserKeys reports whether users may bring their own provider key.
func (h *Handlers) aiUserKeys() bool { return h.AISecrets != nil }

// aiPossible reports whether this server can run AI for anyone: a default
// client, or per-user keys. main.go creates AIJobs exactly when it is true.
func (h *Handlers) aiPossible() bool { return h.AI != nil || h.aiUserKeys() }

// aiFor returns the client the calling user's AI work for task runs on, and
// the reference naming its model (shown with every review, stored with the
// gamma reads): their own provider when they set one, at the model and
// effort they mapped to the task's tier (else their main ones), else the
// server default, else errNoAIProvider.
func (h *Handlers) aiFor(ctx context.Context, task aiTask) (ai.Client, aiModelRef, error) {
	if h.aiUserKeys() {
		row, err := h.Queries.GetUserAISettings(ctx, userID(ctx))
		switch {
		case err == nil:
			tier := task.Tier()
			model, effort, err := h.aiTierChoice(ctx, row, tier)
			if err != nil {
				return nil, aiModelRef{}, err
			}
			opts, err := h.aiOptionsFor(ctx, row, model, effort)
			if err != nil {
				return nil, aiModelRef{}, err
			}
			c, err := ai.New(opts)
			if err != nil {
				return nil, aiModelRef{}, err
			}
			ref := aiModelRef{Model: model, Effort: effort, Task: task, Tier: tier}
			if row.Provider == ai.ProviderCustom {
				ref.Provider = customHost(row.BaseUrl)
			} else if p, ok := ai.PresetByID(row.Provider); ok {
				ref.Provider = p.Label
			}
			return c, ref, nil
		case !errors.Is(err, sql.ErrNoRows):
			return nil, aiModelRef{}, fmt.Errorf("load AI settings: %w", err)
		}
	}
	if h.AI != nil {
		return h.AI, aiModelRef{Model: h.AIModel, Effort: h.AIEffort, Task: task, Tier: task.Tier()}, nil
	}
	return nil, aiModelRef{}, errNoAIProvider
}

// aiTierChoice is the model and effort the user's tier runs at: the tier's
// row where it names one, else the main setting from row.
func (h *Handlers) aiTierChoice(ctx context.Context, row models.UserAiSetting, tier aiTier) (model, effort string, err error) {
	model, effort = row.Model, row.Effort
	t, err := h.Queries.GetUserAITier(ctx, models.GetUserAITierParams{UserID: row.UserID, Tier: string(tier)})
	switch {
	case err == nil:
		if t.Model != "" {
			model = t.Model
		}
		if t.Effort != "" {
			effort = t.Effort
		}
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", fmt.Errorf("load AI tier: %w", err)
	}
	return model, effort, nil
}

// aiUserOptions builds the client options for a stored provider row at its
// main model and effort, decrypting its key.
func (h *Handlers) aiUserOptions(ctx context.Context, row models.UserAiSetting) (ai.Options, error) {
	return h.aiOptionsFor(ctx, row, row.Model, row.Effort)
}

// aiOptionsFor builds the client options for a stored provider row at the
// given model and effort. A custom endpoint is admin-only, checked at use as
// well as at save, so a demoted admin's stored URL stops working.
func (h *Handlers) aiOptionsFor(ctx context.Context, row models.UserAiSetting, model, effort string) (ai.Options, error) {
	p, ok := aiPresetFor(row.Provider, row.BaseUrl)
	if !ok {
		return ai.Options{}, fmt.Errorf("unknown AI provider %q; choose one again in Settings > AI", row.Provider)
	}
	if row.Provider == ai.ProviderCustom {
		if u := auth.UserFrom(ctx); u == nil || !u.IsAdmin {
			return ai.Options{}, errors.New("custom AI endpoints are for admins; choose a hosted provider in Settings > AI")
		}
	}
	key, err := h.aiStoredKey(row)
	if err != nil {
		return ai.Options{}, err
	}
	return p.Options(model, key).WithEffort(effort), nil
}

// aiStoredKey decrypts a row's key ("" for a keyless custom endpoint).
func (h *Handlers) aiStoredKey(row models.UserAiSetting) (string, error) {
	if len(row.ApiKeyEnc) == 0 {
		return "", nil
	}
	key, err := h.AISecrets.Open(row.ApiKeyEnc, row.ApiKeyNonce)
	if err != nil {
		return "", errors.New("your stored AI key cannot be decrypted (the server's encryption key changed?); enter it again in Settings > AI")
	}
	return key, nil
}

// aiReviewError takes the failure of the last background review of entryID,
// to show it once. A live-refresh fragment leaves it for the full render.
func (h *Handlers) aiReviewError(r *http.Request, entryID int64) string {
	if entryID == 0 || r.URL.Query().Get("fragment") == "1" {
		return ""
	}
	return h.AIReviews.TakeError(userID(r.Context()), entryID)
}

// aiPresetFor maps a stored provider id (and, for custom, its URL) to a preset.
func aiPresetFor(provider, baseURL string) (ai.Preset, bool) {
	if provider == ai.ProviderCustom {
		return ai.CustomPreset(baseURL), baseURL != ""
	}
	return ai.PresetByID(provider)
}

// aiAvailable reports whether the calling user can run AI work at all, which
// decides whether the AI buttons show. It does not open the key: a broken key
// surfaces as an error on use, pointing back to Settings.
func (h *Handlers) aiAvailable(ctx context.Context) bool {
	if h.AI != nil {
		return true
	}
	if !h.aiUserKeys() {
		return false
	}
	n, err := h.rq().UserHasAISettings(ctx, userID(ctx))
	return err == nil && n > 0
}
