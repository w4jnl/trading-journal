package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/backup"
	"github.com/w4jnl/trading-journal/internal/config"
	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/handlers"
	"github.com/w4jnl/trading-journal/internal/ibkr"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/mwmail"
	"github.com/w4jnl/trading-journal/internal/topstepx"
	"github.com/w4jnl/trading-journal/web"
)

const topHelp = `Usage: trading-journal [flags]
       trading-journal users <subcommand> [args]
       trading-journal mwmail parse <file.eml>

Run the server (default):
  -config PATH   Path to config.yaml (default: ./config.yaml).
                 If the file is missing the server boots with built-in
                 defaults; see config.example.yaml for the full schema.
  -d             Verbose debug logging: api/mcp requests and AI tool calls.

User management:
  trading-journal users -h
                 Print user-management subcommands (add, reset, delete,
                 list, activate, deactivate, promote, demote).

MotiveWave email capture:
  trading-journal mwmail parse <file.eml>
                 Run the provisional fill parser on an archived message
                 (from mw_email.archive_dir) and print the extraction.
`

func main() {
	// Top-level -h / --help / help shortcut before flag parsing so users
	// don't have to know which subcommand to run first.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			fmt.Print(topHelp)
			return
		}
	}

	// Subcommands run before flag parsing so ` + "`./trading-journal users add ...`" + `
	// doesn't try to interpret subcommand args as global flags.
	if len(os.Args) >= 2 && os.Args[1] == "users" {
		// Allow `--config` to appear anywhere in the subcommand args.
		cfgPath := "config.yaml"
		var rest []string
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "--config" && i+1 < len(args):
				cfgPath = args[i+1]
				i++
			case len(a) > len("--config=") && a[:len("--config=")] == "--config=":
				cfgPath = a[len("--config="):]
			default:
				rest = append(rest, a)
			}
		}
		if err := runUsersCommand(rest, cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if len(os.Args) >= 2 && os.Args[1] == "mwmail" {
		if err := runMWMailCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// statusRecorder captures the response status for debug logging. It forwards
// Flush so the MCP streamable-HTTP (SSE) transport keeps working when wrapped.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// contentSecurityPolicy is deliberately permissive on inline script/style
// ('unsafe-inline') because base.html carries several inline <script>/<style>
// blocks and an inline chart-data blob; the primary XSS control is server-side
// sanitization of rendered markdown (see handlers.renderMarkdown). Scripts and
// styles load from 'self' only - htmx, uPlot, and lightweight-charts are
// vendored under web/static/vendor/ (embedded in the binary), so no CDN hosts
// are needed and a CDN outage can't take the app down. img-src allows https:
// because journal markdown may embed external images and the sanitizer
// deliberately preserves http(s) <img> tags - the two layers must agree.
// frame-ancestors/base-uri/object-src/form-action add clickjacking and
// injection protections independent of inline handling. Follow-up to tighten:
// nonce the inline scripts, then drop 'unsafe-inline'.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'"

// oiFileCSP governs the /oi/file/ subtree: self-contained HTML dashboards
// produced by an external pipeline and displayed inside a same-origin <iframe>
// on /oi (plus their PDF twins). The files are inert HTML+CSS - one <style>
// block, inline style attributes, data:-URI images, zero <script> tags - so
// this policy is STRICTER than the app's (default-src 'none' blocks all script
// execution outright, guarding against a compromised pipeline injecting JS)
// while still permitting the same-origin framing the /oi page needs.
const oiFileCSP = "default-src 'none'; " +
	"img-src 'self' data:; " +
	"style-src 'unsafe-inline'; " +
	"frame-ancestors 'self'"

// securityHeaders sets defense-in-depth response headers on every response.
// HSTS is intentionally omitted: this process commonly runs plain HTTP behind a
// TLS-terminating reverse proxy, which is the correct place to own HSTS.
//
// The policy choice keys on the CLEANED request path: this middleware wraps the
// ServeMux, which canonicalizes dirty paths (e.g. /oi/file/../../dashboard)
// with a 301 only AFTER we run, so matching on the raw path would stamp the
// relaxed headers onto that redirect.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frameOptions, csp := "DENY", contentSecurityPolicy
		if strings.HasPrefix(path.Clean(r.URL.Path), "/oi/file/") {
			frameOptions, csp = "SAMEORIGIN", oiFileCSP
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", frameOptions)
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}

// debugHTTP logs each API/MCP request (method, path, status, duration, user) at
// debug level. Mounted inside the key-auth chain so the user is resolved; a
// near no-op when debug logging is off.
func debugHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		user := ""
		if u := auth.UserFrom(r.Context()); u != nil {
			user = u.Username
		}
		slog.Debug("api request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).String(), "user", user)
	})
}

func run() error {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	debug := flag.Bool("d", false, "verbose debug logging (api/mcp requests + AI tool calls)")
	devUser := flag.String("dev", "", "DEV ONLY: authenticate every request as this username, bypassing login (mimics a proxy Remote-User header). For local development without a proxy - NEVER expose such an instance.")
	flag.Parse()
	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
		slog.Debug("debug logging enabled")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", cfg.Timezone, err)
	}

	conn, err := db.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()
	if err := db.RunMigrations(conn, db.MigrationVars{BootstrapUsername: cfg.Auth.BootstrapUser}); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	queries := models.New(conn)

	// Market-data cache + background prefetch for the trade-replay charts.
	// Disabled -> nil cache (charts hidden in the UI). Prefetch off -> the cache
	// is still served (lazy "Load chart"), but no warm-on-import worker runs.
	var marketCache *marketdata.CachingSource
	var prefetcher *marketdata.Prefetcher
	if cfg.MarketData.Enabled == nil || *cfg.MarketData.Enabled {
		timeout := time.Duration(cfg.MarketData.HTTPTimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		marketCache = &marketdata.CachingSource{
			DB:          conn,
			Source:      &marketdata.Yahoo{BaseURL: cfg.MarketData.YahooBaseURL, Client: &http.Client{Timeout: timeout}},
			Loc:         tz,
			IntradayTTL: time.Duration(cfg.MarketData.IntradayRefreshMin) * time.Minute,
		}
		if cfg.MarketData.Prefetch == nil || *cfg.MarketData.Prefetch {
			prefetcher = marketdata.NewPrefetcher(marketCache, conn, tz)
		}
	}

	// In-app AI analysis (server-side). Both nil when disabled; the handlers gate
	// every AI route + UI affordance on AIJobs being non-nil.
	var aiClient ai.Client
	var aiJobs *ai.JobManager
	if cfg.AI.Enabled {
		c, err := ai.New(ai.Options{
			Provider:    cfg.AI.Provider,
			BaseURL:     cfg.AI.BaseURL,
			Model:       cfg.AI.Model,
			APIKey:      cfg.AI.APIKey,
			Timeout:     time.Duration(cfg.AI.TimeoutSec) * time.Second,
			MaxTokens:   cfg.AI.MaxTokens,
			Temperature: cfg.AI.Temperature,
			Headers:     cfg.AI.Headers,
		})
		if err != nil {
			return fmt.Errorf("ai: %w", err)
		}
		aiClient = c
		aiJobs = ai.NewJobManager()
		slog.Info("AI analysis enabled", "provider", cfg.AI.Provider, "base_url", cfg.AI.BaseURL, "model", cfg.AI.Model)
	}

	// Gamma-exposure annotations (gex-engine). Nil when disabled; the
	// prepare-day annotator is a no-op on a nil client.
	var gexClient *gex.Client
	if cfg.GEX.Enabled {
		c, err := gex.New(gex.Options{
			BaseURL: cfg.GEX.BaseURL,
			APIKey:  cfg.GEX.APIKey,
			Timeout: time.Duration(cfg.GEX.TimeoutSec) * time.Second,
		})
		if err != nil {
			return fmt.Errorf("gex: %w", err)
		}
		gexClient = c
		slog.Info("GEX annotations enabled", "base_url", cfg.GEX.BaseURL)
	}

	inbox := filepath.Join(cfg.Imports.Root, "inbox")
	processed := filepath.Join(cfg.Imports.Root, "processed")
	failed := filepath.Join(cfg.Imports.Root, "failed")
	uploads := filepath.Join(cfg.Imports.Root, "uploads") // same-filesystem staging for UI uploads
	// Attachments live next to the database file so a single backup of the
	// data/ dir captures everything the user has authored.
	attachments := filepath.Join(filepath.Dir(cfg.Database.Path), "attachments")

	// Session secret: HMAC key for CSRF tokens. Auto-generate (and warn)
	// when empty so the app still boots on a fresh deploy; production should
	// set this in config so tokens survive a restart.
	sessionSecret := cfg.Auth.SessionSecret
	if sessionSecret == "" {
		buf := make([]byte, 32)
		_, _ = rand.Read(buf)
		sessionSecret = "base64:" + base64.StdEncoding.EncodeToString(buf)
		slog.Warn("auth.session_secret not set; generated an ephemeral key — sessions will not survive restarts")
	}
	csrfSecret, err := auth.SecretFromString(sessionSecret)
	if err != nil {
		return fmt.Errorf("session secret: %w", err)
	}
	csrf := auth.NewCSRF(csrfSecret)
	actAs := &auth.ActAs{
		Queries: queries,
		Secret:  csrfSecret,
		Secure:  cfg.Auth.CookieSecure == nil || *cfg.Auth.CookieSecure,
	}

	ttlHours := cfg.Auth.SessionTTLHours
	if ttlHours <= 0 {
		ttlHours = 168 // 7 days
	}
	cookieSecure := true
	if cfg.Auth.CookieSecure != nil {
		cookieSecure = *cfg.Auth.CookieSecure
	}
	sessions := &auth.SessionStore{
		DB:      conn,
		Queries: queries,
		TTL:     time.Duration(ttlHours) * time.Hour,
		Secure:  cookieSecure,
	}
	if !cookieSecure {
		slog.Warn("auth.cookie_secure = false; session cookie will be sent over plain HTTP (only safe on a trusted private network)")
	}

	// Authenticator selection. The -dev flag short-circuits config.Auth.Mode: it
	// authenticates EVERY request as the given user (auth.Stub), so you can run
	// locally without a proxy or a login/password. It bypasses all authentication,
	// so it must never front a real deployment - hence the loud warning and the
	// "DEV ONLY" flag help.
	var authn auth.Authenticator
	var bootstrapUserID int64
	authMode := cfg.Auth.Mode
	if *devUser != "" {
		authMode = "dev"
	}
	switch authMode {
	case "dev":
		stub, err := auth.NewStub(conn, *devUser)
		if err != nil {
			return fmt.Errorf("dev auth: resolve user %q: %w", *devUser, err)
		}
		authn = stub
		bootstrapUserID = stub.User.ID
		slog.Warn("DEV AUTH ENABLED - every request is authenticated as this user with no login; do NOT expose this instance to an untrusted network",
			"user", stub.User.Username, "admin", stub.User.IsAdmin)
	case "", "local":
		la := &auth.LocalAuth{
			DB:       conn,
			Queries:  queries,
			Sessions: sessions,
		}
		authn = la
		// Importer / watcher need a user id to own auto-created accounts.
		// In local mode we attribute filesystem-watcher imports to the
		// bootstrap user; UI uploads will get the request-context user
		// once that path is threaded (phase 4).
		stub, err := auth.NewStub(conn, cfg.Auth.BootstrapUser)
		if err != nil {
			return fmt.Errorf("resolve bootstrap user: %w", err)
		}
		bootstrapUserID = stub.User.ID
	case "proxy":
		pa, err := auth.NewProxyAuth(conn, queries, auth.ProxyAuthOptions{
			UserHeader:        cfg.Auth.Proxy.UserHeader,
			EmailHeader:       cfg.Auth.Proxy.EmailHeader,
			DisplayNameHeader: cfg.Auth.Proxy.DisplayNameHeader,
			GroupsHeader:      cfg.Auth.Proxy.GroupsHeader,
			AdminGroup:        cfg.Auth.Proxy.AdminGroup,
			TrustedProxies:    cfg.Auth.Proxy.TrustedProxies,
			AutoProvision:     cfg.Auth.Proxy.AutoProvision,
			LogoutURL:         cfg.Auth.Proxy.LogoutURL,
		})
		if err != nil {
			return fmt.Errorf("proxy auth: %w", err)
		}
		authn = pa
		// Bootstrap user remains the importer's default attribution for
		// watcher drops in the flat inbox/ root; per-user subdirs override
		// it as in local mode.
		stub, err := auth.NewStub(conn, cfg.Auth.BootstrapUser)
		if err != nil {
			return fmt.Errorf("resolve bootstrap user: %w", err)
		}
		bootstrapUserID = stub.User.ID
		slog.Info("proxy auth ready", "trusted", pa.DescribeTrustedProxies(), "auto_provision", cfg.Auth.Proxy.AutoProvision)
	default:
		return fmt.Errorf("unknown auth.mode %q", cfg.Auth.Mode)
	}

	importSvc := &importer.ImportService{
		DB:           conn,
		Queries:      queries,
		ProcessedDir: processed,
		FailedDir:    failed,
		Timezone:     tz,
		UserID:       bootstrapUserID,
	}
	// Assign the concrete prefetcher only when non-nil, so the interface field
	// doesn't end up holding a typed-nil (which would read as != nil).
	if prefetcher != nil {
		importSvc.Prefetch = prefetcher
	}

	// TopstepX API sync: active only when an encryption key is configured
	// (per-user credentials are stored AES-GCM encrypted under it) and the
	// section isn't explicitly disabled. Nil = feature off; the settings card
	// then shows setup instructions.
	var topstepxSync *topstepx.SyncService
	if keyBytes, ok := cfg.TopstepX.EncryptionKeyBytes(); ok &&
		(cfg.TopstepX.Enabled == nil || *cfg.TopstepX.Enabled) {
		box, err := topstepx.NewSecretBox(keyBytes)
		if err != nil {
			return fmt.Errorf("topstepx secret box: %w", err) // key length pre-validated; unexpected
		}
		client := &topstepx.Client{
			BaseURL: cfg.TopstepX.BaseURL,
			HTTP:    &http.Client{Timeout: time.Duration(cfg.TopstepX.HTTPTimeoutSec) * time.Second},
		}
		rthStart, rthEnd, _ := cfg.TopstepX.RTHWindowHM() // validated when rth_sync_min > 0
		topstepxSync = topstepx.NewSyncService(queries, importSvc, client, box, topstepx.SyncOptions{
			Throttle:     time.Duration(cfg.TopstepX.SyncThrottleMin) * time.Minute,
			SessionEvery: time.Duration(cfg.TopstepX.SessionSyncMin) * time.Minute,
			RTHEvery:     time.Duration(cfg.TopstepX.RTHSyncMin) * time.Minute,
			RTHStartMin:  rthStart,
			RTHEndMin:    rthEnd,
		}, slog.Default())
		slog.Info("topstepx api sync enabled",
			"base_url", cfg.TopstepX.BaseURL, "throttle_min", cfg.TopstepX.SyncThrottleMin,
			"session_sync_min", cfg.TopstepX.SessionSyncMin, "rth_sync_min", cfg.TopstepX.RTHSyncMin,
			"rth_window", cfg.TopstepX.RTHWindow)
	}

	// IBKR live sync: gateways are per-user DB rows managed in settings; the
	// config only gates the feature and sets cadences.
	var ibkrSync *ibkr.SyncService
	if cfg.IBKR.Enabled == nil || *cfg.IBKR.Enabled {
		ibkrRTHStart, ibkrRTHEnd, _ := cfg.IBKR.RTHWindowHM() // validated when rth_sync_min > 0
		ibkrSync = ibkr.NewSyncService(queries, importSvc, ibkr.SyncOptions{
			Throttle:     time.Duration(cfg.IBKR.SyncThrottleMin) * time.Minute,
			SessionEvery: time.Duration(cfg.IBKR.SessionSyncMin) * time.Minute,
			RTHEvery:     time.Duration(cfg.IBKR.RTHSyncMin) * time.Minute,
			RTHStartMin:  ibkrRTHStart,
			RTHEndMin:    ibkrRTHEnd,
			TickleEvery:  time.Duration(cfg.IBKR.TickleSec) * time.Second,
		}, slog.Default())
		slog.Info("ibkr live sync enabled",
			"session_sync_min", cfg.IBKR.SessionSyncMin, "rth_sync_min", cfg.IBKR.RTHSyncMin,
			"rth_window", cfg.IBKR.RTHWindow, "tickle_sec", cfg.IBKR.TickleSec)
	}

	h := &handlers.Handlers{
		Templates:        web.TemplatesFS,
		DB:               conn,
		ImportSvc:        importSvc,
		Queries:          queries,
		UploadsDir:       uploads,
		AttachmentsDir:   attachments,
		OIDashboardsDir:  cfg.OIDashboards.Root,
		Timezone:         tz,
		Version:          appVersion(),
		CSRF:             csrf,
		ActAs:            actAs,
		MarketData:       marketCache,
		Prefetch:         prefetcher,
		MCPInstanceLabel: cfg.MCP.InstanceLabel,
		AI:               aiClient,
		AIJobs:           aiJobs,
		GEX:              gexClient,
		TopstepxSync:     topstepxSync,
		IbkrSync:         ibkrSync,
	}

	// LocalAuth's login template uses the existing renderPage pipeline so
	// base.html / nav / theme machinery is reused. The login page itself
	// also needs CSRF + CurrentUser=nil treatment; renderPage handles both.
	if la, ok := authn.(*auth.LocalAuth); ok {
		la.LoginTemplate = func(w http.ResponseWriter, data map[string]any) {
			h.RenderLogin(w, data)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /import", h.ImportPage)
	mux.HandleFunc("POST /import/upload", h.ImportUpload)
	mux.HandleFunc("POST /import/assign", h.ImportAssign)
	mux.HandleFunc("GET /trades", h.TradesPage)
	mux.HandleFunc("GET /trades/{id}", h.TradeDetail)
	mux.HandleFunc("POST /trades/{id}/journal", h.TradeJournalSave)
	// Trade-replay charts (JSON) for the trade-detail + journal views.
	mux.HandleFunc("GET /charts/trade/{id}", h.ChartTrade)
	mux.HandleFunc("GET /charts/group/{id}", h.ChartGroup)
	mux.HandleFunc("GET /charts/day/{date}", h.ChartDay)
	mux.HandleFunc("GET /charts/week/{week}", h.ChartWeek)
	mux.HandleFunc("GET /accounts", h.AccountsPage)
	mux.HandleFunc("GET /reconcile/copy", h.CopyReconcilePage)
	mux.HandleFunc("POST /accounts/save", h.AccountsBulkSave)
	mux.HandleFunc("POST /accounts/new", h.AccountsCreate)
	mux.HandleFunc("POST /accounts/recompute", h.AccountsRecomputeAll)
	mux.HandleFunc("POST /accounts/{id}/reassign", h.AccountsReassign)
	mux.HandleFunc("POST /accounts/{id}/delete", h.AccountsDelete)
	mux.HandleFunc("POST /accounts/{id}/balances", h.AccountsAddBalance)
	mux.HandleFunc("POST /accounts/{id}/clone", h.AccountsClone)
	mux.HandleFunc("POST /accounts/{id}/refetch", h.AccountsRefetch)
	mux.HandleFunc("POST /accounts/{id}/livesync-reset", h.AccountsLivesyncReset)
	mux.HandleFunc("POST /balances/{anchor_id}/delete", h.BalanceDelete)
	mux.HandleFunc("POST /accounts/{id}", h.AccountUpdate)
	// Commissions + instruments: everyone can view, only admins can modify.
	mux.HandleFunc("GET /commissions", h.CommissionsPage)
	mux.Handle("POST /commissions/save", auth.RequireAdmin(http.HandlerFunc(h.CommissionsBulkSave)))
	mux.Handle("POST /commissions/rates/new", auth.RequireAdmin(http.HandlerFunc(h.CommissionsRateCreate)))
	mux.Handle("POST /commissions/rates/delete", auth.RequireAdmin(http.HandlerFunc(h.CommissionsRateDelete)))
	mux.Handle("POST /commissions/classes/new", auth.RequireAdmin(http.HandlerFunc(h.CommissionsClassCreate)))
	// Library: per-user journal templates + trading strategies.
	mux.HandleFunc("GET /library", h.LibraryPage)
	mux.HandleFunc("POST /library/templates", h.TemplateCreate)
	mux.HandleFunc("POST /library/templates/{id}", h.TemplateUpdate)
	mux.HandleFunc("POST /library/templates/{id}/clone", h.TemplateClone)
	mux.HandleFunc("POST /library/templates/{id}/delete", h.TemplateDelete)
	mux.HandleFunc("POST /library/strategies", h.StrategyCreate)
	mux.HandleFunc("POST /library/strategies/{id}", h.StrategyUpdate)
	mux.HandleFunc("POST /library/strategies/{id}/delete", h.StrategyDelete)
	mux.HandleFunc("POST /library/strategies/{id}/archive", h.StrategyArchive)
	mux.HandleFunc("POST /library/strategies/{id}/unarchive", h.StrategyUnarchive)
	mux.HandleFunc("POST /library/playbook", h.PlaybookSave)
	// Settings: per-user API key management (session + CSRF chain).
	mux.HandleFunc("GET /settings", h.SettingsPage)
	mux.HandleFunc("POST /settings/api-keys", h.APIKeyCreate)
	mux.HandleFunc("POST /settings/api-keys/{id}/revoke", h.APIKeyRevoke)
	mux.HandleFunc("POST /settings/api-keys/{id}/delete", h.APIKeyDelete)
	mux.HandleFunc("POST /settings/language", h.SettingsLanguageSave)
	mux.HandleFunc("POST /settings/account-visibility", h.SettingsAccountVisibilitySave)
	mux.HandleFunc("POST /settings/topstepx", h.SettingsTopstepxSave)
	mux.HandleFunc("POST /settings/topstepx/test", h.SettingsTopstepxTest)
	mux.HandleFunc("POST /settings/topstepx/delete", h.SettingsTopstepxDelete)
	mux.HandleFunc("POST /settings/ibkr/gateways", h.SettingsIbkrGatewayCreate)
	mux.HandleFunc("POST /settings/ibkr/gateways/{id}/test", h.SettingsIbkrGatewayTest)
	mux.HandleFunc("POST /settings/ibkr/gateways/{id}/toggle", h.SettingsIbkrGatewayToggle)
	mux.HandleFunc("POST /settings/ibkr/gateways/{id}/delete", h.SettingsIbkrGatewayDelete)
	mux.HandleFunc("GET /journal", h.JournalFeed)
	mux.HandleFunc("GET /journal/export.md", h.JournalExportMarkdown)
	mux.HandleFunc("GET /journal/export.pdf", h.JournalExportPDF)
	mux.HandleFunc("GET /journal/templates", h.JournalTemplatesList)
	mux.HandleFunc("GET /journal/recent", h.JournalRecentList)
	mux.HandleFunc("POST /journal/preview", h.JournalPreview)
	mux.HandleFunc("POST /journal/attachments", h.JournalAttachmentUpload)
	mux.HandleFunc("GET /journal/attachments/{id}", h.JournalAttachmentServe)
	mux.HandleFunc("GET /journal/week/{week}", h.WeekJournalPage)
	mux.HandleFunc("POST /journal/week/{week}", h.WeekJournalSave)
	mux.HandleFunc("POST /journal/week/{week}/delete", h.WeekJournalDelete)
	mux.HandleFunc("GET /journal/trade/{id}", h.JournalTradePage)
	mux.HandleFunc("POST /journal/group/new", h.GroupJournalCreate)
	mux.HandleFunc("GET /journal/group/{id}", h.GroupJournalPage)
	mux.HandleFunc("POST /journal/group/{id}", h.GroupJournalSave)
	mux.HandleFunc("POST /journal/group/{id}/delete", h.GroupJournalDelete)
	mux.HandleFunc("POST /journal/group/{id}/link", h.GroupJournalLink)
	mux.HandleFunc("POST /journal/group/{id}/unlink", h.GroupJournalUnlink)
	mux.HandleFunc("GET /journal/{date}", h.JournalPage)
	mux.HandleFunc("POST /journal/{date}", h.JournalSave)
	// In-app AI analysis (handlers 404 when ai.enabled is false).
	mux.HandleFunc("POST /ai/day/{date}", h.AIDayStart)
	mux.HandleFunc("GET /ai/day/{date}/status", h.AIDayStatus)
	mux.HandleFunc("POST /ai/day/{date}/cancel", h.AIDayCancel)
	mux.HandleFunc("POST /ai/entry/{id}", h.AIAnalyzeEntry)
	mux.HandleFunc("POST /ai/trade/{id}", h.AIAnalyzeTrade)
	mux.HandleFunc("GET /calendar", h.CalendarPage)
	mux.HandleFunc("GET /dashboard", h.DashboardPage)
	mux.HandleFunc("GET /analytics", h.AnalyticsPage)
	mux.HandleFunc("GET /sync/banner", h.SyncBanner)
	mux.HandleFunc("GET /oi", h.OIDashboardPage)
	mux.HandleFunc("GET /oi/{date}", h.OIDashboardPage)
	mux.HandleFunc("GET /oi/file/{name}", h.OIDashboardFile)

	// Gamma-exposure view (handlers 404 when gex is disabled). /gex/data
	// feeds the page's scrub slider with per-moment JSON.
	mux.HandleFunc("GET /gex", h.GEXPage)
	mux.HandleFunc("GET /gex/data/{market}", h.GEXData)
	mux.HandleFunc("GET /gex/{market}", h.GEXPage)

	// Admin user management. RequireAdmin wraps each handler so non-admins
	// get 403 even if they guess the URL. Act-as endpoints live in the
	// same admin namespace because they're scoped by admin privilege.
	mux.Handle("GET /admin/users", auth.RequireAdmin(http.HandlerFunc(h.AdminUsersPage)))
	mux.Handle("POST /admin/users", auth.RequireAdmin(http.HandlerFunc(h.AdminUsersCreate)))
	mux.Handle("POST /admin/users/{id}/reset", auth.RequireAdmin(http.HandlerFunc(h.AdminUsersResetPassword)))
	mux.Handle("POST /admin/users/{id}/admin", auth.RequireAdmin(http.HandlerFunc(h.AdminUsersToggleAdmin)))
	mux.Handle("POST /admin/users/{id}/active", auth.RequireAdmin(http.HandlerFunc(h.AdminUsersToggleActive)))
	mux.Handle("POST /admin/users/{id}/delete", auth.RequireAdmin(http.HandlerFunc(h.AdminUsersDelete)))
	mux.Handle("POST /admin/act-as/{id}", auth.RequireAdmin(http.HandlerFunc(h.AdminActAsStart)))
	mux.HandleFunc("POST /admin/act-as/clear", h.AdminActAsClear)
	// Admin market-data: review chart-cache coverage and backfill gaps.
	mux.Handle("GET /admin/transactions", auth.RequireAdmin(http.HandlerFunc(h.AdminTransactionsPage)))
	mux.Handle("POST /admin/transactions", auth.RequireAdmin(http.HandlerFunc(h.AdminTransactionCreate)))
	mux.Handle("POST /admin/transactions/save", auth.RequireAdmin(http.HandlerFunc(h.AdminTransactionsBulkSave)))

	mux.Handle("GET /admin/market-data", auth.RequireAdmin(http.HandlerFunc(h.AdminMarketDataPage)))
	mux.Handle("POST /admin/market-data/backfill", auth.RequireAdmin(http.HandlerFunc(h.AdminMarketDataBackfill)))

	staticSub, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return fmt.Errorf("static sub-fs: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Root redirects to the dashboard.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})

	authn.Routes(mux)

	// Top-level split: /api/* is a stateless, key-authenticated API that bypasses
	// CSRF + session entirely; everything else keeps the HTML app's chain
	// (CSRF -> RequireUser -> mux). http.ServeMux subtree dispatch on "/api/"
	// does not strip the prefix, so the API mux registers full /api/v1/... paths.
	apiKeyAuth := &auth.APIKeyAuth{Queries: queries}
	root := http.NewServeMux()
	root.Handle("/api/", apiKeyAuth.Middleware(debugHTTP(h.APIMux())))
	// MCP server for AI clients - same per-user key auth as /api, no CSRF/session.
	if cfg.MCP.Enabled == nil || *cfg.MCP.Enabled {
		root.Handle("/mcp", apiKeyAuth.Middleware(debugHTTP(h.MCPHandler())))
		slog.Info("MCP server enabled", "path", "/mcp")
	}
	root.Handle("/", csrf.Middleware(auth.RequireUser(authn, actAs, mux)))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           securityHeaders(root),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background session sweep — purge expired rows every hour.
	go sessions.StartJanitor(ctx, time.Hour)

	// Periodic backup — one snapshot at startup (deduped per day) and one
	// daily at the configured hour. Failures are logged, never fatal.
	if cfg.Backup.Enabled {
		go backup.Schedule(ctx, backup.Options{
			Dir:         cfg.Backup.Dir,
			Hour:        cfg.Backup.Hour,
			Loc:         tz,
			DBPath:      cfg.Database.Path,
			ConfigPath:  *cfgPath,
			ImportsRoot: cfg.Imports.Root,
			DB:          conn,
		})
	}

	// Background market-data prefetch worker - warms the chart bar cache for
	// accounts touched by imports. Stops on ctx cancellation like the others.
	if prefetcher != nil {
		go prefetcher.Run(ctx)
		if h, m, ok := cfg.MarketData.NightlyRefreshHM(); ok {
			go prefetcher.ScheduleNightly(ctx, h, m)
			slog.Info("market-data nightly refresh scheduled", "at", cfg.MarketData.NightlyRefresh)
		}
	}

	// TopstepX API sync worker + hourly CME-hours scheduler + session-token
	// maintainer (renews tokens via validate before they expire, so syncs and
	// restarts never need a fresh login). Page-visit hooks feed the same
	// queue; all stop on ctx cancellation.
	if topstepxSync != nil {
		go topstepxSync.Run(ctx)
		go topstepxSync.Schedule(ctx)
		go topstepxSync.MaintainTokens(ctx)
	}

	// IBKR gateway polling: worker + flat-cadence scheduler + session
	// keep-alive (tickle/reauthenticate).
	if ibkrSync != nil {
		go ibkrSync.Run(ctx)
		go ibkrSync.Schedule(ctx)
		go ibkrSync.KeepAlive(ctx)
	}

	// MotiveWave email capture: embedded SMTP listener archiving MW's
	// "order filled" notifications and (unless mw_email.ingest is false)
	// upserting them as provisional executions that the CSV import later
	// supersedes. Fills attribute to whichever user owns the account ref;
	// unknown refs stay archive-only until their first CSV import. Run logs
	// its own "listening" line; a startup failure (bad TLS files, port in
	// use) is fatal only to the module, not the app.
	if cfg.MWEmail.Enabled {
		h.MWEmailArchiveDir = cfg.MWEmail.ArchiveDir // admin alert card on /accounts
		mwSrv := &mwmail.Server{Cfg: cfg.MWEmail}
		if cfg.MWEmail.IngestEnabled() {
			mwSrv.Ingest = mwmail.ImporterIngest(importSvc)
		}
		go func() {
			if err := mwSrv.Run(ctx); err != nil {
				slog.Error("mw email capture failed", "err", err)
			}
		}()
	}

	// Start the inbox watcher. Errors during individual imports are logged
	// inside the watcher; only setup errors come back here.
	watcher := &importer.Watcher{
		InboxDir: inbox,
		Service:  importSvc,
		DB:       conn, // resolves imports/inbox/<username>/ subdirs to user_ids
	}
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		if err := watcher.Run(ctx); err != nil {
			slog.Error("watcher exited with error", "err", err)
			stop()
		}
	}()

	go func() {
		slog.Info("server listening", "addr", srv.Addr, "auth_mode", authMode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown", "err", err)
	}
	<-watcherDone
	return nil
}
