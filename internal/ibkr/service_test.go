package ibkr

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

// fakeGateway implements gatewayClient in memory.
type fakeGateway struct {
	authenticated bool
	ssoAlive      bool // reauthenticate succeeds only while true
	unreachable   bool
	accounts      []string
	trades        []Trade
	tickles       int
	ssodhInits    int
	reauths       int
}

func (f *fakeGateway) Status(ctx context.Context) (AuthStatus, error) {
	if f.unreachable {
		return AuthStatus{}, ErrGatewayUnreachable
	}
	return AuthStatus{Authenticated: f.authenticated, Connected: true}, nil
}

func (f *fakeGateway) Tickle(ctx context.Context) error {
	f.tickles++
	if f.unreachable {
		return ErrGatewayUnreachable
	}
	if !f.authenticated {
		return ErrNotAuthenticated
	}
	return nil
}

func (f *fakeGateway) SsodhInit(ctx context.Context) (SsodhInitResult, error) {
	f.ssodhInits++
	if f.unreachable {
		return SsodhInitResult{}, ErrGatewayUnreachable
	}
	if f.ssoAlive {
		f.authenticated = true
		return SsodhInitResult{Passed: true, Authenticated: true, Connected: true}, nil
	}
	return SsodhInitResult{}, ErrNotAuthenticated
}

func (f *fakeGateway) Reauthenticate(ctx context.Context) error {
	f.reauths++
	if f.unreachable {
		return ErrGatewayUnreachable
	}
	if f.ssoAlive {
		f.authenticated = true
		return nil
	}
	return ErrNotAuthenticated
}

func (f *fakeGateway) Accounts(ctx context.Context) ([]GatewayAccount, error) {
	if !f.authenticated {
		return nil, ErrNotAuthenticated
	}
	out := make([]GatewayAccount, 0, len(f.accounts))
	for _, id := range f.accounts {
		out = append(out, GatewayAccount{ID: id})
	}
	return out, nil
}

func (f *fakeGateway) Trades(ctx context.Context, days int) ([]Trade, error) {
	if !f.authenticated {
		return nil, ErrNotAuthenticated
	}
	return f.trades, nil
}

func (f *fakeGateway) ContractInfo(ctx context.Context, conid int64) (Contract, error) {
	return Contract{LocalSymbol: "ESM6"}, nil
}

func setupIbkr(t *testing.T) (*SyncService, *fakeGateway, *models.Queries, int64, models.IbkrGateway) {
	t.Helper()
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	loc, _ := time.LoadLocation("America/Chicago")
	imp := &importer.ImportService{DB: conn, Queries: q, Timezone: loc, UserID: uid}

	fake := &fakeGateway{authenticated: true, ssoAlive: true, accounts: []string{"U10000001"}}
	svc := NewSyncService(q, imp, SyncOptions{Throttle: 15 * time.Minute}, nil)
	svc.NewClient = func(url string, insecure bool) gatewayClient { return fake }

	gw, err := q.CreateIbkrGateway(context.Background(), models.CreateIbkrGatewayParams{
		UserID: uid, Name: "main", Url: "https://gw.test:4005", TlsInsecure: 1,
	})
	if err != nil {
		t.Fatalf("CreateIbkrGateway: %v", err)
	}
	return svc, fake, q, uid, gw
}

func ibTrade(execID string, side string, size float64, price float64, at time.Time, comm float64) Trade {
	return Trade{
		ExecutionID: execID, Description1: "ESM6", SecType: "FUT", Conid: 1,
		Side: side, Size: size, Price: jsonNum(price), Commission: jsonNum(comm),
		TradeTimeR: at.UnixMilli(), Account: "U10000001", Exchange: "CME",
	}
}

// TestSyncGateway_FreshAccount: unknown account -> cutover = now-7d, fills
// ingested, account auto-created with firm ibkr, commission carried,
// closed trade uses actual commissions.
func TestSyncGateway_FreshAccount(t *testing.T) {
	svc, fake, q, uid, _ := setupIbkr(t)
	ctx := context.Background()
	now := time.Now().UTC()
	fake.trades = []Trade{
		ibTrade("exec.1", "B", 1, 7000, now.Add(-2*time.Hour), 2.10),
		ibTrade("exec.2", "S", 1, 7002, now.Add(-1*time.Hour), 2.10),
	}

	if err := svc.syncUser(ctx, uid); err != nil {
		t.Fatalf("syncUser: %v", err)
	}
	acct, err := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: uid, ImportSource: "motivewave_csv", AccountRef: "U10000001",
	})
	if err != nil {
		t.Fatalf("account not created: %v", err)
	}
	if acct.Firm == nil || *acct.Firm != "ibkr" {
		t.Errorf("firm = %v, want ibkr", acct.Firm)
	}
	if acct.LiveSyncSince == nil {
		t.Fatal("cutover not set")
	}
	// Fresh account: cutover = now-7d (within a small tolerance).
	wantCut := now.Add(-tradesWindowDays * 24 * time.Hour)
	if d := acct.LiveSyncSince.Sub(wantCut); d < -time.Minute || d > time.Minute {
		t.Errorf("cutover = %v, want ~%v", acct.LiveSyncSince, wantCut)
	}
	trades, _ := q.ListTradesForAccount(ctx, acct.ID)
	if len(trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(trades))
	}
	if trades[0].Commission == nil || *trades[0].Commission != 4.20 {
		t.Errorf("commission = %v, want 4.20 (actual)", trades[0].Commission)
	}
}

// TestSyncGateway_ExistingAccountCutover: an account with CSV history gets
// cutover = newest fill + 1ms, so the API only ingests fills after what the
// CSV already delivered.
func TestSyncGateway_ExistingAccountCutover(t *testing.T) {
	svc, fake, q, uid, gw := setupIbkr(t)
	_ = gw
	ctx := context.Background()
	now := time.Now().UTC()

	// Simulate MW-imported history: account + one execution 3h ago.
	lastCSV := now.Add(-3 * time.Hour).Truncate(time.Millisecond)
	csvFill := importer.Fill{
		AccountRef: "U10000001", BrokerExecID: "900001", Symbol: "ESM6", SymbolRoot: "ES",
		Source: "Interactive Brokers", Side: "buy", Qty: 1, Price: 6990, ExecTime: lastCSV,
	}
	if _, err := svc.Importer.ImportAPIFills(ctx, uid, "", []importer.Fill{csvFill}, importer.FormatMotiveWave); err != nil {
		t.Fatalf("seed csv fill: %v", err)
	}

	// API window contains: a duplicate-era fill BEFORE lastCSV, and two after.
	fake.trades = []Trade{
		ibTrade("exec.dup", "B", 1, 6990, lastCSV.Add(-time.Hour), 2.10), // CSV era - must be filtered
		ibTrade("exec.3", "S", 1, 7005, now.Add(-30*time.Minute), 2.10),
	}
	if err := svc.syncUser(ctx, uid); err != nil {
		t.Fatalf("syncUser: %v", err)
	}

	acct, _ := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: uid, ImportSource: "motivewave_csv", AccountRef: "U10000001",
	})
	if acct.LiveSyncSince == nil {
		t.Fatal("cutover not set")
	}
	wantCut := lastCSV.Add(time.Millisecond)
	if d := acct.LiveSyncSince.Sub(wantCut); d < -time.Second || d > time.Second {
		t.Errorf("cutover = %v, want ~%v (newest fill + 1ms)", acct.LiveSyncSince, wantCut)
	}
	// exec.dup filtered (CSV era), exec.3 ingested.
	if _, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "exec.dup",
	}); err == nil {
		t.Error("CSV-era API fill was ingested (double-count)")
	}
	if _, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "exec.3",
	}); err != nil {
		t.Error("post-cutover API fill missing")
	}
}

// TestSyncGateway_NeedsLogin: dead SSO -> reauth fails -> needs-login state
// with exactly ONE sync_log error row across repeated syncs; recovery after
// the user logs back in resets the state.
func TestSyncGateway_NeedsLogin(t *testing.T) {
	svc, fake, q, uid, gw := setupIbkr(t)
	ctx := context.Background()
	fake.authenticated = false
	fake.ssoAlive = false

	for i := 0; i < 3; i++ {
		if err := svc.syncUser(ctx, uid); err != nil {
			t.Fatalf("syncUser %d: %v", i, err)
		}
	}
	if fake.ssodhInits == 0 {
		t.Error("ssodh/init never attempted")
	}
	if fake.reauths == 0 {
		t.Error("reauthenticate fallback never attempted")
	}
	states := svc.GatewayStates(uid)
	if states[gw.ID].State != "needs-login" {
		t.Errorf("state = %q, want needs-login", states[gw.ID].State)
	}
	logs, _ := q.ListSyncLogRecent(ctx, models.ListSyncLogRecentParams{UserID: &uid, Limit: 50})
	errRows := 0
	for _, l := range logs {
		if l.SyncType == "ibkr_api" && l.Status == "error" {
			errRows++
		}
	}
	if errRows != 1 {
		t.Errorf("error rows = %d, want exactly 1 for the outage", errRows)
	}

	// User logs in again (SSO restored): next sync reauthenticates silently.
	fake.ssoAlive = true
	if err := svc.syncUser(ctx, uid); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if st := svc.GatewayStates(uid)[gw.ID]; st.State != "connected" {
		t.Errorf("state after recovery = %q, want connected", st.State)
	}
}

// TestKeepAliveTicklesAndReauths: the keep-alive loop's single pass logic -
// tickle ok for healthy gateways; brokerage-session drop triggers reauth.
func TestSyncGateway_InactiveAccountSkipped(t *testing.T) {
	svc, fake, q, uid, _ := setupIbkr(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Create the account, then deactivate it.
	fake.trades = []Trade{ibTrade("exec.a", "B", 1, 7000, now.Add(-time.Hour), 2.10)}
	if err := svc.syncUser(ctx, uid); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	acct, _ := q.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: uid, ImportSource: "motivewave_csv", AccountRef: "U10000001",
	})
	if _, err := svc.Importer.DB.Exec(`UPDATE accounts SET active = 0 WHERE id = ?`, acct.ID); err != nil {
		t.Fatal(err)
	}

	fake.trades = append(fake.trades, ibTrade("exec.b", "S", 1, 7003, now.Add(-10*time.Minute), 2.10))
	if err := svc.syncUser(ctx, uid); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if _, err := q.GetExecutionByBrokerID(ctx, models.GetExecutionByBrokerIDParams{
		AccountID: acct.ID, BrokerExecID: "exec.b",
	}); err == nil {
		t.Error("inactive account still ingested fills")
	}
}

// TestClientCacheInvalidatesOnURLChange: SQLite reuses row ids, so deleting
// a gateway and adding a new one can give a NEW url the OLD id. The client
// cache must key on the config, not just the id - regression for the bug
// where a re-added gateway kept polling the deleted gateway's address.
func TestClientCacheInvalidatesOnURLChange(t *testing.T) {
	svc, _, _, uid, gw := setupIbkr(t)
	var builtFor []string
	svc.NewClient = func(url string, insecure bool) gatewayClient {
		builtFor = append(builtFor, url)
		return &fakeGateway{authenticated: true, ssoAlive: true}
	}

	_ = svc.clientFor(gw)
	_ = svc.clientFor(gw) // same config -> cached, no rebuild
	if len(builtFor) != 1 || builtFor[0] != gw.Url {
		t.Fatalf("builtFor = %v, want exactly one build for %s", builtFor, gw.Url)
	}

	// Same id, new URL (delete + re-add reusing the rowid).
	gw2 := gw
	gw2.Url = "https://ibgw.test:5007"
	_ = svc.clientFor(gw2)
	if len(builtFor) != 2 || builtFor[1] != "https://ibgw.test:5007" {
		t.Fatalf("builtFor = %v, want a rebuild for the new URL", builtFor)
	}

	// TLS-mode flip alone must also rebuild.
	gw3 := gw2
	gw3.TlsInsecure = 0
	_ = svc.clientFor(gw3)
	if len(builtFor) != 3 {
		t.Fatalf("builtFor = %v, want a rebuild on tls flip", builtFor)
	}
	_ = uid
}

// TestThrottleAndNilReceiver mirrors the topstepx enqueue semantics.
func TestThrottleAndNilReceiver(t *testing.T) {
	svc, _, _, uid, _ := setupIbkr(t)
	svc.MaybeEnqueueUser(uid)
	svc.MaybeEnqueueUser(uid) // pending -> dropped
	if len(svc.jobs) != 1 {
		t.Fatalf("queue = %d, want 1", len(svc.jobs))
	}
	var nilSvc *SyncService
	nilSvc.MaybeEnqueueUser(uid) // no panic
}

// Unused-var guards for the linter in tests that ignore gw.
var _ = errors.Is
