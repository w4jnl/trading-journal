package main

import (
	"context"
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

	"github.com/w4jnl/trading-journal/internal/config"
	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/handlers"
	"github.com/w4jnl/trading-journal/internal/importer"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

func main() {
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
	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", cfg.Timezone, err)
	}

	conn, err := db.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()
	if err := db.RunMigrations(conn); err != nil {
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

	importSvc := &importer.ImportService{
		DB:           conn,
		Queries:      queries,
		ProcessedDir: processed,
		FailedDir:    failed,
		Timezone:     tz,
	}

	h := &handlers.Handlers{
		Templates:      web.TemplatesFS,
		DB:             conn,
		ImportSvc:      importSvc,
		Queries:        queries,
		UploadsDir:     uploads,
		AttachmentsDir: attachments,
		Timezone:       tz,
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
	mux.HandleFunc("POST /balances/{anchor_id}/delete", h.BalanceDelete)
	mux.HandleFunc("POST /accounts/{id}", h.AccountUpdate)
	mux.HandleFunc("GET /commissions", h.CommissionsPage)
	mux.HandleFunc("POST /commissions/save", h.CommissionsBulkSave)
	mux.HandleFunc("POST /commissions/rates/new", h.CommissionsRateCreate)
	mux.HandleFunc("POST /commissions/rates/delete", h.CommissionsRateDelete)
	mux.HandleFunc("POST /commissions/classes/new", h.CommissionsClassCreate)
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

	staticSub, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return fmt.Errorf("static sub-fs: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Root redirects to the dashboard.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the inbox watcher. Errors during individual imports are logged
	// inside the watcher; only setup errors come back here.
	watcher := &importer.Watcher{
		InboxDir: inbox,
		Service:  importSvc,
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
		slog.Info("server listening", "addr", srv.Addr)
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
