package push

import (
	"context"
	"testing"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Registration keeps a phone with its settings, a second registration of
// the token updates it, settings and activities replace whole, a foreign
// token is not found, removal forgets it, and bad input is refused.
func TestServiceDevices(t *testing.T) {
	conn := dbtest.Open(t, "test")
	uid := dbtest.BootstrapUserID(t, conn, "test")
	s := &Service{DB: conn, Q: models.New(conn)}
	ctx := context.Background()
	tok := "ABCDEF0123456789abcdef0123456789"
	d, err := s.Register(ctx, uid, Registration{Token: tok, Name: "Phone", Sandbox: true, BundleID: "nl.w4j.tradingjournal", Prefs: &Prefs{NoActivity: true}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Token != "abcdef0123456789abcdef0123456789" || !d.Sandbox || d.Prefs == nil || !d.Prefs.NoActivity || d.ActivityStart {
		t.Errorf("registered = %+v", d)
	}
	if _, err := s.Register(ctx, uid, Registration{Token: "short"}); err == nil {
		t.Errorf("short token accepted")
	}
	if _, err := s.Register(ctx, uid, Registration{Token: tok, Prefs: &Prefs{Kinds: map[string]KindPrefs{"x": {}}}}); err == nil {
		t.Errorf("bad prefs accepted")
	}
	d, err = s.Register(ctx, uid, Registration{Token: tok, Name: "Renamed", Sandbox: false})
	if err != nil || d.Name != "Renamed" || d.Sandbox {
		t.Errorf("re-registration = %+v (%v)", d, err)
	}
	list, _ := s.List(ctx, uid)
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	d, err = s.SetPrefs(ctx, uid, tok, &Prefs{Kinds: map[string]KindPrefs{KindNudge: {Off: true}}})
	if err != nil || !d.Prefs.Kinds[KindNudge].Off || d.Prefs.NoActivity {
		t.Errorf("prefs = %+v (%v)", d.Prefs, err)
	}
	if _, err := s.SetPrefs(ctx, uid+1, tok, &Prefs{}); err != ErrNotFound {
		t.Errorf("foreign prefs = %v", err)
	}
	start := "ffffffffffffffffffffffffffffffff"
	d, err = s.SetActivities(ctx, uid, tok, &start, []Activity{{ID: "A1", Token: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}})
	if err != nil || !d.ActivityStart || len(d.Activities) != 1 || d.Activities[0].Kind != "session" {
		t.Errorf("activities = %+v (%v)", d, err)
	}
	d, err = s.SetActivities(ctx, uid, tok, nil, []Activity{})
	if err != nil || !d.ActivityStart || len(d.Activities) != 0 {
		t.Errorf("cleared activities = %+v (%v)", d, err)
	}
	if _, err := s.SetActivities(ctx, uid, tok, nil, []Activity{{ID: "", Token: "x"}}); err == nil {
		t.Errorf("bad activity accepted")
	}
	targets, _ := s.targets(ctx, uid)
	if len(targets) != 1 || targets[0].prefs.Kinds[KindNudge].Off != true {
		t.Errorf("targets = %+v", targets)
	}
	if ok, _ := s.Remove(ctx, uid+1, tok); ok {
		t.Errorf("a foreign removal succeeded")
	}
	if ok, _ := s.Remove(ctx, uid, tok); !ok {
		t.Errorf("removal failed")
	}
	if list, _ := s.List(ctx, uid); len(list) != 0 {
		t.Errorf("device survived removal")
	}
	var none *Service
	if none.Enabled() || s.Enabled() {
		t.Errorf("enabled without an APNs client")
	}
}
