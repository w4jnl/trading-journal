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
type SyncService struct {
	Queries  *models.Queries
	Importer *importer.ImportService
	Client   apiClient
	Box      *SecretBox
	Logger   *slog.Logger
	Throttle time.Duration // min gap between page-visit-triggered syncs per user

	chicago *time.Location // CME session clock for Schedule

	mu      sync.Mutex
	lastRun map[int64]time.Time
	pending map[int64]bool
	tokens  map[int64]string
	jobs    chan syncJob
}

// NewSyncService wires a ready-to-Run service. throttle <= 0 falls back to
// 15 minutes.
func NewSyncService(q *models.Queries, imp *importer.ImportService, client apiClient, box *SecretBox, throttle time.Duration, logger *slog.Logger) *SyncService {
	if throttle <= 0 {
		throttle = 15 * time.Minute
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
		Throttle: throttle,
		chicago:  chicago,
		lastRun:  map[int64]time.Time{},
		pending:  map[int64]bool{},
		tokens:   map[int64]string{},
		jobs:     make(chan syncJob, 64),
	}
}

// MaybeEnqueueUser triggers a background sync for the user unless one is
// already pending or ran within the throttle window. Never blocks; safe on a
// nil receiver (feature disabled).
func (s *SyncService) MaybeEnqueueUser(userID int64) {
	if s == nil || userID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[userID] || time.Since(s.lastRun[userID]) < s.Throttle {
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
// trip. Used by the settings save/test actions.
func (s *SyncService) TestCredentials(ctx context.Context, userName, apiKey string) error {
	_, err := s.Client.LoginKey(ctx, userName, apiKey)
	return err
}

// token returns a working session token for the user, preferring to refresh
// the cached one via /api/Auth/validate; loginKey is the fallback, never the
// first move. Callers must hold no lock.
func (s *SyncService) token(ctx context.Context, userID int64, userName, apiKey string) (string, error) {
	s.mu.Lock()
	cached := s.tokens[userID]
	s.mu.Unlock()

	if cached != "" {
		if rolled, err := s.Client.Validate(ctx, cached); err == nil {
			s.setToken(userID, rolled)
			return rolled, nil
		} else if !errors.Is(err, ErrAuth) {
			// Network/API trouble - don't burn a login attempt on it.
			return "", err
		}
		// Validate rejected the session: fall through to a fresh login.
	}
	tok, err := s.Client.LoginKey(ctx, userName, apiKey)
	if err != nil {
		return "", err
	}
	s.setToken(userID, tok)
	return tok, nil
}

func (s *SyncService) setToken(userID int64, tok string) {
	s.mu.Lock()
	s.tokens[userID] = tok
	s.mu.Unlock()
}

func (s *SyncService) dropToken(userID int64) {
	s.mu.Lock()
	delete(s.tokens, userID)
	s.mu.Unlock()
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

	token, err := s.token(ctx, job.UserID, creds.Username, apiKey)
	if err != nil {
		s.logAuthFailure(ctx, job.UserID, err)
		return fmt.Errorf("authenticate: %w", err)
	}

	apiAccounts, err := s.searchAccountsRetry(ctx, job.UserID, creds.Username, apiKey, &token)
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
		if err := s.syncAccount(ctx, job, apiAcct, local, known, cache, creds.Username, apiKey, &token, now); err != nil {
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
func (s *SyncService) syncAccount(ctx context.Context, job syncJob, apiAcct Account, local models.Account, known bool, cache *contractCache, userName, apiKey string, token *string, now time.Time) error {
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
		got, err := s.searchTradesRetry(ctx, job.UserID, userName, apiKey, token, apiAcct.ID, start, now)
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
			got, err := s.searchTradesRetry(ctx, job.UserID, userName, apiKey, token, apiAcct.ID, start, end)
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
func (s *SyncService) searchAccountsRetry(ctx context.Context, userID int64, userName, apiKey string, token *string) ([]Account, error) {
	accounts, err := s.Client.SearchAccounts(ctx, *token, true)
	if errors.Is(err, ErrAuth) {
		if err = s.refreshToken(ctx, userID, userName, apiKey, token); err != nil {
			return nil, err
		}
		accounts, err = s.Client.SearchAccounts(ctx, *token, true)
	}
	return accounts, err
}

// searchTradesRetry is the same auth-retry wrapper for SearchTrades.
func (s *SyncService) searchTradesRetry(ctx context.Context, userID int64, userName, apiKey string, token *string, accountID int32, start, end time.Time) ([]HalfTrade, error) {
	trades, err := s.Client.SearchTrades(ctx, *token, accountID, start, end)
	if errors.Is(err, ErrAuth) {
		if err = s.refreshToken(ctx, userID, userName, apiKey, token); err != nil {
			return nil, err
		}
		trades, err = s.Client.SearchTrades(ctx, *token, accountID, start, end)
	}
	return trades, err
}

// refreshToken drops the cached token and reruns the validate-first
// lifecycle (which now goes straight to loginKey).
func (s *SyncService) refreshToken(ctx context.Context, userID int64, userName, apiKey string, token *string) error {
	s.dropToken(userID)
	fresh, err := s.token(ctx, userID, userName, apiKey)
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
