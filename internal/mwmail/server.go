// Package mwmail embeds a minimal SMTP listener that captures MotiveWave's
// "Send Email On Filled Order" notifications. It is a capture layer, not a
// mail server: no relaying, no mailboxes, no outbound mail, LAN/VPN only.
// Every received message is archived verbatim to disk (the audit trail and
// parser corpus) and parsed; when an Ingest hook is wired and the message
// carries a complete fill, it is handed over for PROVISIONAL ingestion -
// near-real-time executions that the authoritative MotiveWave CSV import
// later supersedes via the shared (account, order id) key.
package mwmail

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/w4jnl/trading-journal/internal/config"
)

// IngestFunc hands a fully-parsed fill notification to the journal for
// provisional ingestion. It returns a short action word for the capture log
// ("inserted", "updated", "skipped_live_sync", ...). Errors are logged and
// swallowed by the caller - ingest problems must never bounce mail back at
// MotiveWave.
type IngestFunc func(ctx context.Context, res Result) (string, error)

// Server runs the embedded SMTP listener per the mw_email config section.
type Server struct {
	Cfg    config.MWEmail
	Logger *slog.Logger // nil -> slog.Default()
	// Ingest, when non-nil, receives every captured message that parses to a
	// complete fill (IngestReady). Nil keeps the listener in capture-only mode.
	Ingest IngestFunc

	archive *archive
	ctx     context.Context // Run's ctx; scopes ingest DB work to app lifetime
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Run starts the listener and blocks until ctx is cancelled (app shutdown)
// or the listener fails. The archive dir is created and the dedup set seeded
// from it before accepting mail, so restarts never re-capture a message that
// is already on disk.
func (s *Server) Run(ctx context.Context) error {
	arch, err := newArchive(s.Cfg.ArchiveDir)
	if err != nil {
		return fmt.Errorf("mwmail: archive: %w", err)
	}
	s.archive = arch
	s.ctx = ctx

	srv := smtp.NewServer(&backend{server: s})
	srv.Addr = s.Cfg.ListenAddr
	srv.Domain = "trading-journal.local"
	srv.MaxMessageBytes = 1 << 20 // MW notifications are tiny; 1 MiB is generous
	srv.MaxRecipients = 16
	srv.ReadTimeout = 30 * time.Second
	srv.WriteTimeout = 30 * time.Second
	// MW connects in plain on port-25-style setups (LAN/VPN only by policy);
	// without this go-smtp refuses to advertise AUTH on a non-TLS session and
	// MW's login fails opaquely.
	srv.AllowInsecureAuth = true

	errCh := make(chan error, 1)
	switch s.Cfg.TLSMode {
	case "implicit":
		// Implicit TLS (465-style): wrap the LISTENER, not STARTTLS config.
		cert, err := tls.LoadX509KeyPair(s.Cfg.TLSCert, s.Cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("mwmail: load tls keypair: %w", err)
		}
		ln, err := tls.Listen("tcp", s.Cfg.ListenAddr, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err != nil {
			return fmt.Errorf("mwmail: listen (implicit tls): %w", err)
		}
		go func() { errCh <- srv.Serve(ln) }()
	case "starttls":
		cert, err := tls.LoadX509KeyPair(s.Cfg.TLSCert, s.Cfg.TLSKey)
		if err != nil {
			return fmt.Errorf("mwmail: load tls keypair: %w", err)
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		go func() { errCh <- srv.ListenAndServe() }()
	default: // "off"
		go func() { errCh <- srv.ListenAndServe() }()
	}

	s.logger().Info("mw email capture listening",
		"addr", s.Cfg.ListenAddr, "tls_mode", s.Cfg.TLSMode,
		"auth", s.Cfg.Username != "", "archive_dir", s.Cfg.ArchiveDir,
		"seeded_dedup", arch.size(), "ingest", s.Ingest != nil)

	select {
	case <-ctx.Done():
		_ = srv.Close()
		<-errCh // wait for Serve to return
		return nil
	case err := <-errCh:
		if err != nil && !errors.Is(err, smtp.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("mwmail: serve: %w", err)
		}
		return nil
	}
}

// authRequired reports whether AUTH is mandatory (both creds configured).
func (s *Server) authRequired() bool {
	return s.Cfg.Username != "" && s.Cfg.Password != ""
}

// checkCreds constant-time-compares a presented pair against config.
func (s *Server) checkCreds(username, password string) bool {
	u := subtle.ConstantTimeCompare([]byte(username), []byte(s.Cfg.Username))
	p := subtle.ConstantTimeCompare([]byte(password), []byte(s.Cfg.Password))
	return u == 1 && p == 1
}

// backend hands each connection a fresh session.
type backend struct {
	server *Server
}

func (b *backend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &session{server: b.server}, nil
}

// session implements smtp.Session + smtp.AuthSession. It only tracks
// whether the client authenticated; the whole capture contract lives in
// Data (handle.go).
type session struct {
	server *Server
	authed bool
	from   string
	to     []string
}

func (s *session) AuthMechanisms() []string {
	if !s.server.authRequired() {
		return nil
	}
	return []string{sasl.Plain, sasl.Login}
}

// Auth returns a SASL server for the requested mechanism. Wrong credentials
// yield smtp.ErrAuthFailed - a proper 535 the MW mailer understands, never a
// swallowed error (which stalls it).
func (s *session) Auth(mech string) (sasl.Server, error) {
	verify := func(username, password string) error {
		if !s.server.checkCreds(username, password) {
			s.server.logger().Warn("mwmail: auth failed", "username", username)
			return smtp.ErrAuthFailed
		}
		s.authed = true
		return nil
	}
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return verify(username, password)
		}), nil
	case sasl.Login:
		return newLoginServer(verify), nil
	default:
		return nil, smtp.ErrAuthUnknownMechanism
	}
}

func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.server.authRequired() && !s.authed {
		return smtp.ErrAuthRequired
	}
	s.from = from
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.server.authRequired() && !s.authed {
		return smtp.ErrAuthRequired
	}
	s.to = append(s.to, to)
	return nil
}

func (s *session) Reset() {
	s.from = ""
	s.to = nil
}

func (s *session) Logout() error { return nil }

// loginServer is a minimal SASL server for the legacy LOGIN mechanism
// (username and password each sent in response to a challenge). go-sasl
// ships a LOGIN client but no server; MW may pick LOGIN over PLAIN, so we
// provide it.
type loginServer struct {
	verify   func(username, password string) error
	username string
	step     int
}

func newLoginServer(verify func(username, password string) error) sasl.Server {
	return &loginServer{verify: verify}
}

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.step {
	case 0:
		l.step++
		if len(response) > 0 {
			// Client sent the username as an initial response.
			l.username = string(response)
			l.step++
			return []byte("Password:"), false, nil
		}
		return []byte("Username:"), false, nil
	case 1:
		l.username = string(response)
		l.step++
		return []byte("Password:"), false, nil
	case 2:
		l.step++
		if err := l.verify(l.username, string(response)); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	default:
		return nil, true, smtp.ErrAuthFailed
	}
}
