package topstepx

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

// fakeAPI implements apiClient in-memory and records the trade-search windows
// it received, so tests can assert the incremental/walk-back behavior.
type fakeAPI struct {
	loginCalls    int
	validateCalls int
	validateErr   error // nil = validate succeeds (rolls token)
	loginErr      error

	accounts []Account
	// tradesFn decides what each SearchTrades window returns.
	tradesFn func(accountID int32, start, end time.Time) []HalfTrade
	windows  []struct{ Start, End time.Time }
}

func (f *fakeAPI) LoginKey(ctx context.Context, u, k string) (string, error) {
	f.loginCalls++
	if f.loginErr != nil {
		return "", f.loginErr
	}
	return "tok-login", nil
}

func (f *fakeAPI) Validate(ctx context.Context, token string) (string, error) {
	f.validateCalls++
	if f.validateErr != nil {
		return "", f.validateErr
	}
	return "tok-rolled", nil
}

func (f *fakeAPI) SearchAccounts(ctx context.Context, token string, onlyActive bool) ([]Account, error) {
	return f.accounts, nil
}

func (f *fakeAPI) SearchTrades(ctx context.Context, token string, accountID int32, start, end time.Time) ([]HalfTrade, error) {
	f.windows = append(f.windows, struct{ Start, End time.Time }{start, end})
	if f.tradesFn == nil {
		return nil, nil
	}
	return f.tradesFn(accountID, start, end), nil
}

func (f *fakeAPI) SearchContractByID(ctx context.Context, token, id string) (Contract, error) {
	return Contract{}, errors.New("not used")
}

func setupSync(t *testing.T) (*SyncService, *fakeAPI, *models.Queries, int64) {
	t.Helper()
	conn := dbtest.Open(t, "test")
	q := models.New(conn)
	uid := dbtest.BootstrapUserID(t, conn, "test")
	loc, _ := time.LoadLocation("America/Chicago")
	imp := &importer.ImportService{DB: conn, Queries: q, Timezone: loc, UserID: uid}
	api := &fakeAPI{}
	box, _ := NewSecretBox(bytes.Repeat([]byte{7}, 32))
	svc := NewSyncService(q, imp, api, box, SyncOptions{
		Throttle:     15 * time.Minute,
		SessionEvery: time.Hour,
		RTHEvery:     15 * time.Minute,
		RTHStartMin:  7*60 + 30, // 07:30 Chicago
		RTHEndMin:    16 * 60,   // 16:00 Chicago
	}, nil)

	// Store working credentials for the bootstrap user.
	ct, nonce, _ := box.Seal("api-key")
	if err := q.UpsertTopstepxCredentials(context.Background(), models.UpsertTopstepxCredentialsParams{
		UserID: uid, Username: "trader", ApiKeyEnc: ct, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatalf("store creds: %v", err)
	}
	return svc, api, q, uid
}

// mesHalf builds a filled MES half-trade at the given time.
func mesHalf(id, orderID int64, side int, size int32, price float64, at time.Time) HalfTrade {
	fees := 0.37
	return HalfTrade{
		ID: id, OrderID: orderID, ContractID: "CON.F.US.MES.M26", Side: side,
		Size: size, Price: price, Fees: fees, CreationTimestamp: at,
	}
}

// TestSyncUser_FreshAccountWalkBack proves a never-seen account triggers the
// month-by-month walk-back, stops after 3 consecutive empty months, ingests
// the fills it found, and stores broker commissions on the trade.
func TestSyncUser_FreshAccountWalkBack(t *testing.T) {
	svc, api, q, uid := setupSync(t)
	api.accounts = []Account{{ID: 42, Name: "EXPRESS-TEST-1"}}

	// History exists only in the current month: one closed MES round-turn.
	tradeAt := time.Now().UTC().Add(-2 * time.Hour)
	api.tradesFn = func(accountID int32, start, end time.Time) []HalfTrade {
		if tradeAt.After(start) && tradeAt.Before(end) {
			return []HalfTrade{
				mesHalf(1, 100, 0, 1, 7300, tradeAt),
				mesHalf(2, 101, 1, 1, 7302, tradeAt.Add(time.Minute)),
			}
		}
		return nil
	}

	if err := svc.syncUser(context.Background(), syncJob{UserID: uid}); err != nil {
		t.Fatalf("syncUser: %v", err)
	}

	// Walk-back: month 0 had trades, then 3 empty months -> exactly 4 windows.
	if len(api.windows) != 4 {
		t.Errorf("windows = %d, want 4 (1 hit + 3 empty stop)", len(api.windows))
	}

	acct, err := q.GetAccountByRef(context.Background(), models.GetAccountByRefParams{
		UserID: uid, ImportSource: "topstepx_csv", AccountRef: "EXPRESS-TEST-1",
	})
	if err != nil {
		t.Fatalf("account not auto-created: %v", err)
	}
	if acct.Firm == nil || *acct.Firm != "topstep" {
		t.Errorf("firm = %v, want topstep", acct.Firm)
	}
	if acct.ApiBackfillDone != 1 {
		t.Errorf("api_backfill_done = %d, want 1", acct.ApiBackfillDone)
	}

	trades, _ := q.ListTradesForAccount(context.Background(), acct.ID)
	if len(trades) != 1 {
		t.Fatalf("trades = %d, want 1", len(trades))
	}
	// Broker commission: 0.37 per half, both legs actual = 0.74.
	if trades[0].Commission == nil || *trades[0].Commission != 0.74 {
		t.Errorf("commission = %v, want 0.74 (from API fees)", trades[0].Commission)
	}

	// A second sync is incremental now: exactly one more window, starting
	// 24h behind the newest fill.
	api.windows = nil
	if err := svc.syncUser(context.Background(), syncJob{UserID: uid}); err != nil {
		t.Fatalf("second syncUser: %v", err)
	}
	if len(api.windows) != 1 {
		t.Fatalf("incremental windows = %d, want 1", len(api.windows))
	}
	wantStart := trades[0].EntryTime.Add(time.Minute).Add(-incrementalOverlap) // exit fill is newest
	gotStart := api.windows[0].Start
	if diff := gotStart.Sub(wantStart); diff > 2*time.Second || diff < -2*time.Second {
		t.Errorf("incremental start = %v, want ~%v", gotStart, wantStart)
	}

	// Dedup: the overlap re-delivered both fills, but nothing was re-inserted.
	trades2, _ := q.ListTradesForAccount(context.Background(), acct.ID)
	if len(trades2) != 1 {
		t.Errorf("trades after resync = %d, want 1 (dedup)", len(trades2))
	}
}

// TestSyncUser_InlineAccountSkipped proves a trades-CSV (inline) claimed
// account is never touched by API sync and gets a 'skipped' log row.
func TestSyncUser_InlineAccountSkipped(t *testing.T) {
	svc, api, q, uid := setupSync(t)
	ctx := context.Background()

	firm := "topstep"
	acct, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID: uid, Name: "INLINE-1", Firm: &firm, ImportSource: "topstepx_csv",
		AccountRef: "INLINE-1", Currency: "USD", CommissionSource: "inline",
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	api.accounts = []Account{{ID: 7, Name: "INLINE-1"}}
	api.tradesFn = func(int32, time.Time, time.Time) []HalfTrade {
		t.Error("SearchTrades must not be called for an inline account")
		return nil
	}

	if err := svc.syncUser(ctx, syncJob{UserID: uid}); err != nil {
		t.Fatalf("syncUser: %v", err)
	}
	rows, _ := q.ListLastAPISyncPerAccount(ctx, models.ListLastAPISyncPerAccountParams{UserID: &uid, SyncType: "topstepx_api"})
	if len(rows) != 1 || rows[0].Status != "skipped" || *rows[0].AccountID != acct.ID {
		t.Fatalf("want one skipped row for the inline account, got %+v", rows)
	}
}

// TestSyncUser_InactiveAccountSkipped: accounts deactivated on /accounts are
// not synced at all (no API calls, no log rows).
func TestSyncUser_InactiveAccountSkipped(t *testing.T) {
	svc, api, q, uid := setupSync(t)
	ctx := context.Background()

	firm := "topstep"
	if _, err := q.CreateAccount(ctx, models.CreateAccountParams{
		UserID: uid, Name: "OLD-1", Firm: &firm, ImportSource: "topstepx_csv",
		AccountRef: "OLD-1", Currency: "USD", CommissionSource: "rate",
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if _, err := svc.Importer.DB.Exec(`UPDATE accounts SET active = 0 WHERE account_ref = 'OLD-1'`); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	api.accounts = []Account{{ID: 7, Name: "OLD-1"}}
	if err := svc.syncUser(ctx, syncJob{UserID: uid}); err != nil {
		t.Fatalf("syncUser: %v", err)
	}
	if len(api.windows) != 0 {
		t.Errorf("windows = %d, want 0 (inactive account untouched)", len(api.windows))
	}
}

// loadCreds re-reads the credentials row (the persisted token lives there).
func loadCreds(t *testing.T, q *models.Queries, uid int64) models.TopstepxCredential {
	t.Helper()
	creds, err := q.GetTopstepxCredentials(context.Background(), uid)
	if err != nil {
		t.Fatalf("GetTopstepxCredentials: %v", err)
	}
	return creds
}

// seedToken stores an encrypted session token with a chosen expiry.
func seedToken(t *testing.T, svc *SyncService, uid int64, token string, expiresIn time.Duration) {
	t.Helper()
	enc, nonce, err := svc.Box.Seal(token)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	exp := time.Now().UTC().Add(expiresIn)
	if err := svc.Queries.SetTopstepxToken(context.Background(), models.SetTopstepxTokenParams{
		TokenEnc: enc, TokenNonce: nonce, TokenExpiresAt: &exp, UserID: uid,
	}); err != nil {
		t.Fatalf("SetTopstepxToken: %v", err)
	}
}

// TestTokenLifecycle: persisted token is used as-is while valid (zero
// network); refresh is validate-first with loginKey as last resort; the
// token survives a "restart" (new service instance over the same DB).
func TestTokenLifecycle(t *testing.T) {
	svc, api, q, uid := setupSync(t)
	ctx := context.Background()

	// No stored token: straight to loginKey, then persisted.
	tok, err := svc.token(ctx, loadCreds(t, q, uid), "api-key")
	if err != nil || tok != "tok-login" {
		t.Fatalf("first token = (%q, %v)", tok, err)
	}
	if api.loginCalls != 1 || api.validateCalls != 0 {
		t.Errorf("calls = login %d / validate %d, want 1/0", api.loginCalls, api.validateCalls)
	}
	creds := loadCreds(t, q, uid)
	if len(creds.TokenEnc) == 0 || creds.TokenExpiresAt == nil {
		t.Fatal("token not persisted")
	}
	// Fallback TTL applies (mock token is not a JWT): ~23h out.
	if until := time.Until(*creds.TokenExpiresAt); until < 22*time.Hour || until > 24*time.Hour {
		t.Errorf("persisted expiry %v from now, want ~23h", until)
	}

	// Stored token still valid: NO network calls at all.
	tok, err = svc.token(ctx, creds, "api-key")
	if err != nil || tok != "tok-login" {
		t.Fatalf("second token = (%q, %v)", tok, err)
	}
	if api.loginCalls != 1 || api.validateCalls != 0 {
		t.Errorf("valid stored token must cost zero API calls, got login %d / validate %d",
			api.loginCalls, api.validateCalls)
	}

	// "Restart": a brand-new service over the same DB still needs no auth.
	svc2 := NewSyncService(svc.Queries, svc.Importer, api, svc.Box, SyncOptions{Throttle: 15 * time.Minute}, nil)
	tok, err = svc2.token(ctx, loadCreds(t, q, uid), "api-key")
	if err != nil || tok != "tok-login" {
		t.Fatalf("post-restart token = (%q, %v)", tok, err)
	}
	if api.loginCalls != 1 || api.validateCalls != 0 {
		t.Errorf("restart must reuse the persisted token, got login %d / validate %d",
			api.loginCalls, api.validateCalls)
	}

	// Near expiry (< use margin): inline refresh validates first, no login.
	seedToken(t, svc, uid, "tok-login", 2*time.Minute)
	tok, err = svc.token(ctx, loadCreds(t, q, uid), "api-key")
	if err != nil || tok != "tok-rolled" {
		t.Fatalf("near-expiry token = (%q, %v)", tok, err)
	}
	if api.loginCalls != 1 || api.validateCalls != 1 {
		t.Errorf("near-expiry refresh = login %d / validate %d, want 1/1", api.loginCalls, api.validateCalls)
	}

	// Validate rejects the session: fall back to loginKey.
	api.validateErr = ErrAuth
	seedToken(t, svc, uid, "tok-dead", time.Minute)
	tok, err = svc.token(ctx, loadCreds(t, q, uid), "api-key")
	if err != nil || tok != "tok-login" {
		t.Fatalf("rejected-session token = (%q, %v)", tok, err)
	}
	if api.loginCalls != 2 {
		t.Errorf("loginCalls = %d, want 2 (re-auth is last resort)", api.loginCalls)
	}

	// Both fail: abort with ErrAuth.
	api.loginErr = ErrAuth
	seedToken(t, svc, uid, "tok-dead", time.Minute)
	if _, err = svc.token(ctx, loadCreds(t, q, uid), "api-key"); !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}

	// Validate hits a network problem (not auth): do NOT burn a login.
	api.validateErr = errors.New("connection refused")
	logins := api.loginCalls
	seedToken(t, svc, uid, "tok-x", time.Minute)
	if _, err = svc.token(ctx, loadCreds(t, q, uid), "api-key"); err == nil {
		t.Error("network validate error should propagate")
	}
	if api.loginCalls != logins {
		t.Error("network validate error must not trigger loginKey")
	}
}

// TestSyncUsesStoredToken: a full syncUser pass with a warm token performs
// ZERO auth calls - the exact complaint this change fixes.
func TestSyncUsesStoredToken(t *testing.T) {
	svc, api, _, uid := setupSync(t)
	api.accounts = []Account{{ID: 42, Name: "NOHISTORY-1"}}
	seedToken(t, svc, uid, "tok-warm", 10*time.Hour)

	if err := svc.syncUser(context.Background(), syncJob{UserID: uid}); err != nil {
		t.Fatalf("syncUser: %v", err)
	}
	if api.loginCalls != 0 || api.validateCalls != 0 {
		t.Errorf("sync with warm token = login %d / validate %d, want 0/0",
			api.loginCalls, api.validateCalls)
	}
}

// TestMaintainTokensPass: the background maintainer renews near-expiry
// tokens via validate, acquires missing ones via loginKey, and leaves warm
// tokens alone.
func TestMaintainTokensPass(t *testing.T) {
	svc, api, q, uid := setupSync(t)
	ctx := context.Background()

	// No token stored yet: the pass acquires one (loginKey - nothing to validate).
	svc.maintainTokensPass(ctx)
	if api.loginCalls != 1 || api.validateCalls != 0 {
		t.Fatalf("acquire pass = login %d / validate %d, want 1/0", api.loginCalls, api.validateCalls)
	}

	// Warm token (23h fallback): the next pass does nothing.
	svc.maintainTokensPass(ctx)
	if api.loginCalls != 1 || api.validateCalls != 0 {
		t.Errorf("warm pass must be free, got login %d / validate %d", api.loginCalls, api.validateCalls)
	}

	// Under the refresh margin: renewed via validate, no login, expiry advanced.
	seedToken(t, svc, uid, "tok-login", time.Hour)
	svc.maintainTokensPass(ctx)
	if api.loginCalls != 1 || api.validateCalls != 1 {
		t.Errorf("renew pass = login %d / validate %d, want 1/1", api.loginCalls, api.validateCalls)
	}
	creds := loadCreds(t, q, uid)
	if creds.TokenExpiresAt == nil || time.Until(*creds.TokenExpiresAt) < 20*time.Hour {
		t.Errorf("expiry not advanced: %v", creds.TokenExpiresAt)
	}
	if tok, ok := svc.storedToken(creds); !ok || tok != "tok-rolled" {
		t.Errorf("stored token = (%q, %v), want tok-rolled", tok, ok)
	}
}

// TestUpsertCredentialsClearsToken: replacing credentials wipes the stored
// session token so the new pair starts with a fresh login.
func TestUpsertCredentialsClearsToken(t *testing.T) {
	svc, _, q, uid := setupSync(t)
	seedToken(t, svc, uid, "tok-old", 10*time.Hour)

	ct, nonce, _ := svc.Box.Seal("new-api-key")
	if err := q.UpsertTopstepxCredentials(context.Background(), models.UpsertTopstepxCredentialsParams{
		UserID: uid, Username: "trader2", ApiKeyEnc: ct, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	creds := loadCreds(t, q, uid)
	if len(creds.TokenEnc) != 0 || creds.TokenExpiresAt != nil {
		t.Errorf("token not cleared on credential replace: enc=%d exp=%v", len(creds.TokenEnc), creds.TokenExpiresAt)
	}
}

// TestMaybeEnqueueUserThrottle: a second page visit within the throttle
// window is dropped; a pending job dedups too.
func TestMaybeEnqueueUserThrottle(t *testing.T) {
	svc, _, _, uid := setupSync(t)

	svc.MaybeEnqueueUser(uid)
	if len(svc.jobs) != 1 {
		t.Fatalf("queue = %d, want 1", len(svc.jobs))
	}
	svc.MaybeEnqueueUser(uid) // pending -> dropped
	if len(svc.jobs) != 1 {
		t.Fatalf("queue after pending dup = %d, want 1", len(svc.jobs))
	}

	// Simulate the worker finishing just now: pending cleared, lastRun fresh.
	<-svc.jobs
	svc.mu.Lock()
	svc.pending[uid] = false
	svc.lastRun[uid] = time.Now()
	svc.mu.Unlock()

	svc.MaybeEnqueueUser(uid) // inside throttle window -> dropped
	if len(svc.jobs) != 0 {
		t.Fatalf("queue inside throttle = %d, want 0", len(svc.jobs))
	}

	svc.mu.Lock()
	svc.lastRun[uid] = time.Now().Add(-16 * time.Minute)
	svc.mu.Unlock()
	svc.MaybeEnqueueUser(uid) // window expired -> enqueued
	if len(svc.jobs) != 1 {
		t.Fatalf("queue after window = %d, want 1", len(svc.jobs))
	}

	// Nil receiver is a no-op.
	var nilSvc *SyncService
	nilSvc.MaybeEnqueueUser(uid)
	nilSvc.EnqueueAccountRefetch(uid, 1)
}

// TestMaybeEnqueueUserAPI: the read-triggered twin is off unless APIThrottle
// is set, then dedups on pending and on its own (shorter) gap.
func TestMaybeEnqueueUserAPI(t *testing.T) {
	svc, _, _, uid := setupSync(t)

	svc.MaybeEnqueueUserAPI(uid) // APIThrottle 0 -> trigger disabled
	if len(svc.jobs) != 0 {
		t.Fatalf("disabled API trigger enqueued: queue = %d", len(svc.jobs))
	}
	svc.Opts.APIThrottle = time.Minute
	svc.MaybeEnqueueUserAPI(uid)
	if len(svc.jobs) != 1 {
		t.Fatalf("queue = %d, want 1", len(svc.jobs))
	}
	svc.MaybeEnqueueUserAPI(uid) // pending -> dropped
	if len(svc.jobs) != 1 {
		t.Fatalf("queue after pending dup = %d, want 1", len(svc.jobs))
	}
	<-svc.jobs
	svc.mu.Lock()
	svc.pending[uid] = false
	svc.lastRun[uid] = time.Now()
	svc.mu.Unlock()
	svc.MaybeEnqueueUserAPI(uid) // inside the API gap -> dropped
	if len(svc.jobs) != 0 {
		t.Fatalf("queue inside API gap = %d, want 0", len(svc.jobs))
	}
	svc.mu.Lock()
	svc.lastRun[uid] = time.Now().Add(-61 * time.Second)
	svc.mu.Unlock()
	svc.MaybeEnqueueUserAPI(uid) // gap expired -> enqueued
	if len(svc.jobs) != 1 {
		t.Fatalf("queue after API gap = %d, want 1", len(svc.jobs))
	}
	var nilSvc *SyncService
	nilSvc.MaybeEnqueueUserAPI(uid) // no-op
}

// TestSyncUser_NoCredentials is a no-op, not an error.
func TestSyncUser_NoCredentials(t *testing.T) {
	svc, _, q, uid := setupSync(t)
	if err := q.DeleteTopstepxCredentials(context.Background(), uid); err != nil {
		t.Fatalf("delete creds: %v", err)
	}
	if err := svc.syncUser(context.Background(), syncJob{UserID: uid}); err != nil {
		t.Fatalf("syncUser without creds should be a no-op, got %v", err)
	}
}

// Guard: sql import used by the ErrNoRows path in the service.
var _ = sql.ErrNoRows
