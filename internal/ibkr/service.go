package ibkr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

const syncType = "ibkr_api"

// tradesWindowDays is the CP trades endpoint's rolling window (its hard max).
const tradesWindowDays = 7

// gatewayClient is the slice of Client the service uses; an interface so
// tests can fake a gateway without HTTP.
type gatewayClient interface {
	Status(ctx context.Context) (AuthStatus, error)
	Tickle(ctx context.Context) error
	SsodhInit(ctx context.Context) (SsodhInitResult, error)
	Reauthenticate(ctx context.Context) error
	Accounts(ctx context.Context) ([]GatewayAccount, error)
	Trades(ctx context.Context, days int) ([]Trade, error)
	ContractInfo(ctx context.Context, conid int64) (Contract, error)
}

// SyncOptions bundles the global knobs from config (gateways themselves live
// in the DB, managed per user via settings). Scheduling mirrors TopstepX: a
// page-visit throttle plus a two-tier cadence gated to CME Globex hours.
type SyncOptions struct {
	Throttle     time.Duration // min gap between page-visit syncs per user; <=0 -> 15m
	APIThrottle  time.Duration // min gap between API-read-triggered syncs per user; <=0 disables that trigger
	SessionEvery time.Duration // scheduled cadence during Globex hours; 0 disables
	RTHEvery     time.Duration // faster cadence inside the RTH window; 0 disables
	RTHStartMin  int           // RTH window bounds, minutes-of-day Chicago
	RTHEndMin    int
	TickleEvery  time.Duration // keep-alive cadence per gateway; 0 disables
}

// GatewayState is one gateway's runtime status for the settings card.
// Kept in memory only - polls re-establish it within minutes of a boot.
type GatewayState struct {
	State    string // "connected" | "needs-login" | "unreachable" | "unknown"
	LastPoll time.Time
	LastErr  string
	Accounts []string
}

type syncJob struct{ UserID int64 }

// SyncService polls the user-configured Client Portal Gateways for executed
// trades and feeds them through the importer. It never logs in: gateway
// sessions are established by the user's browser; the service tickles them
// alive, reauthenticates dropped brokerage sessions, and surfaces
// "needs-login" when only the user can fix it.
//
// A nil *SyncService is a valid no-op receiver on the enqueue methods.
type SyncService struct {
	Queries  *models.Queries
	Importer *importer.ImportService
	Logger   *slog.Logger
	Opts     SyncOptions
	// NewClient builds a client for one gateway; tests inject fakes.
	NewClient func(url string, insecure bool) gatewayClient

	chicago *time.Location // CME session clock for Schedule

	mu      sync.Mutex
	lastRun map[int64]time.Time
	pending map[int64]bool
	states  map[int64]*GatewayState // by gateway id
	clients map[int64]cachedClient  // by gateway id (client caches its transport)
	jobs    chan syncJob
}

// cachedClient remembers the config a client was built from so the cache
// invalidates when it changes. Guarding on the id alone is not enough:
// SQLite reuses row ids, so delete-gateway + add-gateway can hand a NEW url
// the OLD id - the cached client would silently keep calling the old
// address while the UI shows the new one.
type cachedClient struct {
	url      string
	insecure bool
	client   gatewayClient
}

// NewSyncService wires a ready-to-Run service.
func NewSyncService(q *models.Queries, imp *importer.ImportService, opts SyncOptions, logger *slog.Logger) *SyncService {
	if opts.Throttle <= 0 {
		opts.Throttle = 15 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		logger.Warn("ibkr: America/Chicago tz unavailable, scheduler uses UTC", "err", err)
		chicago = time.UTC
	}
	return &SyncService{
		Queries:  q,
		Importer: imp,
		Logger:   logger,
		Opts:     opts,
		chicago:  chicago,
		NewClient: func(url string, insecure bool) gatewayClient {
			return &Client{BaseURL: url, Insecure: insecure, Logger: logger}
		},
		lastRun: map[int64]time.Time{},
		pending: map[int64]bool{},
		states:  map[int64]*GatewayState{},
		clients: map[int64]cachedClient{},
		jobs:    make(chan syncJob, 64),
	}
}

// MaybeEnqueueUser triggers a background poll of the user's gateways unless
// one is pending or ran within the throttle window. Nil-safe, never blocks.
func (s *SyncService) MaybeEnqueueUser(userID int64) {
	if s == nil {
		return
	}
	s.enqueueWithGap(userID, s.Opts.Throttle)
}

// MaybeEnqueueUserAPI mirrors topstepx: the read-triggered sync for the JSON
// API / MCP tools, bounded by its own gap (Opts.APIThrottle; <=0 disables)
// so a fast-polling consumer cannot hammer the gateway. Nil-safe.
func (s *SyncService) MaybeEnqueueUserAPI(userID int64) {
	if s == nil || s.Opts.APIThrottle <= 0 {
		return
	}
	s.enqueueWithGap(userID, s.Opts.APIThrottle)
}

func (s *SyncService) enqueueWithGap(userID int64, minGap time.Duration) {
	if userID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[userID] || time.Since(s.lastRun[userID]) < minGap {
		return
	}
	select {
	case s.jobs <- syncJob{UserID: userID}:
		s.pending[userID] = true
	default:
		s.Logger.Warn("ibkr: sync queue full, dropping", "user_id", userID)
	}
}

// Run drains the queue until ctx is done (serial - the FIFO rebuild assumes
// one writer per (account, symbol) at a time).
func (s *SyncService) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			if err := s.syncUser(ctx, job.UserID); err != nil && !errors.Is(err, context.Canceled) {
				s.Logger.Error("ibkr sync failed", "user_id", job.UserID, "err", err)
			}
			s.mu.Lock()
			s.pending[job.UserID] = false
			s.lastRun[job.UserID] = time.Now()
			s.mu.Unlock()
		}
	}
}

// PendingForUser reports whether a sync is currently queued or running for
// the user (drives the "syncing..." view banner). Nil-safe.
func (s *SyncService) PendingForUser(userID int64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[userID]
}

// WaitForUser mirrors topstepx: block until the user's sync is idle, the
// deadline passes, or ctx ends (the refresh read's wait). Nil-safe.
func (s *SyncService) WaitForUser(ctx context.Context, userID int64, deadline time.Time) {
	if s == nil {
		return
	}
	for s.PendingForUser(userID) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// KeepAlive tickles every enabled gateway so their sessions do not idle out
// between polls; a dropped brokerage session is reauthenticated on the spot.
func (s *SyncService) KeepAlive(ctx context.Context) {
	if s == nil || s.Opts.TickleEvery <= 0 {
		return
	}
	ticker := time.NewTicker(s.Opts.TickleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		gws, err := s.Queries.ListEnabledIbkrGateways(ctx)
		if err != nil {
			s.Logger.Error("ibkr: list gateways", "err", err)
			continue
		}
		for _, gw := range gws {
			client := s.clientFor(gw)
			if err := client.Tickle(ctx); err != nil && !errors.Is(err, ErrNotAuthenticated) {
				s.setGatewayState(ctx, gw, "unreachable", err.Error())
				continue
			}
			// Tickle succeeds with HTTP 200 even when the brokerage session
			// has dropped (the state lives in the body), so always verify via
			// auth/status - ensureAuthenticated reauthenticates a dropped
			// session and transition-logs needs-login when only the user can
			// fix it.
			s.ensureAuthenticated(ctx, gw, client)
		}
	}
}

// GatewayStates returns the runtime status of a user's gateways for the
// settings card, keyed by gateway id. Nil-safe.
func (s *SyncService) GatewayStates(userID int64) map[int64]GatewayState {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int64]GatewayState{}
	for id, st := range s.states {
		out[id] = *st
	}
	return out
}

// TestGateway probes the gateway for the settings "Test" button, applying
// the same silent session recovery the sync uses (ssodh/init, then legacy
// reauthenticate) - a bare status check would report "not authenticated"
// for a session the app is perfectly able to re-establish itself.
func (s *SyncService) TestGateway(ctx context.Context, gw models.IbkrGateway) (AuthStatus, error) {
	client := s.clientFor(gw)
	st, err := client.Status(ctx)
	if err == nil && st.Authenticated {
		s.touchGateway(gw.ID, "connected", "")
		return st, nil
	}
	if err != nil && !errors.Is(err, ErrNotAuthenticated) {
		return AuthStatus{}, err // genuinely unreachable
	}
	if s.ensureAuthenticated(ctx, gw, client) {
		return client.Status(ctx)
	}
	return AuthStatus{}, nil // responds, but only a manual browser login can fix it
}

func (s *SyncService) clientFor(gw models.IbkrGateway) gatewayClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	insecure := gw.TlsInsecure == 1
	if c, ok := s.clients[gw.ID]; ok && c.url == gw.Url && c.insecure == insecure {
		return c.client
	}
	c := s.NewClient(gw.Url, insecure)
	s.clients[gw.ID] = cachedClient{url: gw.Url, insecure: insecure, client: c}
	// The runtime state belongs to the previous gateway that carried this id
	// (or a previous URL) - reset it so a stale "unreachable" doesn't stick
	// to the fresh config.
	delete(s.states, gw.ID)
	return c
}

// touchGateway updates the in-memory state without transition logging.
func (s *SyncService) touchGateway(gatewayID int64, state, errMsg string) *GatewayState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[gatewayID]
	if !ok {
		st = &GatewayState{}
		s.states[gatewayID] = st
	}
	st.State = state
	st.LastErr = errMsg
	return st
}

// setGatewayState updates state and, on a TRANSITION into a bad state,
// writes one sync_log error row (not one per tick - outages would spam).
func (s *SyncService) setGatewayState(ctx context.Context, gw models.IbkrGateway, state, errMsg string) {
	s.mu.Lock()
	prev := ""
	if st, ok := s.states[gw.ID]; ok {
		prev = st.State
	}
	s.mu.Unlock()
	s.touchGateway(gw.ID, state, errMsg)
	if state != prev && (state == "needs-login" || state == "unreachable") {
		msg := fmt.Sprintf("gateway %s (%s): %s", gw.Name, state, errMsg)
		if state == "needs-login" {
			msg = fmt.Sprintf("gateway %s needs a manual login: open %s in a browser and authenticate", gw.Name, gw.Url)
		}
		s.writeLog(ctx, gw.UserID, nil, "error", 0, 0, msg)
		s.Logger.Warn("ibkr gateway state", "gateway", gw.Name, "state", state, "err", errMsg)
	}
}

// ensureAuthenticated verifies the brokerage session and silently restores
// it while the gateway's SSO session lives: ssodh/init first (the effective
// mechanism on modern gateway builds - verified against 10.46), plain
// reauthenticate as the legacy fallback. Returns false when the gateway
// needs a manual browser login or is unreachable.
func (s *SyncService) ensureAuthenticated(ctx context.Context, gw models.IbkrGateway, client gatewayClient) bool {
	st, err := client.Status(ctx)
	if err == nil && st.Authenticated {
		s.touchGateway(gw.ID, "connected", "")
		return true
	}
	if err != nil && !errors.Is(err, ErrNotAuthenticated) {
		s.setGatewayState(ctx, gw, "unreachable", err.Error())
		return false
	}
	// Brokerage session down: re-establish it on top of the SSO session.
	if res, err := client.SsodhInit(ctx); err == nil && res.Authenticated {
		s.Logger.Debug("ibkr gateway brokerage session re-established (ssodh/init)", "gateway", gw.Name)
		s.touchGateway(gw.ID, "connected", "")
		return true
	}
	if err := client.Reauthenticate(ctx); err == nil {
		if st, err := client.Status(ctx); err == nil && st.Authenticated {
			s.Logger.Debug("ibkr gateway reauthenticated", "gateway", gw.Name)
			s.touchGateway(gw.ID, "connected", "")
			return true
		}
	}
	s.setGatewayState(ctx, gw, "needs-login", "session expired")
	return false
}

// syncUser polls each of the user's enabled gateways. Per-gateway failures
// don't stop the rest.
func (s *SyncService) syncUser(ctx context.Context, userID int64) error {
	gws, err := s.Queries.ListIbkrGatewaysForUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("list gateways: %w", err)
	}
	for _, gw := range gws {
		if gw.Enabled == 0 {
			continue
		}
		if err := s.syncGateway(ctx, gw); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.Logger.Error("ibkr gateway sync failed", "gateway", gw.Name, "err", err)
		}
	}
	return nil
}

// syncGateway pulls the gateway's rolling trade window and ingests each
// account's fills at/after its live-sync cutover.
func (s *SyncService) syncGateway(ctx context.Context, gw models.IbkrGateway) error {
	client := s.clientFor(gw)
	if !s.ensureAuthenticated(ctx, gw, client) {
		return nil // state + log already recorded
	}

	gwAccounts, err := client.Accounts(ctx)
	if err != nil {
		s.setGatewayState(ctx, gw, stateForErr(err), err.Error())
		return fmt.Errorf("accounts: %w", err)
	}
	trades, err := client.Trades(ctx, tradesWindowDays)
	if err != nil {
		s.setGatewayState(ctx, gw, stateForErr(err), err.Error())
		return fmt.Errorf("trades: %w", err)
	}

	byAccount := map[string][]Trade{}
	for _, t := range trades {
		byAccount[t.Account] = append(byAccount[t.Account], t)
	}

	// contractInfo resolver, cached for this gateway sync.
	resolved := map[int64]string{}
	resolve := func(conid int64) (string, error) {
		if sym, ok := resolved[conid]; ok {
			return sym, nil
		}
		ct, err := client.ContractInfo(ctx, conid)
		if err != nil {
			return "", err
		}
		sym := ct.LocalSymbol
		if sym == "" {
			sym = ct.Symbol
		}
		resolved[conid] = sym
		return sym, nil
	}

	names := make([]string, 0, len(gwAccounts))
	for _, ga := range gwAccounts {
		// "All" is IBKR's aggregate pseudo-account shown to multi-account
		// logins (the "combined view" entry of the account switcher). It is
		// not a real account and trades always carry concrete account ids,
		// so it is skipped rather than treated as an account ref.
		if ga.ID == "All" {
			continue
		}
		names = append(names, ga.ID)
		if err := s.syncAccount(ctx, gw, ga.ID, byAccount[ga.ID], resolve); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.Logger.Error("ibkr account sync failed", "gateway", gw.Name, "ref", ga.ID, "err", err)
		}
	}

	s.mu.Lock()
	if st, ok := s.states[gw.ID]; ok {
		st.LastPoll = time.Now()
		st.Accounts = names
	}
	s.mu.Unlock()
	return nil
}

func stateForErr(err error) string {
	if errors.Is(err, ErrNotAuthenticated) {
		return "needs-login"
	}
	return "unreachable"
}

// syncAccount ingests one account's window. The cutover rule:
//   - existing cutover -> filter fills before it (CSV era);
//   - no cutover yet -> compute max(newest known fill + 1ms, now - 7d):
//     everything the CSV already delivered stays CSV-owned, the API takes
//     over from the newest known fill (or its whole visible window for a
//     fresh account), and the gap the API cannot see remains CSV territory.
func (s *SyncService) syncAccount(ctx context.Context, gw models.IbkrGateway, ref string, trades []Trade, resolve func(int64) (string, error)) error {
	acct, err := s.Queries.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: gw.UserID, ImportSource: string(importer.FormatMotiveWave), AccountRef: ref,
	})
	known := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("account lookup: %w", err)
	}
	if known && acct.Active == 0 {
		s.Logger.Debug("ibkr: account inactive, skipped", "ref", ref)
		return nil
	}

	var cutover time.Time
	newCutover := false
	switch {
	case known && acct.LiveSyncSince != nil:
		cutover = *acct.LiveSyncSince
	default:
		cutover = time.Now().UTC().Add(-tradesWindowDays * 24 * time.Hour)
		if known {
			if lastExec, err := s.Queries.GetLastExecTimeForAccount(ctx, acct.ID); err == nil {
				if c := lastExec.Add(time.Millisecond); c.After(cutover) {
					cutover = c
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("last exec time: %w", err)
			}
		}
		newCutover = true
	}

	var fills []importer.Fill
	mapErrs := 0
	for _, t := range trades {
		fill, err := MapTrade(t, resolve)
		if err != nil {
			s.Logger.Warn("ibkr: trade skipped", "gateway", gw.Name, "err", err)
			mapErrs++
			continue
		}
		if fill.ExecTime.Before(cutover) {
			continue // CSV-owned era
		}
		fills = append(fills, fill)
	}

	// One line per account per poll so the -d log shows exactly what each
	// gateway account contributed and why (windowed executions vs fills that
	// survived the cutover filter).
	s.Logger.Debug("ibkr account window", "gateway", gw.Name, "ref", ref,
		"known", known, "cutover", cutover.Format(time.RFC3339),
		"trades_in_window", len(trades), "fills_after_cutover", len(fills), "unmappable", mapErrs)

	if len(fills) == 0 && !known {
		// Fresh account with nothing visible: create no rows yet.
		return nil
	}

	var execsAdded, tradesAdded int64
	if len(fills) > 0 {
		out, err := s.Importer.ImportAPIFills(ctx, gw.UserID, "", fills, importer.FormatMotiveWave)
		if err != nil {
			accountIDPtr := (*int64)(nil)
			if known {
				accountIDPtr = &acct.ID
			}
			s.writeLog(ctx, gw.UserID, accountIDPtr, "error", 0, 0, "ingest: "+err.Error())
			return err
		}
		execsAdded, tradesAdded = int64(out.ExecsInserted), int64(out.TradesCreated)
	}

	// (Re)load the possibly just-created account row for the cutover write
	// and the log row.
	acct, err = s.Queries.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: gw.UserID, ImportSource: string(importer.FormatMotiveWave), AccountRef: ref,
	})
	if err != nil {
		return fmt.Errorf("account row after ingest: %w", err)
	}
	if newCutover {
		if err := s.Queries.SetAccountLiveSyncSince(ctx, models.SetAccountLiveSyncSinceParams{
			LiveSyncSince: &cutover, ID: acct.ID, UserID: gw.UserID,
		}); err != nil {
			return fmt.Errorf("set cutover: %w", err)
		}
		s.Logger.Info("ibkr: live sync enabled for account", "ref", ref, "cutover", cutover)
	}

	// Only log a success row when the poll actually did something worth
	// recording - new fills, the initial cutover, or a mapping problem.
	// Otherwise a scheduled poll every session interval (or a page-visit
	// sync) would append a "0 new fills" row each time and drown the sync
	// log. The gateway's in-memory LastPoll (set by syncGateway) still
	// reflects every poll for the settings-card freshness indicator.
	if execsAdded == 0 && !newCutover && mapErrs == 0 {
		return nil
	}
	msg := fmt.Sprintf("gateway %s, window %dd, %d executions, %d new fills", gw.Name, tradesWindowDays, len(trades), execsAdded)
	if newCutover {
		msg += fmt.Sprintf("; live-sync cutover set to %s", cutover.Format(time.RFC3339))
	}
	if mapErrs > 0 {
		msg = fmt.Sprintf("contract: %d unmappable executions skipped; %s", mapErrs, msg)
	}
	s.writeLog(ctx, gw.UserID, &acct.ID, "success", tradesAdded, execsAdded, msg)
	return nil
}

func (s *SyncService) writeLog(ctx context.Context, userID int64, accountID *int64, status string, trades, execs int64, msg string) {
	if _, err := s.Queries.CreateSyncLog(ctx, models.CreateSyncLogParams{
		UserID:      &userID,
		AccountID:   accountID,
		SyncType:    syncType,
		Status:      status,
		TradesAdded: trades,
		ExecsAdded:  execs,
		Message:     &msg,
	}); err != nil {
		s.Logger.Error("ibkr: write sync_log", "err", err)
	}
}
