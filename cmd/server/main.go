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
	"path/filepath"
	"syscall"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/backup"
	"github.com/w4jnl/trading-journal/internal/config"
	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/handlers"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

const topHelp = `Usage: trading-journal [flags]
       trading-journal users <subcommand> [args]

Run the server (default):
  -config PATH   Path to config.yaml (default: ./config.yaml).
                 If the file is missing the server boots with built-in
                 defaults; see config.example.yaml for the full schema.

User management:
  trading-journal users -h
                 Print user-management subcommands (add, reset, delete,
                 list, activate, deactivate, promote, demote).
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

	if err := run(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

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

	// Authenticator selection from cfg.Auth.Mode. Defaults to local.
	var authn auth.Authenticator
	var bootstrapUserID int64
	switch cfg.Auth.Mode {
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

	h := &handlers.Handlers{
		Templates:       web.TemplatesFS,
		DB:              conn,
		ImportSvc:       importSvc,
		Queries:         queries,
		UploadsDir:      uploads,
		AttachmentsDir:  attachments,
		OIDashboardsDir: cfg.OIDashboards.Root,
		Timezone:        tz,
		CSRF:            csrf,
		ActAs:           actAs,
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
	mux.HandleFunc("GET /accounts", h.AccountsPage)
	mux.HandleFunc("POST /accounts/save", h.AccountsBulkSave)
	mux.HandleFunc("POST /accounts/new", h.AccountsCreate)
	mux.HandleFunc("POST /accounts/recompute", h.AccountsRecomputeAll)
	mux.HandleFunc("POST /accounts/{id}/reassign", h.AccountsReassign)
	mux.HandleFunc("POST /accounts/{id}/delete", h.AccountsDelete)
	mux.HandleFunc("POST /accounts/{id}/balances", h.AccountsAddBalance)
	mux.HandleFunc("POST /accounts/{id}/clone", h.AccountsClone)
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
	mux.HandleFunc("POST /library/playbook", h.PlaybookSave)
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
	mux.HandleFunc("GET /calendar", h.CalendarPage)
	mux.HandleFunc("GET /dashboard", h.DashboardPage)
	mux.HandleFunc("GET /analytics", h.AnalyticsPage)
	mux.HandleFunc("GET /oi", h.OIDashboardPage)
	mux.HandleFunc("GET /oi/{date}", h.OIDashboardPage)
	mux.HandleFunc("GET /oi/file/{name}", h.OIDashboardFile)

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

	// Handler chain: CSRF -> RequireUser -> mux. CSRF runs first so even
	// the auth-protected POSTs are token-checked; RequireUser then resolves
	// the session into r.Context().
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           csrf.Middleware(auth.RequireUser(authn, actAs, mux)),
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
		slog.Info("server listening", "addr", srv.Addr, "auth_mode", cfg.Auth.Mode)
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
