package handlers

import (
	"net/http"
	"testing"
)

// The open suggestions ride on the detail with names; a device key accepts
// one (it becomes the trader's label and leaves the list) or dismisses one;
// an agent key is refused; an unknown one answers 404.
func TestAPIJournalSuggestion(t *testing.T) {
	fx := newJournalEditFixture(t)
	if _, err := fx.h.journalSvc().Suggest(fx.ctx, fx.uid, fx.day.ID, "", 0, []string{"hesitated"}, []string{"calm"}); err != nil {
		t.Fatal(err)
	}
	_, out := fx.call(fx.device, fx.h.APIJournalDetail, http.MethodGet, fx.day.ID, "")
	open, _ := out["suggestions"].([]any)
	if len(open) != 2 {
		t.Fatalf("suggestions = %v, want hesitated and calm", out["suggestions"])
	}
	first, _ := open[0].(map[string]any)
	if first["kind"] != "mistake" || first["value"] != "hesitated" || first["name"] == "hesitated" || first["name"] == "" {
		t.Errorf("first = %v, want the mistake with its display name", first)
	}
	if code, _ := fx.call(fx.agent, fx.h.APIJournalSuggestion, http.MethodPost, fx.day.ID, `{"kind":"emotion","value":"calm","do":"accept"}`); code != http.StatusForbidden {
		t.Errorf("agent key = %d, want 403", code)
	}
	code, out := fx.call(fx.device, fx.h.APIJournalSuggestion, http.MethodPost, fx.day.ID, `{"kind":"emotion","value":"calm","do":"accept"}`)
	if code != http.StatusOK {
		t.Fatalf("accept = %d: %v", code, out)
	}
	labels, _ := out["labels"].(map[string]any)
	emotions, _ := labels["emotions"].([]any)
	if len(emotions) != 1 || emotions[0] != "calm" {
		t.Errorf("emotions after accept = %v, want [calm]", emotions)
	}
	if open, _ := out["suggestions"].([]any); len(open) != 1 {
		t.Errorf("suggestions after accept = %v, want the mistake alone", out["suggestions"])
	}
	if code, _ := fx.call(fx.device, fx.h.APIJournalSuggestion, http.MethodPost, fx.day.ID, `{"kind":"mistake","value":"hesitated","do":"dismiss"}`); code != http.StatusOK {
		t.Errorf("dismiss = %d", code)
	}
	if code, _ := fx.call(fx.device, fx.h.APIJournalSuggestion, http.MethodPost, fx.day.ID, `{"kind":"mistake","value":"hesitated","do":"accept"}`); code != http.StatusNotFound {
		t.Errorf("accept a dismissed one = %d, want 404", code)
	}
	if code, _ := fx.call(fx.device, fx.h.APIJournalSuggestion, http.MethodPost, fx.day.ID, `{"kind":"mistake","value":"hesitated","do":"later"}`); code != http.StatusBadRequest {
		t.Errorf("bad do = %d, want 400", code)
	}
}
