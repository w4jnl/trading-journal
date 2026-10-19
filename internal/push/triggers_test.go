package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/push/apns"
)

// sent is one push the stand-in received.
type sent struct {
	path, pushType, topic, priority, collapse string
	aps                                       map[string]any
	body                                      map[string]any
}

// apnsStandIn is an HTTP/2 server that records every push.
type apnsStandIn struct {
	*httptest.Server
	mu   sync.Mutex
	got  []sent
	fail bool // answer 410
}

func newStandIn(t *testing.T) (*apnsStandIn, *apns.Client) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	st := &apnsStandIn{}
	st.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		aps, _ := body["aps"].(map[string]any)
		st.mu.Lock()
		st.got = append(st.got, sent{r.URL.Path, r.Header.Get("apns-push-type"), r.Header.Get("apns-topic"), r.Header.Get("apns-priority"), r.Header.Get("apns-collapse-id"), aps, body})
		fail := st.fail
		st.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"reason":"Unregistered"}`)
		}
	}))
	st.EnableHTTP2 = true
	st.StartTLS()
	t.Cleanup(st.Close)
	c, err := apns.New(apns.Config{Key: pemKey, KeyID: "KEY1234567", TeamID: "TEAM123456", Topic: "nl.w4j.tradingjournal", URL: st.URL, HTTP: st.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return st, c
}

func (st *apnsStandIn) drain() []sent {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := st.got
	st.got = nil
	return out
}

// pushFixture is a user with a phone and a working stand-in.
type pushFixture struct {
	s    *Service
	st   *apnsStandIn
	q    *models.Queries
	uid  int64
	acct models.Account
	ctx  context.Context
	now  time.Time
}

func newPushFixture(t *testing.T, prefs *Prefs) *pushFixture {
	t.Helper()
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	st, client := newStandIn(t)
	chicago, _ := time.LoadLocation("America/Chicago")
	fx := &pushFixture{st: st, q: q, uid: uid, ctx: context.Background()}
	// A Wednesday 09:30 Chicago.
	fx.now = time.Date(2026, 10, 14, 14, 30, 0, 0, time.UTC)
	fx.s = &Service{DB: conn, Q: q, APNs: client, Live: livedata.NewNotifier(), TZ: time.UTC, Chicago: chicago, Now: func() time.Time { return fx.now }}
	if _, err := fx.s.Register(fx.ctx, uid, Registration{Token: "0123456789abcdef0123456789abcdef", Name: "Phone", Sandbox: true, Prefs: prefs}); err != nil {
		t.Fatal(err)
	}
	firm := "apex"
	acct, err := q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: uid, Name: "APEX-1", Firm: &firm, ImportSource: "motivewave_csv", AccountRef: "A1", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	fx.acct = acct
	return fx
}

func (fx *pushFixture) closeTrade(t *testing.T, id int64, net float64, exit time.Time) {
	t.Helper()
	if _, err := fx.s.DB.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl)
		VALUES (?, ?, 'MESZ6', 'MES', 'short', 2, 7000, 6990, ?, ?, 'closed', ?)`, id, fx.acct.ID, exit.Add(-10*time.Minute), exit, net); err != nil {
		t.Fatal(err)
	}
}

// A recorded nudge becomes a time-sensitive alert with the app's sound,
// the lesson in the body, the route and the badge, then a silent sync
// carrying the unseen ids; a phone with nudges off gets the sync only.
func TestNudgeCreated(t *testing.T) {
	fx := newPushFixture(t, nil)
	if _, err := fx.s.Register(fx.ctx, fx.uid, Registration{Token: "ffffffffffffffffffffffffffffffff", Name: "Quiet", Prefs: &Prefs{Kinds: map[string]KindPrefs{KindNudge: {Off: true}}}}); err != nil {
		t.Fatal(err)
	}
	lesson := "Stop after two losers."
	n, err := fx.q.InsertNudge(fx.ctx, models.InsertNudgeParams{UserID: fx.uid, Day: "2026-10-14", Key: "unplanned-open", Ref: "1", Severity: "warn",
		Title: "No stop recorded on MESZ6", Detail: "Open long 2 MESZ6 since 09:21 with no plan.", Lesson: &lesson, Href: "/trades?status=open&sheet=pos:7:MESZ6"})
	if err != nil {
		t.Fatal(err)
	}
	fx.s.NudgeCreated(fx.ctx, fx.uid, []models.Nudge{n})
	got := fx.st.drain()
	if len(got) != 3 {
		t.Fatalf("pushes = %d, want alert + 2 syncs: %+v", len(got), got)
	}
	alert := got[0]
	al := alert.aps["alert"].(map[string]any)
	if alert.pushType != "alert" || alert.path != "/3/device/0123456789abcdef0123456789abcdef" || alert.aps["category"] != CategoryPosition ||
		al["title"] != "No stop recorded on MESZ6" || al["subtitle"] != "MESZ6 · open" || al["body"] != "Open long 2 MESZ6 since 09:21 with no plan. — Your lesson: Stop after two losers." ||
		alert.aps["interruption-level"] != "time-sensitive" || alert.aps["sound"] != SoundNudge || alert.aps["badge"].(float64) != 1 || alert.aps["mutable-content"].(float64) != 1 ||
		alert.body["route"] != "position/7/MESZ6" || alert.body["kind"] != "nudge" || alert.body["id"].(float64) != float64(n.ID) {
		t.Errorf("alert = %+v", alert)
	}
	syncs := 0
	for _, s := range got[1:] {
		if s.pushType != "background" || s.aps["content-available"].(float64) != 1 || s.body["kind"] != "sync" || s.body["badge"].(float64) != 1 || len(s.body["unseen"].([]any)) != 1 {
			t.Errorf("sync = %+v", s)
		}
		syncs++
	}
	if syncs != 2 {
		t.Errorf("syncs = %d", syncs)
	}
	// A day nudge goes under the nudge category with the day as subtitle.
	d, _ := fx.q.InsertNudge(fx.ctx, models.InsertNudgeParams{UserID: fx.uid, Day: "2026-10-14", Key: "stop-time", Ref: "x", Severity: "warn", Title: "Stop time", Detail: "It is 14:30.", Href: "/journal/2026-10-14"})
	fx.s.NudgeCreated(fx.ctx, fx.uid, []models.Nudge{d})
	got = fx.st.drain()
	if al := got[0].aps["alert"].(map[string]any); got[0].aps["category"] != CategoryNudge || al["subtitle"] != "Wed 14 Oct" || got[0].body["route"] != "day/2026-10-14" {
		t.Errorf("day nudge = %+v", got[0])
	}
}

// The first tick only remembers the recent trades; a trade that closes
// after a data change becomes a quiet alert and one sync; nothing repeats
// on a tick without a change; a phone that is quiet outside RTH gets the
// sync only when the clock is outside regular hours.
func TestDataChangedClosedTrade(t *testing.T) {
	fx := newPushFixture(t, &Prefs{Kinds: map[string]KindPrefs{KindTradeClosed: {Sound: "tj"}}})
	fx.closeTrade(t, 1, -33.3, fx.now.Add(-5*time.Minute))
	fx.s.Tick(fx.ctx) // snapshot
	if got := fx.st.drain(); len(got) != 0 {
		t.Fatalf("the first tick pushed: %+v", got)
	}
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Fatalf("an unchanged tick pushed: %+v", got)
	}
	fx.closeTrade(t, 2, 71, fx.now.Add(-time.Minute))
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	got := fx.st.drain()
	if len(got) != 2 {
		t.Fatalf("pushes = %d, want trade + sync: %+v", len(got), got)
	}
	al := got[0].aps["alert"].(map[string]any)
	if got[0].aps["category"] != CategoryTrade || al["title"] != "MES short" || al["subtitle"] != "APEX-1" || al["body"] != "+71.00 · 14:29" ||
		got[0].aps["interruption-level"] != nil || got[0].aps["sound"] != SoundTrade || got[0].body["route"] != "trade/2" || got[0].body["netPnl"].(float64) != 71 {
		t.Errorf("trade alert = %+v", got[0])
	}
	if got[1].pushType != "background" || got[1].body["kind"] != "sync" {
		t.Errorf("sync = %+v", got[1])
	}
	// The same version again: nothing; a plan change (a bump without a
	// trade): the sync only.
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Errorf("repeat = %+v", got)
	}
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 1 || got[0].pushType != "background" {
		t.Errorf("plain change = %+v", got)
	}
	// Quiet outside RTH: at 03:00 Chicago the trade alert is skipped.
	if _, err := fx.s.SetPrefs(fx.ctx, fx.uid, "0123456789abcdef0123456789abcdef", &Prefs{QuietOutsideRth: true}); err != nil {
		t.Fatal(err)
	}
	fx.now = time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	fx.closeTrade(t, 3, 10, fx.now.Add(-time.Minute))
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 1 || got[0].pushType != "background" {
		t.Errorf("outside RTH = %+v", got)
	}
	// A refused token forgets the phone.
	fx.st.mu.Lock()
	fx.st.fail = true
	fx.st.mu.Unlock()
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	if list, _ := fx.s.List(fx.ctx, fx.uid); len(list) != 0 {
		t.Errorf("a 410 did not forget the phone: %+v", list)
	}
}

// A copied exit closes one trade per account; the phone hears the decision
// once: the legs in the title, the accounts as subtitle, the summed net,
// the first leg's id and the set id in the payload. A leg that lands a
// tick later re-tells the whole decision under the same collapse id, so
// the banner is replaced, not stacked.
func TestDataChangedClosedSet(t *testing.T) {
	fx := newPushFixture(t, nil)
	firm := "apex"
	beta, err := fx.q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: fx.uid, Name: "APEX-2", Firm: &firm, ImportSource: "motivewave_csv", AccountRef: "A2", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	gamma, err := fx.q.CreateAccount(fx.ctx, models.CreateAccountParams{UserID: fx.uid, Name: "APEX-3", Firm: &firm, ImportSource: "motivewave_csv", AccountRef: "A3", Currency: "USD", CommissionSource: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	fx.s.Tick(fx.ctx) // snapshot
	exit := fx.now.Add(-time.Minute)
	closeLeg := func(id, acct int64, net float64, exit time.Time) {
		t.Helper()
		if _, err := fx.s.DB.Exec(`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl)
			VALUES (?, ?, 'MESZ6', 'MES', 'short', 5, 7769, 7766.25, ?, ?, 'closed', ?)`, id, acct, exit.Add(-10*time.Minute), exit, net); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.s.DB.Exec(`INSERT INTO trade_set_members (trade_id, set_id) VALUES (?, 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.s.DB.Exec(`INSERT INTO trade_sets (id, user_id, cluster_key, symbol_root, direction, entry_time, members, accounts, qty, entry_avg, entry_spread)
		VALUES (1, ?, 'ES=F', 'MES', 'short', ?, 2, 2, 10, 7769, 0)`, fx.uid, exit.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closeLeg(10, fx.acct.ID, 12.5, exit)
	closeLeg(11, beta.ID, 12.5, exit.Add(time.Second))
	fx.closeTrade(t, 12, -8, exit.Add(2*time.Second)) // a lone trade on the first account, told on its own
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	got := fx.st.drain()
	if len(got) != 3 {
		t.Fatalf("pushes = %d, want set + trade + sync: %+v", len(got), got)
	}
	al := got[0].aps["alert"].(map[string]any)
	if al["title"] != "MES short ×2" || al["subtitle"] != "2 accounts" || al["body"] != "+25.00 · 14:29" ||
		got[0].body["tradeId"].(float64) != 10 || got[0].body["route"] != "trade/10" || got[0].body["setId"].(float64) != 1 || got[0].body["legs"].(float64) != 2 ||
		got[0].body["netPnl"].(float64) != 25 || got[0].collapse != "trade:"+(closedTrade{AccountID: fx.acct.ID, Symbol: "MESZ6", Direction: "short", Qty: 5, ExitTime: exit}).key() {
		t.Errorf("set alert = %+v (collapse %q)", got[0], got[0].collapse)
	}
	if al := got[1].aps["alert"].(map[string]any); al["title"] != "MES short" || al["subtitle"] != "APEX-1" || got[1].body["legs"].(float64) != 1 || got[1].body["setId"] != nil {
		t.Errorf("lone alert = %+v", got[1])
	}
	// The third leg arrives a tick later: the decision again, now ×3, same collapse id.
	closeLeg(13, gamma.ID, 10, exit.Add(3*time.Second))
	fx.s.Live.Bump(fx.uid)
	fx.s.Tick(fx.ctx)
	got = fx.st.drain()
	if len(got) != 2 {
		t.Fatalf("pushes = %d, want set + sync: %+v", len(got), got)
	}
	if al := got[0].aps["alert"].(map[string]any); al["title"] != "MES short ×3" || al["subtitle"] != "3 accounts" || al["body"] != "+35.00 · 14:29" ||
		got[0].body["tradeId"].(float64) != 10 || got[0].collapse != "trade:"+(closedTrade{AccountID: fx.acct.ID, Symbol: "MESZ6", Direction: "short", Qty: 5, ExitTime: exit}).key() {
		t.Errorf("late leg alert = %+v", got[0])
	}
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Errorf("repeat = %+v", got)
	}
}

// The missing-plan reminder goes once, between 08:15 and 08:30 Chicago,
// to a phone that opted in, when the guardrail is on and no plan exists.
func TestPlanMissing(t *testing.T) {
	fx := newPushFixture(t, &Prefs{Kinds: map[string]KindPrefs{KindPlanMissing: {}}})
	if err := fx.q.SetUserGuardrail(fx.ctx, models.SetUserGuardrailParams{Guardrail: 1, ID: fx.uid}); err != nil {
		t.Fatal(err)
	}
	fx.s.Tick(fx.ctx) // 09:30: too late today, nothing
	if got := fx.st.drain(); len(got) != 0 {
		t.Fatalf("09:30 = %+v", got)
	}
	fx.now = time.Date(2026, 10, 15, 13, 20, 0, 0, time.UTC) // Thu 08:20 Chicago
	fx.s.Tick(fx.ctx)
	got := fx.st.drain()
	if len(got) != 1 || got[0].aps["alert"].(map[string]any)["title"] != "No plan for today" || got[0].body["route"] != "plan" || got[0].aps["interruption-level"] != nil {
		t.Fatalf("reminder = %+v", got)
	}
	fx.s.Tick(fx.ctx)
	if got := fx.st.drain(); len(got) != 0 {
		t.Errorf("reminded twice: %+v", got)
	}
}
