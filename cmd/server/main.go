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
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/backup"
	"github.com/w4jnl/trading-journal/internal/config"
	"github.com/w4jnl/trading-journal/internal/copyset"
	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/excursion"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/guardrail"
	"github.com/w4jnl/trading-journal/internal/handlers"
	"github.com/w4jnl/trading-journal/internal/ibkr"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/mwmail"
	"github.com/w4jnl/trading-journal/internal/oauth"
	"github.com/w4jnl/trading-journal/internal/secretbox"
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
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recoverAndLog is the outermost middleware: a handler panic becomes a
// logged 500 with a stack instead of a dropped connection (net/http's
// default only logs it), and any response that took over a second or ended
// in a 5xx gets one Warn line - the request log the HTML routes never had.
// http.ErrAbortHandler is re-panicked, as the contract requires.
func recoverAndLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				slog.Error("panic serving request", "method", r.Method, "path", r.URL.Path,
					"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if !rec.wrote {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
				}
				return
			}
			if dur := time.Since(start); rec.status >= 500 || dur > time.Second {
				slog.Warn("request", "method", r.Method, "path", r.URL.Path,
					"status", rec.status, "dur", dur.String())
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// noWriteDeadline lifts the server's WriteTimeout for one route. The MCP
// transport holds a streamable-HTTP (SSE) response open for as long as the
// client keeps the session; the 120 s server default would sever it.
func noWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		next.ServeHTTP(w, r)
	})
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
//
// form-action is 'self' plus any configured cross-origin SSO logout target
// (proxy mode's auth.proxy.logout_url): the sign-out form POSTs to /logout
// which 303-redirects to the identity provider, and a browser enforces
// form-action against that final redirect origin - without it listed the
// logout navigation is blocked and sign-out silently does nothing.
func buildContentSecurityPolicy(logoutOrigin, nonce string) string {
	formAction := "form-action 'self'"
	if logoutOrigin != "" {
		formAction += " " + logoutOrigin
	}
	// Scripts: same-origin files plus this response's nonce. No
	// 'unsafe-inline': every inline <script> a template renders carries
	// nonce="{{$.Nonce}}", htmx stamps the same nonce on scripts it swaps in
	// (inlineScriptNonce), and on* attributes are gone (inline_handlers.js).
	// Styles keep 'unsafe-inline' for the style="" attributes.
	return "default-src 'self'; " +
		"script-src 'self' 'nonce-" + nonce + "'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: https:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		formAction + "; " +
		"object-src 'none'"
}

// cspOrigin reduces a URL to its scheme://host origin (a valid CSP source),
// dropping path/query. Empty when the URL is blank, relative (same-origin -
// already covered by 'self'), or unparseable.
func cspOrigin(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

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
func securityHeaders(logoutOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newScriptNonce()
		frameOptions, csp := "DENY", buildContentSecurityPolicy(logoutOrigin, nonce)
		if strings.HasPrefix(path.Clean(r.URL.Path), "/oi/file/") {
			frameOptions, csp = "SAMEORIGIN", oiFileCSP
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", frameOptions)
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r.WithContext(handlers.WithNonce(r.Context(), nonce)))
	})
}

// newScriptNonce is 128 random bits, base64 (the CSP nonce alphabet).
func newScriptNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error()) // the kernel RNG is gone; nothing sane to serve
	}
	return base64.StdEncoding.EncodeToString(b[:])
}

// oauthConfig converts the YAML block into the oauth package's resolved config
// (seconds -> durations; tri-state consent resolved).
func oauthConfig(o config.MCPOAuth) oauth.Config {
	return oauth.Config{
		Issuer:               o.IssuerURL(),
		Consent:              o.ConsentRequired(),
		AccessTokenTTL:       time.Duration(o.AccessTokenTTLSec) * time.Second,
		RefreshTokenTTL:      time.Duration(o.RefreshTokenTTLSec) * time.Second,
		CodeTTL:              time.Duration(o.CodeTTLSec) * time.Second,
		AllowedRedirectHosts: o.AllowedRedirectHosts,
	}
}

// staticCacheHeaders sets the cache policy for embedded /static assets. With
// a release build the asset URLs carry ?v=<assetVersion> (see the assetVer
// template func), so the content behind a URL never changes: a year-long
// immutable cache is correct and an ETag lets a stale-but-present copy
// revalidate with a 304 (http.ServeContent honours a pre-set ETag). A
// development build ("dev" or a dirty checkout) has no stable version, so
// its assets are served no-cache. Cutting asset requests matters here
// because each one otherwise pays the reverse proxy's forward-auth round trip.
// Assets referenced without ?v= (fonts from app.css url(), the vendored
// chart library) must change filename when their content changes.
func staticCacheHeaders(assetVersion string, next http.Handler) http.Handler {
	etag := ""
	if assetVersion != "" {
		etag = `"` + assetVersion + `"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag == "" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
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
	backupOpts := backup.Options{
		Dir:         cfg.Backup.Dir,
		Hour:        cfg.Backup.Hour,
		Loc:         tz,
		DBPath:      cfg.Database.Path,
		ConfigPath:  *cfgPath,
		ImportsRoot: cfg.Imports.Root,
		DB:          conn,
	}
	// A schema change is the one moment a rollback needs the exact prior
	// state, and the daily archive may already be hours old (or taken after
	// the migration, on a restart). Snapshot first when anything is pending.
	pending, err := db.PendingMigrations(conn)
	if err != nil {
		return fmt.Errorf("check migrations: %w", err)
	}
	if len(pending) > 0 && cfg.Backup.Enabled {
		preCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := backup.RunPreMigration(preCtx, backupOpts, pending[len(pending)-1]); err != nil {
			slog.Error("pre-migration backup failed; continuing with the migration", "err", err)
		} else {
			slog.Info("pre-migration backup written", "pending", len(pending), "through", pending[len(pending)-1])
		}
		cancel()
	}
	if err := db.RunMigrations(conn, db.MigrationVars{BootstrapUsername: cfg.Auth.BootstrapUser}); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	queries := models.New(conn)
	// Second pool, read-only, for page renders and API reads: WAL lets them
	// run alongside the writer instead of queueing behind imports and syncs.
	rconn, err := db.OpenReader(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open reader: %w", err)
	}
	defer rconn.Close()
	rqueries := models.New(rconn)

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

	// In-app AI analysis (server-side): a server-wide default client and/or
	// per-user provider keys (stored under the secrets box). AIJobs is non-nil
	// when either is possible; the handlers gate every AI route + UI affordance
	// on it, and resolve each user's client per call.
	// The app context: cancelled by SIGINT/SIGTERM, parent of every
	// background job (AI analyses, sync workers, retention loops).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		slog.Info("AI analysis enabled", "provider", cfg.AI.Provider, "base_url", cfg.AI.BaseURL, "model", cfg.AI.Model)
	}
	var aiSecrets *secretbox.Box
	if cfg.AIUserKeys() {
		keyBytes, _ := cfg.SecretsKeyBytes()
		box, err := secretbox.New(keyBytes)
		if err != nil {
			return fmt.Errorf("ai secret box: %w", err) // key length pre-validated; unexpected
		}
		aiSecrets = box
		slog.Info("AI: users may bring their own provider key (Settings > AI)")
	}
	var aiReviews *ai.Reviews
	var aiTasks *ai.Tasks
	if aiClient != nil || aiSecrets != nil {
		aiJobs = ai.NewJobManager(ctx)
		aiReviews = ai.NewReviews(ctx, 2)
		aiTasks = ai.NewTasks(ctx, 1)
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

	// Live-refresh notifier: mutation paths bump it post-commit, open pages
	// poll it via GET /data/version. Always constructed (no typed-nil hazard;
	// its methods are nil-safe anyway).
	liveData := livedata.NewNotifier()
	if aiReviews != nil {
		// A review starting or ending redraws the open lists (row spinners).
		aiReviews.OnChange = liveData.Bump
		aiTasks.OnChange = liveData.Bump
	}

	// MAE/MFE off the bar cache: filled after each prefetch pass, on sheet
	// view, and once over the whole cache at boot (nothing fetches).
	var excursions *excursion.Service
	if marketCache != nil {
		excursions = &excursion.Service{DB: conn, Q: queries, Bars: marketCache, Changed: liveData.Bump}
		if prefetcher != nil {
			prefetcher.AfterAccount = func(ctx context.Context, accountID int64) {
				if _, err := excursions.FillAccount(ctx, accountID); err != nil && ctx.Err() == nil {
					slog.Warn("excursion fill after prefetch", "account_id", accountID, "err", err)
				}
			}
		}
	}

	importSvc := &importer.ImportService{
		DB:           conn,
		Queries:      queries,
		ProcessedDir: processed,
		FailedDir:    failed,
		Timezone:     tz,
		UserID:       bootstrapUserID,
		Versions:     liveData,
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
	if keyBytes, ok := cfg.TopstepXKeyBytes(); ok &&
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
			APIThrottle:  time.Duration(cfg.TopstepX.APISyncThrottleSec) * time.Second,
			SessionEvery: time.Duration(cfg.TopstepX.SessionSyncMin) * time.Minute,
			RTHEvery:     time.Duration(cfg.TopstepX.RTHSyncMin) * time.Minute,
			RTHStartMin:  rthStart,
			RTHEndMin:    rthEnd,
		}, slog.Default())
		if marketCache != nil {
			// The exchange feed behind the shared bar cache: the exact
			// contract for the recent days of the traded roots, and the
			// one-second bars around short trades.
			marketCache.Exchange = &topstepx.BarFeed{Sync: topstepxSync, DB: conn}
		}
		slog.Info("topstepx api sync enabled",
			"base_url", cfg.TopstepX.BaseURL, "throttle_min", cfg.TopstepX.SyncThrottleMin,
			"api_sync_throttle_sec", cfg.TopstepX.APISyncThrottleSec,
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
			APIThrottle:  time.Duration(cfg.IBKR.APISyncThrottleSec) * time.Second,
			SessionEvery: time.Duration(cfg.IBKR.SessionSyncMin) * time.Minute,
			RTHEvery:     time.Duration(cfg.IBKR.RTHSyncMin) * time.Minute,
			RTHStartMin:  ibkrRTHStart,
			RTHEndMin:    ibkrRTHEnd,
			TickleEvery:  time.Duration(cfg.IBKR.TickleSec) * time.Second,
		}, slog.Default())
		slog.Info("ibkr live sync enabled",
			"session_sync_min", cfg.IBKR.SessionSyncMin, "rth_sync_min", cfg.IBKR.RTHSyncMin,
			"api_sync_throttle_sec", cfg.IBKR.APISyncThrottleSec,
			"rth_window", cfg.IBKR.RTHWindow, "tickle_sec", cfg.IBKR.TickleSec)
	}

	// Built-in OAuth 2.1 authorization server for /mcp (mcp.oauth). Built
	// before the handlers so the consent pages can reach it; nil when off.
	var oauthSrv *oauth.Server
	if (cfg.MCP.Enabled == nil || *cfg.MCP.Enabled) && cfg.MCP.OAuthEnabled() {
		oauthSrv = &oauth.Server{Queries: queries, Cfg: oauthConfig(cfg.MCP.OAuth)}
	}

	// The intraday guardrail: re-reads the day of each user who turned it on
	// as their fills arrive and records nudges the pages pick up.
	guard := &guardrail.Service{DB: conn, RDB: rconn, Live: liveData, TZ: tz}
	if marketCache != nil {
		// The newest cached bar, never a fetch: the warm-up keeps it fresh.
		guard.LastClose = func(ctx context.Context, root string) (float64, time.Time, bool) {
			sym, ok := marketdata.YahooSymbol(root)
			if !ok {
				return 0, time.Time{}, false
			}
			b, ok := marketCache.CachedLatest(ctx, sym)
			if !ok {
				return 0, time.Time{}, false
			}
			return b.Close, time.Unix(b.Time, 0), true
		}
	}

	h := &handlers.Handlers{
		LiveData:         liveData,
		Guardrail:        guard,
		Templates:        web.TemplatesFS,
		DB:               conn,
		ImportSvc:        importSvc,
		Queries:          queries,
		RDB:              rconn,
		RQ:               rqueries,
		UploadsDir:       uploads,
		AttachmentsDir:   attachments,
		ExternalURL:      cfg.Server.ExternalURL,
		OIDashboardsDir:  cfg.OIDashboards.Root,
		Timezone:         tz,
		Version:          appVersion(),
		CSRF:             csrf,
		IbkrGatewayHosts: cfg.IBKR.AllowedGatewayHosts,
		ActAs:            actAs,
		MarketData:       marketCache,
		Prefetch:         prefetcher,
		Excursions:       excursions,
		MCPInstanceLabel: cfg.MCP.InstanceLabel,
		OAuth:            oauthSrv,
		AI:               aiClient,
		AIJobs:           aiJobs,
		AIReviews:        aiReviews,
		AITasks:          aiTasks,
		AISecrets:        aiSecrets,
		AIModel:          cfg.AI.Model,
		GEX:              gexClient,
		TopstepxSync:     topstepxSync,
		IbkrSync:         ibkrSync,
		PairLimiter:      auth.NewAttemptLimiter(),
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// A locked or corrupt database must not read as healthy; one
		// trivial statement with a short deadline is the probe.
		pingCtx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		var one int
		if err := conn.QueryRowContext(pingCtx, "SELECT 1").Scan(&one); err != nil {
			slog.Warn("healthz: database probe failed", "err", err)
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /import", h.ImportPage)
	mux.HandleFunc("POST /import/upload", h.ImportUpload)
	mux.HandleFunc("POST /import/assign", h.ImportAssign)
	mux.HandleFunc("GET /trades", h.TradesPage)
	mux.HandleFunc("GET /trades/{id}", h.TradeDetail)
	mux.HandleFunc("GET /trades/sets/{id}", h.TradeSetDetail)
	mux.HandleFunc("POST /trades/sets/{id}/journal", h.TradeSetJournal)
	mux.HandleFunc("POST /trades/sets/{id}/plan", h.TradeSetPlanSave)
	mux.HandleFunc("POST /trades/{id}/journal", h.TradeJournalSave)
	mux.HandleFunc("POST /trades/{id}/plan", h.TradePlanSave)
	// An open position (no trade row yet) as a sheet, with its plan.
	mux.HandleFunc("GET /positions/{account}/{symbol}", h.PositionSheet)
	mux.HandleFunc("POST /positions/{account}/{symbol}/plan", h.PositionPlanSave)
	// The AI's stop and target on the open position: a background task the
	// sheet polls; nothing is written until the trader applies it.
	mux.HandleFunc("POST /positions/{account}/{symbol}/suggest", h.PositionSuggestStart)
	mux.HandleFunc("POST /positions/{account}/{symbol}/suggest/apply", h.PositionSuggestApply)
	mux.HandleFunc("POST /positions/{account}/{symbol}/suggest/dismiss", h.PositionSuggestDismiss)
	// Trade-replay charts (JSON) for the trade-detail + journal views.
	mux.HandleFunc("GET /charts/trade/{id}", h.ChartTrade)
	mux.HandleFunc("GET /charts/group/{id}", h.ChartGroup)
	mux.HandleFunc("GET /charts/set/{id}", h.ChartSet)
	mux.HandleFunc("GET /charts/day/{date}", h.ChartDay)
	mux.HandleFunc("GET /charts/week/{week}", h.ChartWeek)
	mux.HandleFunc("GET /accounts", h.AccountsPage)
	mux.HandleFunc("GET /reconcile/copy", h.CopyReconcilePage)
	mux.HandleFunc("POST /accounts/save", h.AccountsBulkSave)
	mux.HandleFunc("POST /accounts/new", h.AccountsCreate)
	// Admin-only: the alert card's catch-up for captured email fills whose
	// account exists but whose orders were never booked.
	mux.Handle("POST /accounts/email-fills/retry", auth.RequireAdmin(http.HandlerFunc(h.AccountsRetryEmailFills)))
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
	mux.HandleFunc("POST /library/strategies/{id}/draft", h.StrategyDraftStart)
	mux.HandleFunc("POST /library/strategies/{id}/draft/apply", h.StrategyDraftApply)
	mux.HandleFunc("POST /library/strategies/{id}/draft/discard", h.StrategyDraftDiscard)
	mux.HandleFunc("POST /library/strategies/discover", h.StrategyDiscoverStart)
	mux.HandleFunc("POST /library/strategies/discover/create", h.StrategyDiscoverCreate)
	mux.HandleFunc("POST /library/strategies/discover/discard", h.StrategyDiscoverDiscard)
	mux.HandleFunc("GET /ai/tasks/status", h.AITaskStatus)
	mux.HandleFunc("POST /library/playbook", h.PlaybookSave)
	// Settings: per-user API key management (session + CSRF chain).
	mux.HandleFunc("GET /settings", h.SettingsPage)
	mux.HandleFunc("POST /settings/api-keys", h.APIKeyCreate)
	mux.HandleFunc("POST /settings/devices/pair", h.SettingsPairStart)
	mux.HandleFunc("POST /settings/api-keys/{id}/revoke", h.APIKeyRevoke)
	mux.HandleFunc("POST /settings/api-keys/{id}/delete", h.APIKeyDelete)
	// Connected apps (OAuth grants for /mcp): revoke cuts the app off at once.
	mux.HandleFunc("POST /settings/oauth/{id}/revoke", h.OAuthGrantRevoke)
	mux.HandleFunc("POST /settings/oauth/{id}/delete", h.OAuthGrantDelete)
	// In-app API reference (API.md rendered) + a session-auth alias for the
	// OpenAPI document so the page's link works in a browser; /api/v1/openapi.json
	// itself stays key-gated for machines.
	mux.HandleFunc("GET /settings/api-docs", h.APIDocsPage)
	// OAuth consent (mcp.oauth): on the session/proxy chain on purpose - the
	// human must be signed in (Authelia / local session) so the grant binds to
	// the right journal user. The reverse proxy must NOT bypass Authelia here.
	mux.HandleFunc("GET /oauth/authorize", h.OAuthAuthorizePage)
	mux.HandleFunc("POST /oauth/authorize", h.OAuthAuthorizeApprove)
	mux.HandleFunc("GET /settings/api-docs/openapi.json", h.APIOpenAPI)
	mux.HandleFunc("POST /settings/language", h.SettingsLanguageSave)
	mux.HandleFunc("POST /settings/ai", h.SettingsAISave)
	mux.HandleFunc("POST /settings/ai/test", h.SettingsAITest)
	mux.HandleFunc("POST /settings/ai/delete", h.SettingsAIDelete)
	mux.HandleFunc("POST /settings/ai/models", h.SettingsAIModels)
	mux.HandleFunc("POST /settings/ai/chart", h.SettingsAIChartSave)
	mux.HandleFunc("POST /settings/ai/autoplan", h.SettingsAIAutoPlanSave)
	mux.HandleFunc("POST /settings/guardrail", h.SettingsGuardrailSave)
	mux.HandleFunc("POST /settings/account-visibility", h.SettingsAccountVisibilitySave)
	mux.HandleFunc("POST /settings/topstepx", h.SettingsTopstepxSave)
	mux.HandleFunc("POST /settings/topstepx/test", h.SettingsTopstepxTest)
	mux.HandleFunc("POST /settings/topstepx/delete", h.SettingsTopstepxDelete)
	mux.HandleFunc("POST /settings/ibkr/gateways", h.SettingsIbkrGatewayCreate)
	mux.HandleFunc("POST /settings/ibkr/gateways/{id}/test", h.SettingsIbkrGatewayTest)
	mux.HandleFunc("POST /settings/ibkr/gateways/{id}/toggle", h.SettingsIbkrGatewayToggle)
	mux.HandleFunc("POST /settings/ibkr/gateways/{id}/delete", h.SettingsIbkrGatewayDelete)
	mux.HandleFunc("GET /digest", h.DigestPage)
	mux.HandleFunc("GET /digest.md", h.DigestMarkdown)
	mux.HandleFunc("GET /journal", h.JournalFeed)
	mux.HandleFunc("GET /journal/export.md", h.JournalExportMarkdown)
	mux.HandleFunc("GET /journal/export.pdf", h.JournalExportPDF)
	mux.HandleFunc("GET /journal/templates", h.JournalTemplatesList)
	mux.HandleFunc("GET /journal/recent", h.JournalRecentList)
	mux.HandleFunc("POST /journal/preview", h.JournalPreview)
	mux.HandleFunc("POST /journal/attachments", h.JournalAttachmentUpload)
	mux.HandleFunc("GET /journal/attachments/{id}", h.JournalAttachmentServe)
	mux.HandleFunc("POST /journal/plan/{date}", h.DayPlanSave)
	mux.HandleFunc("POST /journal/plan/{date}/draft", h.DayPlanDraft)
	mux.HandleFunc("POST /journal/plan/{date}/draft/discard", h.DayPlanDraftDiscard)
	mux.HandleFunc("GET /journal/lessons", h.LessonsPage)
	mux.HandleFunc("POST /nudges/take", h.NudgesTake)
	mux.HandleFunc("POST /journal/lessons/learned", h.LessonsLearned)
	mux.HandleFunc("GET /journal/week/{week}", h.WeekJournalPage)
	mux.HandleFunc("POST /journal/week/{week}", h.WeekJournalSave)
	mux.HandleFunc("POST /journal/week/{week}/delete", h.WeekJournalDelete)
	mux.HandleFunc("GET /journal/trade/{id}", h.JournalTradePage)
	// Re-fetch a trade/group entry's gamma-context block (engine update 009:
	// outage gaps get backfilled with reconstructed rows after the fact).
	mux.HandleFunc("POST /journal/{id}/gex/refresh", h.JournalGexRefresh)
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
	mux.HandleFunc("POST /ai/set/{id}", h.AIAnalyzeSet)
	mux.HandleFunc("POST /ai/note/day/{date}", h.AIAnalyzeDayNote)
	mux.HandleFunc("POST /ai/note/week/{week}", h.AIAnalyzeWeekNote)
	mux.HandleFunc("GET /ai/review/{id}", h.AIReviewStatus)
	mux.HandleFunc("POST /journal/suggestions/{id}", h.JournalSuggestion)
	mux.HandleFunc("GET /calendar", h.CalendarPage)
	mux.HandleFunc("GET /dashboard", h.DashboardPage)
	mux.HandleFunc("GET /analytics", h.AnalyticsPage)
	mux.HandleFunc("GET /analytics/lab", h.LabPage)
	mux.HandleFunc("POST /analytics/lab/explain", h.LabExplain)
	mux.HandleFunc("GET /data/version", h.DataVersion)
	mux.HandleFunc("GET /oi", h.OIDashboardPage)
	mux.HandleFunc("GET /oi/{date}", h.OIDashboardPage)
	mux.HandleFunc("GET /oi/file/{name}", h.OIDashboardFile)

	// Gamma-exposure view (handlers 404 when gex is disabled). /gex/data
	// feeds the page's scrub slider with per-moment JSON.
	mux.HandleFunc("GET /gex", h.GEXPage)
	mux.HandleFunc("GET /gex/exposure", h.GEXExposurePage)
	mux.HandleFunc("GET /gex/exposure/{market}", h.GEXExposurePage)
	mux.HandleFunc("GET /gex/exposure/data/{market}", h.GEXExposureData)
	mux.HandleFunc("POST /gex/viewstate", h.GEXViewStateSave)
	mux.HandleFunc("GET /gex/data/{market}", h.GEXData)
	mux.HandleFunc("POST /gex/analyze/{market}", h.GEXAnalyze)
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
	mux.Handle("POST /admin/transactions/trades/save", auth.RequireAdmin(http.HandlerFunc(h.AdminTransactionsTradesSave)))

	mux.Handle("GET /admin/market-data", auth.RequireAdmin(http.HandlerFunc(h.AdminMarketDataPage)))
	mux.Handle("POST /admin/market-data/backfill", auth.RequireAdmin(http.HandlerFunc(h.AdminMarketDataBackfill)))
	// Bulk re-read of gamma-context blocks after the engine repairs or
	// backfills history (update 010 recomputed every session since 2026-09-29).
	mux.Handle("POST /admin/market-data/gex-refresh", auth.RequireAdmin(http.HandlerFunc(h.AdminGexRefresh)))

	staticSub, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return fmt.Errorf("static sub-fs: %w", err)
	}
	if handlers.AssetVersion(h.Version) == "" {
		h.DevAssetHash = handlers.StaticHash(staticSub)
	}
	// Embedded assets: long-lived cache keyed to the build (templates append
	// ?v=<build> so a new release is fetched at once); every asset request
	// otherwise pays the reverse proxy's forward-auth round trip.
	mux.Handle("GET /static/", staticCacheHeaders(handlers.AssetVersion(h.Version),
		http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))))

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
	// The one API route without a key: the phone's pairing-code exchange
	// (throttled per address). A more specific pattern than "/api/".
	root.Handle("POST /api/v1/pair", debugHTTP(http.HandlerFunc(h.APIPair)))
	// MCP server for AI clients - same per-user key auth as /api, no CSRF/session.
	if cfg.MCP.Enabled == nil || *cfg.MCP.Enabled {
		// /mcp accepts tj_ API keys; with mcp.oauth on, MCPAuth also accepts
		// the tjo_ tokens the built-in authorization server issues and turns
		// every 401 into an RFC 9728 challenge so OAuth-capable connectors can
		// discover how to sign in. The machine-facing OAuth endpoints and the
		// discovery documents are public (the reverse proxy must route them
		// straight to the app, like /mcp); /oauth/authorize is on the mux above.
		mcpMW := apiKeyAuth.Middleware
		if oauthSrv != nil {
			mcpAuth := &auth.MCPAuth{
				Keys: apiKeyAuth, Queries: queries,
				Resource: oauthSrv.Resource(), ResourceMetadataURL: oauthSrv.ProtectedResourceMetadataURL(),
			}
			mcpMW = mcpAuth.Middleware
			root.Handle("/.well-known/oauth-authorization-server", oauthSrv.ASMetadataHandler())
			root.Handle("/.well-known/oauth-protected-resource", oauthSrv.PRMHandler())
			root.Handle("/.well-known/oauth-protected-resource/mcp", oauthSrv.PRMHandler())
			root.Handle("/oauth/register", debugHTTP(oauthSrv.RegisterHandler()))
			root.Handle("/oauth/token", debugHTTP(oauthSrv.TokenHandler()))
			slog.Info("MCP OAuth enabled", "issuer", oauthSrv.Issuer(), "resource", oauthSrv.Resource(), "consent", oauthSrv.Cfg.Consent)
		}
		root.Handle("/mcp", noWriteDeadline(mcpMW(debugHTTP(h.MCPHandler()))))
		slog.Info("MCP server enabled", "path", "/mcp")
	}
	root.Handle("/", csrf.Middleware(auth.RequireUser(authn, actAs, mux)))

	// Allow the SSO logout redirect (proxy mode) through form-action, so the
	// sign-out form's 303 to the identity provider isn't blocked by CSP.
	logoutOrigin := cspOrigin(cfg.Auth.Proxy.LogoutURL)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           securityHeaders(logoutOrigin, recoverAndLog(root)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Background session sweep — purge expired rows every hour.
	go sessions.StartJanitor(ctx, time.Hour)
	if cfg.SyncLogRetentionDays > 0 {
		// Bound the API polling history (one sync_log row per account per poll,
		// thousands a day); runs once at start so a backlog clears at once.
		go (&importer.SyncLogRetention{Queries: queries, NoOpDays: cfg.SyncLogRetentionDays}).Start(ctx, time.Hour)
	}
	if oauthSrv != nil {
		go oauthSrv.StartJanitor(ctx, time.Hour) // expired OAuth codes/tokens
	}

	// Periodic backup — one snapshot at startup (deduped per day) and one
	// daily at the configured hour. Failures are logged, never fatal.
	if cfg.Backup.Enabled {
		go backup.Schedule(ctx, backupOpts)
	}
	// Retention sweeps: intraday bar cache and the raw MotiveWave e-mail
	// archive, both of which otherwise grow without bound.
	if marketCache != nil {
		go marketdata.Retention{Cache: marketCache, Days: cfg.MarketData.BarRetentionDays}.Start(ctx)
		// The traded roots' recent sessions, kept complete and fresh for
		// what reads the session live (positions, guardrail, AI levels).
		warmer := &marketdata.Warmer{Cache: marketCache, DB: conn, Every: cfg.MarketData.SessionWarmup()}
		if marketCache.Exchange != nil {
			// One-second bars around the recent short trades, then the
			// excursions measured again from them.
			warmer.Fine = &marketdata.FineFetcher{Cache: marketCache, DB: conn}
			if excursions != nil {
				warmer.Fine.AfterTrade = func(ctx context.Context, t marketdata.FineTrade) {
					if _, _, err := excursions.Fill(ctx, excursion.Trade{ID: t.ID, SymbolRoot: t.SymbolRoot, Direction: t.Direction, Qty: t.Qty,
						EntryPrice: t.EntryPrice, EntryTime: t.EntryTime, ExitTime: t.ExitTime}); err != nil {
						slog.Warn("excursion from fine bars", "trade", t.ID, "err", err)
					}
				}
			}
		}
		go warmer.Start(ctx)
	}
	if cfg.MWEmail.Enabled {
		go mwmail.RunArchiveRetention(ctx, cfg.MWEmail.ArchiveDir, cfg.MWEmail.ArchiveRetentionDays, nil)
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
	if excursions != nil {
		go excursions.FillAll(ctx)
	}
	go guard.Run(ctx)
	if aiTasks != nil {
		// Stop and target on new positions for the users who asked for it.
		go h.RunAutoPlanner(ctx)
	}
	// Copy sets for databases from before the feature: one pass, fast.
	copyset.Backfill(ctx, queries, slog.Default())

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
	// AI day analyses were cancelled with ctx; give the in-flight entry the
	// rest of the window to record its terminal state.
	if aiJobs != nil {
		if err := aiJobs.Wait(shutdownCtx); err != nil {
			slog.Warn("ai jobs did not finish before shutdown", "err", err)
		}
		if err := aiReviews.Wait(shutdownCtx); err != nil {
			slog.Warn("ai reviews did not finish before shutdown", "err", err)
		}
		if err := aiTasks.Wait(shutdownCtx); err != nil {
			slog.Warn("ai tasks did not finish before shutdown", "err", err)
		}
	}
	// Let SQLite refresh its planner statistics while nothing is writing;
	// cheap, and the only ANALYZE this database ever gets.
	if _, err := conn.ExecContext(shutdownCtx, "PRAGMA optimize"); err != nil {
		slog.Debug("pragma optimize", "err", err)
	}
	return nil
}
