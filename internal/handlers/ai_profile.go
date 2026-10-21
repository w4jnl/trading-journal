package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/w4jnl/trading-journal/internal/aimethod"
	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- The trader profile --------------------------------------------------
//
// The skills are code (internal/aimethod); the trader tunes how they apply
// through a profile stored as JSON on the user: style, the evidence they
// weigh most, their markets, standing instructions. It is one cached block
// in every review, sizes the evidence the app sends, and reaches every MCP
// client through server_info, the profile resource and the prompts.

// userAIProfile is the calling user's profile; the default when unset.
func (h *Handlers) userAIProfile(ctx context.Context) aimethod.Profile {
	if h.Queries == nil {
		return aimethod.ParseProfile("")
	}
	u, err := h.Queries.GetUserByID(ctx, userID(ctx))
	if err != nil {
		return aimethod.ParseProfile("")
	}
	return aimethod.ParseProfile(u.AiProfile)
}

// aiProfileLine is the profile as a system-prompt suffix for the JSON-answer
// helpers (the plan draft, the levels suggestion); "" for the default.
func aiProfileLine(p aimethod.Profile) string {
	if blk := p.Block(); blk != "" {
		return "\n\n" + blk
	}
	return ""
}

// aiLangKey carries a per-request language override (the lang query
// parameter on the review routes) to the review that runs in the background.
type aiLangKey struct{}

// withAILanguage returns ctx asking for reviews in lang, when lang is one of
// the offered languages; any other value leaves the profile's language.
func withAILanguage(ctx context.Context, lang string) context.Context {
	lang = strings.TrimSpace(lang)
	if lang == "" || !allowedAILanguage(lang) {
		return ctx
	}
	return context.WithValue(ctx, aiLangKey{}, lang)
}

// aiLanguageOverride is the language a request asked for, "" for none.
func aiLanguageOverride(ctx context.Context) string {
	s, _ := ctx.Value(aiLangKey{}).(string)
	return s
}

// aiCtx is the request's context with its lang parameter applied.
func aiCtx(r *http.Request) context.Context {
	return withAILanguage(r.Context(), r.URL.Query().Get("lang"))
}

// aiProfileView is the Settings > AI profile card's state.
type aiProfileView struct {
	Profile aimethod.Profile
	Styles  []aiProfileChoice
	Focus   []aiProfileChoice // ranked picks first, in their order, then the rest
	Markets []aiProfileChoice // the Levels markets, checked when in the profile
	Extra   string            // the profile's markets outside the Levels list
}

type aiProfileChoice struct {
	Key, Label, Hint string
	On               bool
	Rank             int // 1-based for a ranked focus, 0 otherwise
}

func (h *Handlers) aiProfileSettings(ctx context.Context) aiProfileView {
	p := h.userAIProfile(ctx)
	v := aiProfileView{Profile: p}
	for _, s := range aimethod.Styles {
		v.Styles = append(v.Styles, aiProfileChoice{Key: s.Key, Label: s.Label, Hint: s.Horizon, On: s.Key == p.Style})
	}
	rank := map[string]int{}
	for i, f := range p.Focus {
		rank[f] = i + 1
	}
	for _, f := range aimethod.FocusKeys {
		if r := rank[f.Key]; r > 0 {
			v.Focus = append(v.Focus, aiProfileChoice{Key: f.Key, Label: f.Label, Hint: f.Means, On: true, Rank: r})
		}
	}
	for i := 0; i < len(v.Focus); i++ { // ranked picks in their rank order
		for j := i + 1; j < len(v.Focus); j++ {
			if v.Focus[j].Rank < v.Focus[i].Rank {
				v.Focus[i], v.Focus[j] = v.Focus[j], v.Focus[i]
			}
		}
	}
	for _, f := range aimethod.FocusKeys {
		if rank[f.Key] == 0 {
			v.Focus = append(v.Focus, aiProfileChoice{Key: f.Key, Label: f.Label, Hint: f.Means})
		}
	}
	known := map[string]bool{}
	for _, m := range levelmap.Markets {
		known[m] = true
		on := false
		for _, pm := range p.Markets {
			if pm == m {
				on = true
			}
		}
		v.Markets = append(v.Markets, aiProfileChoice{Key: m, Label: m, On: on})
	}
	var extra []string
	for _, pm := range p.Markets {
		if !known[pm] {
			extra = append(extra, pm)
		}
	}
	v.Extra = strings.Join(extra, ", ")
	return v
}

// SettingsAIProfileSave: POST /settings/ai/profile - the trader's style, the
// evidence they weigh most (in the order the form lists them), their markets
// and their standing instructions.
func (h *Handlers) SettingsAIProfileSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	p := aimethod.Profile{
		Style:      strings.TrimSpace(r.PostFormValue("style")),
		Focus:      r.PostForm["focus"],
		Markets:    r.PostForm["market"],
		HouseRules: r.PostFormValue("house_rules"),
	}
	for _, m := range strings.FieldsFunc(r.PostFormValue("markets_extra"), func(c rune) bool { return c == ',' || c == ' ' || c == ';' }) {
		if len(m) > 12 {
			http.Redirect(w, r, "/settings?err="+errToQuery(fmt.Errorf("%q is not a symbol root", m))+"#profile", http.StatusSeeOther)
			return
		}
		p.Markets = append(p.Markets, m)
	}
	if err := p.Validate(); err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(err)+"#profile", http.StatusSeeOther)
		return
	}
	if err := h.Queries.SetUserAIProfile(ctx, models.SetUserAIProfileParams{AiProfile: p.Encode(), ID: userID(ctx)}); err != nil {
		h.serverError(w, fmt.Errorf("set ai profile: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=ai-profile#profile", http.StatusSeeOther)
}
