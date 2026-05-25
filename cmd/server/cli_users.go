package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/config"
	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/models"
)

// usersHelp is printed for `users -h`, `users help`, an unknown subcommand,
// or when no subcommand is given.
const usersHelp = `Usage: trading-journal users <subcommand> [args]

Subcommands:
  add <username> [--admin] [--email=foo@bar]
        Create a new user. Prompts for a password (twice).
  reset <username>
        Set a new password. Existing sessions are destroyed.
  list
        Print every user (id, username, admin flag, active flag, email).
  activate   <username>
        Re-enable a deactivated user.
  deactivate <username>
        Disable login + kill existing sessions. Data is preserved.
  promote <username>
        Grant admin.
  demote <username>
        Revoke admin.
  delete <username> [--yes]
        Permanently delete the user and ALL their data:
          - DB: accounts, trades, executions, journal entries (every
            scope), balance anchors, daily stats, sessions, custom
            journal templates, journal_entry links. sync_log history
            is anonymized (user_id -> NULL).
          - Files: journal attachment files, the user's
            imports/inbox/<name>/, imports/processed/<name>/,
            imports/failed/<name>/, and imports/uploads/<id>/.
        Requires retyping the username at the prompt unless --yes is
        passed. Refuses to remove the last active admin.

Global flags (recognized anywhere in the args):
  --config=PATH   Path to config.yaml (default: ./config.yaml)
`

// runUsersCommand dispatches ` + "`./trading-journal users <subcommand>`" + `. Called
// from main when os.Args[1] == "users". Returns a non-nil error to bubble up.
func runUsersCommand(args []string, cfgPath string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usersHelp)
		if len(args) == 0 {
			return fmt.Errorf("missing subcommand")
		}
		return nil
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	conn, err := db.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer conn.Close()
	if err := db.RunMigrations(conn, db.MigrationVars{BootstrapUsername: cfg.Auth.BootstrapUser}); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	q := models.New(conn)
	ctx := context.Background()

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add":
		return userAdd(ctx, conn, q, rest)
	case "reset":
		return userReset(ctx, q, rest)
	case "list":
		return userList(ctx, q)
	case "deactivate":
		return userSetActive(ctx, q, rest, false)
	case "activate":
		return userSetActive(ctx, q, rest, true)
	case "promote":
		return userSetAdmin(ctx, q, rest, true)
	case "demote":
		return userSetAdmin(ctx, q, rest, false)
	case "delete":
		return userDelete(ctx, conn, q, rest, cfg)
	default:
		fmt.Fprint(os.Stderr, usersHelp)
		return fmt.Errorf("unknown users subcommand %q", sub)
	}
}

func userAdd(ctx context.Context, conn *sql.DB, q *models.Queries, args []string) error {
	admin := false
	var username, email string
	for _, a := range args {
		switch {
		case a == "--admin":
			admin = true
		case strings.HasPrefix(a, "--email="):
			email = strings.TrimPrefix(a, "--email=")
		default:
			if username == "" && !strings.HasPrefix(a, "--") {
				username = a
			}
		}
	}
	if username == "" {
		return fmt.Errorf("usage: users add <username> [--admin] [--email=...]")
	}

	// Reject if the username already exists - admins can use `reset` to
	// change a password rather than silently double-creating.
	if _, err := q.GetUserByUsername(ctx, username); err == nil {
		return fmt.Errorf("user %q already exists; use `users reset %s` to change its password", username, username)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup: %w", err)
	}

	password, err := promptPassword("Password for " + username + ": ")
	if err != nil {
		return err
	}
	confirm, err := promptPassword("Confirm password: ")
	if err != nil {
		return err
	}
	if password != confirm {
		return fmt.Errorf("passwords do not match")
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	adminFlag := int64(0)
	if admin {
		adminFlag = 1
	}
	var emailPtr *string
	if email != "" {
		emailPtr = &email
	}
	row, err := q.CreateUser(ctx, models.CreateUserParams{
		Username:     username,
		Email:        emailPtr,
		PasswordHash: &hash,
		IsAdmin:      adminFlag,
		DisplayName:  &username,
	})
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	role := "user"
	if admin {
		role = "admin"
	}
	fmt.Printf("Created %s id=%d username=%s\n", role, row.ID, row.Username)
	return nil
}

func userReset(ctx context.Context, q *models.Queries, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: users reset <username>")
	}
	username := args[0]
	row, err := q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no such user: %s", username)
		}
		return err
	}

	password, err := promptPassword("New password for " + username + ": ")
	if err != nil {
		return err
	}
	confirm, err := promptPassword("Confirm password: ")
	if err != nil {
		return err
	}
	if password != confirm {
		return fmt.Errorf("passwords do not match")
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}
	if err := q.SetUserPassword(ctx, models.SetUserPasswordParams{
		PasswordHash: &hash,
		ID:           row.ID,
	}); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	// Drop existing sessions so the next request must re-authenticate.
	_ = q.DeleteSessionsForUser(ctx, row.ID)
	fmt.Printf("Password reset for %s; existing sessions destroyed\n", username)
	return nil
}

func userList(ctx context.Context, q *models.Queries) error {
	rows, err := q.ListUsers(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%-5s  %-20s  %-8s  %-8s  %s\n", "id", "username", "admin", "active", "email")
	for _, u := range rows {
		email := ""
		if u.Email != nil {
			email = *u.Email
		}
		adminTag := "-"
		if u.IsAdmin != 0 {
			adminTag = "yes"
		}
		activeTag := "yes"
		if u.IsActive == 0 {
			activeTag = "-"
		}
		fmt.Printf("%-5d  %-20s  %-8s  %-8s  %s\n", u.ID, u.Username, adminTag, activeTag, email)
	}
	return nil
}

func userSetActive(ctx context.Context, q *models.Queries, args []string, active bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: users (activate|deactivate) <username>")
	}
	row, err := q.GetUserByUsername(ctx, args[0])
	if err != nil {
		return fmt.Errorf("lookup: %w", err)
	}
	flag := int64(0)
	if active {
		flag = 1
	}
	if err := q.SetUserActive(ctx, models.SetUserActiveParams{IsActive: flag, ID: row.ID}); err != nil {
		return err
	}
	if !active {
		_ = q.DeleteSessionsForUser(ctx, row.ID)
	}
	state := "deactivated"
	if active {
		state = "activated"
	}
	fmt.Printf("User %s %s\n", row.Username, state)
	return nil
}

func userSetAdmin(ctx context.Context, q *models.Queries, args []string, admin bool) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: users (promote|demote) <username>")
	}
	row, err := q.GetUserByUsername(ctx, args[0])
	if err != nil {
		return fmt.Errorf("lookup: %w", err)
	}
	flag := int64(0)
	if admin {
		flag = 1
	}
	if err := q.SetUserAdmin(ctx, models.SetUserAdminParams{IsAdmin: flag, ID: row.ID}); err != nil {
		return err
	}
	state := "demoted to user"
	if admin {
		state = "promoted to admin"
	}
	fmt.Printf("User %s %s\n", row.Username, state)
	return nil
}

func userDelete(ctx context.Context, conn *sql.DB, q *models.Queries, args []string, cfg config.Config) error {
	confirmed := false
	var username string
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			confirmed = true
		default:
			if username == "" && !strings.HasPrefix(a, "--") {
				username = a
			}
		}
	}
	if username == "" {
		return fmt.Errorf("usage: users delete <username> [--yes]")
	}

	row, err := q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no such user: %s", username)
		}
		return err
	}

	// Refuse to remove the last active admin - otherwise the deployment
	// becomes unreachable for user management until somebody opens
	// the sqlite file by hand.
	if row.IsAdmin != 0 && row.IsActive != 0 {
		n, err := q.CountAdmins(ctx)
		if err != nil {
			return fmt.Errorf("count admins: %w", err)
		}
		if n <= 1 {
			return fmt.Errorf("refusing to delete %q: they are the last active admin. Promote another user first", username)
		}
	}

	if !confirmed {
		fmt.Fprintf(os.Stderr,
			"This will permanently delete user %q (id=%d) and ALL their accounts, trades,\n"+
				"executions, journal entries, balance anchors, daily stats, sessions, and\n"+
				"custom journal templates. sync_log history will be anonymized.\n"+
				"\n"+
				"Type the username to confirm: ", username, row.ID)
		typed, err := stdinReader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		typed = strings.TrimRight(typed, "\r\n")
		if typed != username {
			return fmt.Errorf("aborted (typed %q, expected %q)", typed, username)
		}
	}

	// Derive on-disk roots from cfg, matching the layout main.go uses
	// at server startup so the per-user dirs line up.
	importsRoot := cfg.Imports.Root
	dbDir := filepath.Dir(cfg.Database.Path)
	opts := auth.PurgeUserOptions{
		Username:       username,
		AttachmentsDir: filepath.Join(dbDir, "attachments"),
		InboxDir:       filepath.Join(importsRoot, "inbox"),
		ProcessedDir:   filepath.Join(importsRoot, "processed"),
		FailedDir:      filepath.Join(importsRoot, "failed"),
		UploadsDir:     filepath.Join(importsRoot, "uploads"),
	}

	stats, err := auth.PurgeUser(ctx, conn, row.ID, opts)
	if err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	fmt.Printf("Deleted user %s (id=%d): %d accounts, %d trades, %d attachment files removed; %d user dirs cleaned\n",
		username, row.ID, stats.Accounts, stats.Trades, stats.Attachments, stats.DirsRemoved)
	if stats.FailedFiles > 0 || stats.FailedDirs > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d file(s) and %d dir(s) could not be removed; see logs\n",
			stats.FailedFiles, stats.FailedDirs)
	}
	return nil
}

// promptPassword reads a password from stdin without echoing. Falls back to
// echoing line read when stdin isn't a terminal (e.g., piped from a file).
// stdinReader is a shared bufio reader so two successive promptPassword
// calls drain the same buffered stream rather than each constructing a new
// one and reading past the first line.
var stdinReader = bufio.NewReader(os.Stdin)

func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		buf, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(buf), "\r\n"), nil
	}
	s, err := stdinReader.ReadString('\n')
	if err != nil {
		// EOF after some bytes is fine for the last line of input.
		if err != io.EOF || s == "" {
			return "", err
		}
	}
	return strings.TrimRight(s, "\r\n"), nil
}
