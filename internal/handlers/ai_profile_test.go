package handlers

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/aimethod"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The profile form stores the style, the focus in form order, the markets
// (ticked and typed, uppercased) and the rules; bad values are refused.
func TestSettingsAIProfileSave(t *testing.T) {
	h, _, q := newAIHandlers(t)
	ctx := context.Background()
	rec := aiPost(h.SettingsAIProfileSave, aiUser, "/settings/ai/profile", url.Values{
		"style": {"scalper"}, "focus": {"gamma", "levels"}, "market": {"ES"}, "markets_extra": {"zb, 6e"},
		"house_rules": {"  Never add to a loser.  "},
	})
	if flash, errMsg := redirectQuery(t, rec); flash != "ai-profile" || errMsg != "" {
		t.Fatalf("flash %q err %q", flash, errMsg)
	}
	u, err := q.GetUserByID(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	p := aimethod.ParseProfile(u.AiProfile)
	if p.Style != "scalper" || strings.Join(p.Focus, ",") != "gamma,levels" || strings.Join(p.Markets, ",") != "6E,ES,ZB" || p.HouseRules != "Never add to a loser." {
		t.Errorf("stored = %+v (%s)", p, u.AiProfile)
	}
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"style": {"robot"}}, "unknown style"},
		{url.Values{"focus": {"astrology"}}, "unknown focus"},
		{url.Values{"markets_extra": {"NOTASYMBOLROOT"}}, "not a symbol root"},
		{url.Values{"house_rules": {strings.Repeat("x", 2001)}}, "over 2000 characters"},
	} {
		rec := aiPost(h.SettingsAIProfileSave, aiUser, "/settings/ai/profile", c.form)
		if _, errMsg := redirectQuery(t, rec); !strings.Contains(errMsg, c.want) {
			t.Errorf("%v: err=%q, want %q", c.form, errMsg, c.want)
		}
	}
	// The default profile stores as nothing.
	rec = aiPost(h.SettingsAIProfileSave, aiUser, "/settings/ai/profile", url.Values{"style": {"daytrader"}})
	redirectQuery(t, rec)
	if u, _ := q.GetUserByID(ctx, 7); u.AiProfile != "" {
		t.Errorf("default profile stored as %q", u.AiProfile)
	}
	v := h.aiProfileSettings(auth.WithUser(ctx, aiUser))
	if len(v.Styles) != 3 || len(v.Focus) != len(aimethod.FocusKeys) || v.Focus[0].Rank != 0 {
		t.Errorf("view = %+v", v)
	}
}

// A review carries the trader's profile as its own block after the
// vocabulary, and a request may ask for another language once.
func TestReviewCarriesTheProfile(t *testing.T) {
	h, db, q := newAIHandlers(t)
	const key = "sk-profile-key-0001"
	fp := newFakeProvider(t, key, "- **Entry:** late.\n\n<!-- tj-ai:verdict tag=bad strategy=none mistakes=chased emotions=none -->")
	withPresetURL(t, "openai", fp.URL)
	for _, stmt := range []string{
		`UPDATE users SET ai_profile = '{"style":"scalper","focus":["levels","gamma"],"markets":["ES"],"houseRules":"Never add to a loser."}' WHERE id = 7`,
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, 7, 'apex-1', 'motivewave_csv', 'A-1')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl)
			VALUES (10, 1, 'ESZ6', 'ES', 'long', 1, 6800, 6801, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:05:00 +0000 UTC', 'closed', 45)`,
		`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope) VALUES (1, 7, 10, '2026-10-09', 'My note.', 'trade')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	ctx := auth.WithUser(context.Background(), aiUser)
	enc, nonce, _ := h.AISecrets.Seal(key)
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{
		UserID: 7, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.analyzeEntry(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	vocab, prof := strings.Index(fp.user, `"labelVocabulary"`), strings.Index(fp.user, "THE TRADER: a scalper")
	if vocab < 0 || prof < vocab || !strings.Contains(fp.user, "Never add to a loser.") || !strings.Contains(fp.user, "short-term levels") {
		t.Errorf("profile not in the user turn after the vocabulary (%d, %d): %.400q", vocab, prof, fp.user)
	}
	if strings.Contains(fp.user, "Reply in ") {
		t.Errorf("a language asked for without a setting: %.200q", fp.user)
	}
	// One review in Czech, the setting untouched; an unknown language is ignored.
	if _, err := h.analyzeEntry(withAILanguage(ctx, "Czech"), 1, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp.user, "\n\nReply in Czech") || !strings.HasPrefix(fp.system, "WRITE YOUR ENTIRE REVIEW IN Czech") {
		t.Errorf("override not applied: system %.80q user %.60q", fp.system, fp.user)
	}
	if _, err := h.analyzeEntry(withAILanguage(ctx, "Klingon"), 1, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fp.user, "Reply in ") {
		t.Errorf("an unknown language was asked for: %.200q", fp.user)
	}
	if u, _ := q.GetUserByID(ctx, 7); u.AiLanguage != "" {
		t.Errorf("the override changed the setting to %q", u.AiLanguage)
	}
}
