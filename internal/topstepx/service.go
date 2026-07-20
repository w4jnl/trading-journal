package topstepx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
)

// apiClient is the slice of Client the sync service uses; an interface so
// tests can fake the API without HTTP.
type apiClient interface {
	LoginKey(ctx context.Context, userName, apiKey string) (string, error)
	Validate(ctx context.Context, token string) (string, error)
	SearchAccounts(ctx context.Context, token string, onlyActive bool) ([]Account, error)
	SearchTrades(ctx context.Context, token string, accountID int32, start, end time.Time) ([]HalfTrade, error)
	SearchContractByID(ctx context.Context, token, contractID string) (Contract, error)
}

const (
	syncType = "topstepx_api"

	// incrementalOverlap re-fetches this far behind the newest known fill so
	// late-reported trades and clock skew cannot open a gap; dedup absorbs it.
	incrementalOverlap = 24 * time.Hour
	// walkBackSeamOverlap overlaps adjacent walk-back windows at month seams.
	walkBackSeamOverlap = time.Hour
	// walkBackMaxMonths caps the full-history walk-back.
	walkBackMaxMonths = 24
	// walkBackEmptyStop ends the walk-back after this many consecutive empty
	// months (accounts are young; a 3-month silence means we are past its birth).
	walkBackEmptyStop = 3
	// walkBackPause is the polite delay between consecutive history requests.
	walkBackPause = 300 * time.Millisecond
)

// syncJob is one unit of work for the Run loop. AccountID 0 = sync every
// account of the user; non-zero = one account, Full (the re-fetch button).
type syncJob struct {
	UserID    int64
	AccountID int64
	Full      bool
}

// SyncService pulls TopstepX trade history through the ProjectX API and feeds
// it into the importer. One background worker (Run) drains a small queue fed
// by throttled page-visit hooks (MaybeEnqueueUser), the hourly scheduler
// (Schedule), and the manual per-account re-fetch button.
//
// A nil *SyncService is a valid no-op receiver on the enqueue methods, so
// handlers can call them unconditionally (mirrors marketdata.Prefetcher).
// SyncOptions bundles the cadence/throttle knobs from config.
type SyncOptions struct {
	Throttle     time.Duration // min gap between page-visit syncs per user; <=0 -> 15m
	SessionEvery time.Duration // scheduled cadence in the Globex window; 0 disables
	RTHEvery     time.Duration // faster cadence inside the RTH window; 0 disables
	RTHStartMin  int           // RTH window bounds, minutes-of-day Chicago
	RTHEndMin    int
}

type SyncService struct {
	Queries  *models.Queries
	Importer *importer.ImportService
	Client   apiClient
	Box      *SecretBox
	Logger   *slog.Logger
	Opts     SyncOptions

	chicago *time.Location // CME session clock for Schedule

	mu      sync.Mutex
	lastRun map[int64]time.Time
	pending map[int64]bool
	jobs    chan syncJob
}

// NewSyncService wires a ready-to-Run service.
func NewSyncService(q *models.Queries, imp *importer.ImportService, client apiClient, box *SecretBox, opts SyncOptions, logger *slog.Logger) *SyncService {
	if opts.Throttle <= 0 {
		opts.Throttle = 15 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		logger.Warn("topstepx: America/Chicago tz unavailable, scheduler uses UTC", "err", err)
		chicago = time.UTC
	}
	return &SyncService{
		Queries:  q,
		Importer: imp,
		Client:   client,
		Box:      box,
		Logger:   logger,
		Opts:     opts,
		chicago:  chicago,
		lastRun:  map[int64]time.Time{},
		pending:  map[int64]bool{},
		jobs:     make(chan syncJob, 64),
	}
}

// MaybeEnqueueUser triggers a background sync for the user unless one is
// already pending or ran within the throttle window. Never blocks; safe on a
// nil receiver (feature disabled).
func (s *SyncService) MaybeEnqueueUser(userID int64) {
	if s == nil {
		return
	}
	s.enqueueWithGap(userID, s.Opts.Throttle)
}

// enqueueWithGap enqueues a whole-user sync unless one is pending or the
// last one finished less than minGap ago. The page-visit hook uses the
// configured throttle; the scheduler passes half its cadence so a sync a
// user just triggered by browsing isn't pointlessly repeated seconds later.
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
		s.Logger.Warn("topstepx: sync queue full, dropping", "user_id", userID)
	}
}

// EnqueueAccountRefetch queues a full-history re-fetch for one account,
// bypassing the throttle (explicit user action). Safe on a nil receiver.
func (s *SyncService) EnqueueAccountRefetch(userID, accountID int64) {
	if s == nil || userID == 0 || accountID == 0 {
		return
	}
	select {
	case s.jobs <- syncJob{UserID: userID, AccountID: accountID, Full: true}:
	default:
		s.Logger.Warn("topstepx: sync queue full, dropping refetch", "account_id", accountID)
	}
}

// Run drains the queue until ctx is done. Jobs run serially — the importer's
// FIFO rebuild assumes one writer per (account, symbol) at a time.
func (s *SyncService) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.jobs:
			if err := s.syncUser(ctx, job); err != nil && !errors.Is(err, context.Canceled) {
				s.Logger.Error("topstepx sync failed", "user_id", job.UserID, "err", err)
			}
			if job.AccountID == 0 {
				s.mu.Lock()
				s.pending[job.UserID] = false
				s.lastRun[job.UserID] = time.Now()
				s.mu.Unlock()
			}
		}
	}
}

// TestCredentials verifies a userName/apiKey pair with a bare loginKey round
// trip and returns the session token it produced, so the settings handlers
// can persist it (StoreToken) instead of throwing it away - the verification
// login then doubles as the first sync's token.
func (s *SyncService) TestCredentials(ctx context.Context, userName, apiKey string) (string, error) {
	return s.Client.LoginKey(ctx, userName, apiKey)
}

// StoreToken persists a session token obtained outside the sync flow (the
// settings verification login). Best-effort from the caller's perspective -
// a failure just means the next sync logs in again.
func (s *SyncService) StoreToken(ctx context.Context, userID int64, token string) error {
	return s.persistToken(ctx, userID, token)
}

// token returns a working session token for the user. The happy path is
// pure DB: the token persisted on the credentials row (already loaded by the
// caller) is used as long as it stays comfortably clear of its JWT expiry -
// NO validate, NO login. A refresh only happens here when the background
// maintainer hasn't kept it warm (first sync ever, maintainer lagging, or an
// undecryptable token after a key rotation).
func (s *SyncService) token(ctx context.Context, creds models.TopstepxCredential, apiKey string) (string, error) {
	if stored, ok := s.storedToken(creds); ok {
		if time.Until(*creds.TokenExpiresAt) > tokenUseMargin {
			return stored, nil
		}
		// Too close to expiry to trust for a whole sync: refresh inline,
		// validating the still-live token first.
		return s.refreshStoredToken(ctx, creds, apiKey, stored)
	}
	return s.refreshStoredToken(ctx, creds, apiKey, "")
}

// storedToken decrypts the persisted session token; ok=false when absent or
// undecryptable (key rotation - treated as "no token", never fatal).
func (s *SyncService) storedToken(creds models.TopstepxCredential) (string, bool) {
	if len(creds.TokenEnc) == 0 || creds.TokenExpiresAt == nil {
		return "", false
	}
	tok, err := s.Box.Open(creds.TokenEnc, creds.TokenNonce)
	if err != nil {
		s.Logger.Warn("topstepx: stored token undecryptable, will re-login", "user_id", creds.UserID, "err", err)
		return "", false
	}
	return tok, true
}

// refreshStoredToken renews the user's session token and persists it:
// validate the stale token first (rolls the ~24h window); loginKey only when
// validate rejects it or there is nothing to validate - re-authentication is
// the last resort. Non-auth validate errors (network) propagate without
// burning a login attempt.
func (s *SyncService) refreshStoredToken(ctx context.Context, creds models.TopstepxCredential, apiKey, staleToken string) (string, error) {
	if staleToken != "" {
		rolled, err := s.Client.Validate(ctx, staleToken)
		switch {
		case err == nil:
			return rolled, s.persistToken(ctx, creds.UserID, rolled)
		case !errors.Is(err, ErrAuth):
			return "", err
		}
		s.Logger.Debug("topstepx: validate rejected session, re-authenticating", "user_id", creds.UserID)
	}
	tok, err := s.Client.LoginKey(ctx, creds.Username, apiKey)
	if err != nil {
		return "", err
	}
	return tok, s.persistToken(ctx, creds.UserID, tok)
}

// persistToken stores the encrypted token + its JWT expiry on the
// credentials row so restarts and the maintainer can reuse it.
func (s *SyncService) persistToken(ctx context.Context, userID int64, token string) error {
	enc, nonce, err := s.Box.Seal(token)
	if err != nil {
		return fmt.Errorf("encrypt token: %w", err)
	}
	exp := tokenExpiry(token)
	if err := s.Queries.SetTopstepxToken(ctx, models.SetTopstepxTokenParams{
		TokenEnc: enc, TokenNonce: nonce, TokenExpiresAt: &exp, UserID: userID,
	}); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}
	s.Logger.Debug("topstepx: session token persisted", "user_id", userID, "expires_at", exp)
	return nil
}

// MaintainTokens keeps every credentialed user's session token warm,
// independent of sync and schedule triggers: every tokenMaintainInterval it
// renews (validate-first) any token with less than tokenRefreshMargin of
// validity left, and obtains one where none is stored. This is the only
// place validate runs routinely - syncs just read the persisted token.
// Runs one pass immediately so a boot right before expiry still renews.
func (s *SyncService) MaintainTokens(ctx context.Context) {
	if s == nil {
		return
	}
	ticker := time.NewTicker(tokenMaintainInterval)
	defer ticker.Stop()
	s.maintainTokensPass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maintainTokensPass(ctx)
		}
	}
}

// maintainTokensPass renews/acquires tokens for all users that need it.
// Failures are slog-only (the pass reruns in 15 minutes; sync_log rows are
// reserved for actual sync outcomes).
func (s *SyncService) maintainTokensPass(ctx context.Context) {
	users, err := s.Queries.ListTopstepxSyncUsers(ctx)
	if err != nil {
		s.Logger.Error("topstepx: token maintainer: list users", "err", err)
		return
	}
	for _, uid := range users {
		creds, err := s.Queries.GetTopstepxCredentials(ctx, uid)
		if err != nil {
			continue // row vanished between list and get
		}
		stale, hasToken := s.storedToken(creds)
		if hasToken && time.Until(*creds.TokenExpiresAt) > tokenRefreshMargin {
			continue // comfortably valid - nothing to do
		}
		apiKey, err := s.Box.Open(creds.ApiKeyEnc, creds.ApiKeyNonce)
		if err != nil {
			s.Logger.Warn("topstepx: token maintainer: credentials undecryptable", "user_id", uid, "err", err)
			continue
		}
		if _, err := s.refreshStoredToken(ctx, creds, apiKey, stale); err != nil {
			s.Logger.Warn("topstepx: token maintainer: refresh failed", "user_id", uid, "err", err)
			continue
		}
		s.Logger.Debug("topstepx: token maintained", "user_id", uid, "had_token", hasToken)
	}
}

// contractCache memoizes contractId -> (symbol, root) for one sync run.
type contractCache struct {
	entries map[string][2]string
}

// resolve maps a ProjectX contractId to the local (symbol, root), parsing
// locally first and falling back to Contract/searchById.
func (s *SyncService) resolve(ctx context.Context, cache *contractCache, token, contractID string) (symbol, root string, err error) {
	if v, ok := cache.entries[contractID]; ok {
		return v[0], v[1], nil
	}
	symbol, root, ok := SymbolFromContractID(contractID)
	if !ok {
		contract, cerr := s.Client.SearchContractByID(ctx, token, contractID)
		if cerr != nil {
			return "", "", fmt.Errorf("resolve contract %s: %w", contractID, cerr)
		}
		symbol = strings.TrimSpace(contract.Name)
		if symbol == "" {
			return "", "", fmt.Errorf("resolve contract %s: empty name from API", contractID)
		}
		root = importer.DeriveSymbolRoot(symbol)
		s.Logger.Debug("topstepx contract resolved via API", "contract_id", contractID, "symbol", symbol)
	}
	cache.entries[contractID] = [2]string{symbol, root}
	return symbol, root, nil
}

// syncUser is one full sync pass for a user (or one account when
// job.AccountID is set). Per-account failures are logged to sync_log and do
// not stop the remaining accounts.
func (s *SyncService) syncUser(ctx context.Context, job syncJob) error {
	creds, err := s.Queries.GetTopstepxCredentials(ctx, job.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // credentials removed since enqueue - nothing to do
	}
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	apiKey, err := s.Box.Open(creds.ApiKeyEnc, creds.ApiKeyNonce)
	if err != nil {
		s.writeLog(ctx, job.UserID, nil, "error", 0, 0, "credentials: cannot decrypt; re-enter them in Settings")
		return fmt.Errorf("decrypt credentials: %w", err)
	}

	token, err := s.token(ctx, creds, apiKey)
	if err != nil {
		s.logAuthFailure(ctx, job.UserID, err)
		return fmt.Errorf("authenticate: %w", err)
	}

	apiAccounts, err := s.searchAccountsRetry(ctx, creds, apiKey, &token)
	if err != nil {
		s.logAuthFailure(ctx, job.UserID, err)
		return fmt.Errorf("search accounts: %w", err)
	}

	locals, err := s.Queries.ListAccounts(ctx, job.UserID)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	localByRef := map[string]models.Account{}
	for _, a := range locals {
		if a.ImportSource == string(importer.FormatTopstepX) {
			localByRef[a.AccountRef] = a
		}
	}

	cache := &contractCache{entries: map[string][2]string{}}
	now := time.Now().UTC()

	for _, apiAcct := range apiAccounts {
		local, known := localByRef[apiAcct.Name]
		if job.AccountID != 0 && (!known || local.ID != job.AccountID) {
			continue // re-fetch targets exactly one account
		}
		if known && local.Active == 0 {
			s.Logger.Debug("topstepx: account inactive, skipped", "ref", apiAcct.Name)
			continue
		}
		if known {
			claim, err := s.Queries.GetAccountImportClaim(ctx, models.GetAccountImportClaimParams{
				ID: local.ID, UserID: job.UserID,
			})
			if err != nil {
				s.writeLog(ctx, job.UserID, &local.ID, "error", 0, 0, "ingest: read import claim: "+err.Error())
				continue
			}
			if claim == "trades" {
				s.writeLog(ctx, job.UserID, &local.ID, "skipped", 0, 0,
					"account is trades-CSV (inline) imported; API sync disabled for it")
				continue
			}
		}
		if err := s.syncAccount(ctx, job, apiAcct, local, known, cache, creds, apiKey, &token, now); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			s.Logger.Error("topstepx account sync failed", "ref", apiAcct.Name, "err", err)
		}
	}
	return nil
}

// syncAccount fetches, aggregates, and ingests one account's window(s), then
// writes its sync_log row.
func (s *SyncService) syncAccount(ctx context.Context, job syncJob, apiAcct Account, local models.Account, known bool, cache *contractCache, creds models.TopstepxCredential, apiKey string, token *string, now time.Time) error {
	full := job.Full || !known || local.ApiBackfillDone == 0
	var lastExec time.Time
	if known && !job.Full {
		var err error
		lastExec, err = s.Queries.GetLastExecTimeForAccount(ctx, local.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			full = true
		case err != nil:
			return fmt.Errorf("last exec time: %w", err)
		default:
			if !full {
				// Account is considered up to date until its newest known
				// fill; pull only since then (minus overlap).
			}
		}
	}

	var halves []HalfTrade
	var windowDesc string
	if !full {
		start := lastExec.Add(-incrementalOverlap)
		got, err := s.searchTradesRetry(ctx, creds, apiKey, token, apiAcct.ID, start, now)
		if err != nil {
			s.logAccountError(ctx, job.UserID, known, local.ID, "network: "+err.Error())
			return err
		}
		halves = got
		windowDesc = fmt.Sprintf("window %s..now", start.Format("2006-01-02T15:04Z"))
	} else {
		// Month-by-month walk-back from now until the account's history runs
		// dry (walkBackEmptyStop consecutive empty months) or the cap.
		emptyStreak := 0
		months := 0
		for i := 0; i < walkBackMaxMonths; i++ {
			end := now.AddDate(0, -i, 0)
			start := now.AddDate(0, -(i + 1), 0).Add(-walkBackSeamOverlap)
			got, err := s.searchTradesRetry(ctx, creds, apiKey, token, apiAcct.ID, start, end)
			if err != nil {
				s.logAccountError(ctx, job.UserID, known, local.ID, "network: "+err.Error())
				return err
			}
			months++
			nonVoided := 0
			for _, ht := range got {
				if !ht.Voided {
					nonVoided++
				}
			}
			halves = append(halves, got...)
			if nonVoided == 0 {
				emptyStreak++
				if emptyStreak >= walkBackEmptyStop {
					break
				}
			} else {
				emptyStreak = 0
			}
			if err := sleepCtx(ctx, walkBackPause); err != nil {
				return err
			}
		}
		windowDesc = fmt.Sprintf("walk-back %d months", months)
	}

	orderFills := AggregateOrders(halves, s.Logger)

	fills := make([]importer.Fill, 0, len(orderFills))
	contractErrs := 0
	for _, of := range orderFills {
		symbol, root, err := s.resolve(ctx, cache, *token, of.ContractID)
		if err != nil {
			s.Logger.Warn("topstepx: contract unresolved, order skipped",
				"contract_id", of.ContractID, "order", of.BrokerExecID, "err", err)
			contractErrs++
			continue
		}
		comm := of.Commission
		fills = append(fills, importer.Fill{
			AccountRef:   apiAcct.Name,
			BrokerExecID: of.BrokerExecID,
			Symbol:       symbol,
			SymbolRoot:   root,
			Side:         of.Side,
			Qty:          of.Qty,
			Price:        of.Price,
			Commission:   &comm,
			ExecTime:     of.ExecTime.UTC(),
			RawData:      of.RawData,
		})
	}

	if len(fills) == 0 && !known {
		// Never-traded API account: deliberately create no local row.
		s.Logger.Debug("topstepx: account has no history, nothing created", "ref", apiAcct.Name)
		return nil
	}

	var execsAdded, tradesAdded int64
	if len(fills) > 0 {
		out, err := s.Importer.ImportAPIFills(ctx, job.UserID, "", fills)
		if err != nil {
			s.logAccountError(ctx, job.UserID, known, local.ID, "ingest: "+err.Error())
			return err
		}
		execsAdded, tradesAdded = int64(out.ExecsInserted), int64(out.TradesCreated)
	}

	// Locate the (possibly just-created) account row for logging + the
	// backfill flag.
	acct, err := s.Queries.GetAccountByRef(ctx, models.GetAccountByRefParams{
		UserID: job.UserID, ImportSource: string(importer.FormatTopstepX), AccountRef: apiAcct.Name,
	})
	if err != nil {
		return fmt.Errorf("account row after ingest: %w", err)
	}
	if full {
		if err := s.Queries.SetAccountAPIBackfillDone(ctx, models.SetAccountAPIBackfillDoneParams{
			ApiBackfillDone: 1, ID: acct.ID, UserID: job.UserID,
		}); err != nil {
			return fmt.Errorf("mark backfill done: %w", err)
		}
	}

	msg := fmt.Sprintf("%s, %d orders, %d new fills", windowDesc, len(orderFills), execsAdded)
	if contractErrs > 0 {
		msg = fmt.Sprintf("contract: %d unresolved orders skipped; %s", contractErrs, msg)
	}
	s.writeLog(ctx, job.UserID, &acct.ID, "success", tradesAdded, execsAdded, msg)
	return nil
}

// searchAccountsRetry runs SearchAccounts with one validate-then-relogin
// retry on ErrAuth (token can expire mid-lifecycle).
func (s *SyncService) searchAccountsRetry(ctx context.Context, creds models.TopstepxCredential, apiKey string, token *string) ([]Account, error) {
	accounts, err := s.Client.SearchAccounts(ctx, *token, true)
	if errors.Is(err, ErrAuth) {
		if err = s.recoverToken(ctx, creds, apiKey, token); err != nil {
			return nil, err
		}
		accounts, err = s.Client.SearchAccounts(ctx, *token, true)
	}
	return accounts, err
}

// searchTradesRetry is the same auth-retry wrapper for SearchTrades.
func (s *SyncService) searchTradesRetry(ctx context.Context, creds models.TopstepxCredential, apiKey string, token *string, accountID int32, start, end time.Time) ([]HalfTrade, error) {
	trades, err := s.Client.SearchTrades(ctx, *token, accountID, start, end)
	if errors.Is(err, ErrAuth) {
		if err = s.recoverToken(ctx, creds, apiKey, token); err != nil {
			return nil, err
		}
		trades, err = s.Client.SearchTrades(ctx, *token, accountID, start, end)
	}
	return trades, err
}

// recoverToken handles a mid-sync 401: the persisted token is cleared, then
// the standard validate-first refresh runs with the rejected token as the
// stale candidate (validate stays the first move; loginKey the fallback).
func (s *SyncService) recoverToken(ctx context.Context, creds models.TopstepxCredential, apiKey string, token *string) error {
	if err := s.Queries.ClearTopstepxToken(ctx, creds.UserID); err != nil {
		s.Logger.Warn("topstepx: clear rejected token", "user_id", creds.UserID, "err", err)
	}
	fresh, err := s.refreshStoredToken(ctx, creds, apiKey, *token)
	if err != nil {
		return err
	}
	*token = fresh
	return nil
}

// logAuthFailure writes the single user-level auth error row (account_id
// NULL) so the settings page can surface "credentials invalid".
func (s *SyncService) logAuthFailure(ctx context.Context, userID int64, err error) {
	label := "auth: "
	if errors.Is(err, ErrSubscription) {
		label = "subscription: "
	}
	s.writeLog(ctx, userID, nil, "error", 0, 0, label+err.Error())
}

func (s *SyncService) logAccountError(ctx context.Context, userID int64, known bool, accountID int64, msg string) {
	var idPtr *int64
	if known {
		idPtr = &accountID
	}
	s.writeLog(ctx, userID, idPtr, "error", 0, 0, msg)
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
		s.Logger.Error("topstepx: write sync_log", "err", err)
	}
}

// sleepCtx sleeps for d or until ctx is done (mirrors marketdata's helper).
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
