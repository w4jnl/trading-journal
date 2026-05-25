package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/config"
	"github.com/w4jnl/trading-journal/internal/db"
	"github.com/w4jnl/trading-journal/internal/models"
)

// runUsersCommand dispatches `./trading-journal users <subcommand>`. Called
// from main when os.Args[1] == "users". Returns a non-nil error to bubble up.
func runUsersCommand(args []string, cfgPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: trading-journal users <add|reset|list> ...")
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
	default:
		return fmt.Errorf("unknown users subcommand %q (try add|reset|list|activate|deactivate|promote|demote)", sub)
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

// promptPassword reads a password from stdin without echoing. Falls back to
// echoing line read when stdin isn't a terminal (e.g., piped from a file).
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(syscall.Stdin)) {
		buf, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(buf), "\r\n"), nil
	}
	br := bufio.NewReader(os.Stdin)
	s, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}
