package mwmail

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/config"
)

// startServer boots a Server on an ephemeral port and returns its address
// plus a shutdown func. The ephemeral port comes from pre-binding :0 and
// releasing it - a tiny race, acceptable in tests.
func startServer(t *testing.T, cfg config.MWEmail) (addr string, stop func()) {
	t.Helper()
	if cfg.ListenAddr == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.ListenAddr = ln.Addr().String()
		ln.Close()
	}
	srv := &Server{Cfg: cfg}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// Wait for the listener to come up.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", cfg.ListenAddr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return cfg.ListenAddr, func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("server exited with error: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("server did not stop within 5s")
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatalf("server never came up on %s", cfg.ListenAddr)
	return "", nil
}

// TestRunReturnsOnImmediateCancel: shutdown racing startup must still win.
// go-smtp's Close only closes listeners Serve has already registered, so the
// old ListenAndServe-in-a-goroutine shape could Close before the bind, leave
// the fresh listener accepting forever, and hang Run on its errCh - the
// TestArchiveReplay CI hang (its runOnce cancels milliseconds after boot). A
// context cancelled BEFORE Run makes that near-deterministic: Run reaches
// shutdown while the serve goroutine is still unscheduled.
func TestRunReturnsOnImmediateCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := &Server{Cfg: config.MWEmail{ArchiveDir: t.TempDir(), ListenAddr: addr}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel (close/listen race)")
	}
}

func archiveFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".eml") {
			names = append(names, e.Name())
		}
	}
	return names
}

const testMail = "From: MotiveWave <mw@localhost>\r\nTo: journal@localhost\r\nSubject: Order Filled: BOT 2 MESU6 @ 6,852.25\r\n\r\nAccount: APEX-1\r\nBOT 2 MESU6 @ 6,852.25\r\n"

func sendMail(addr string, auth smtp.Auth, msg string) error {
	return smtp.SendMail(addr, auth, "mw@localhost", []string{"journal@localhost"}, []byte(msg))
}

// TestCaptureAndDedup: accepted mail lands verbatim in the archive; a
// byte-identical resend is still accepted (250) but produces no second file
// - including across a server restart over the same archive dir.
func TestCaptureAndDedup(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startServer(t, config.MWEmail{ArchiveDir: dir})

	if err := sendMail(addr, nil, testMail); err != nil {
		t.Fatalf("send: %v", err)
	}
	files := archiveFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("archive files = %d, want 1", len(files))
	}
	raw, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "BOT 2 MESU6 @ 6,852.25") {
		t.Error("archived content does not match sent mail")
	}

	// Resend identical bytes: accepted, no new file.
	if err := sendMail(addr, nil, testMail); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if n := len(archiveFiles(t, dir)); n != 1 {
		t.Errorf("after resend: files = %d, want 1 (dedup)", n)
	}
	stop()

	// Restart over the same dir: dedup must survive (seeded from filenames).
	addr2, stop2 := startServer(t, config.MWEmail{ArchiveDir: dir})
	defer stop2()
	if err := sendMail(addr2, nil, testMail); err != nil {
		t.Fatalf("resend after restart: %v", err)
	}
	if n := len(archiveFiles(t, dir)); n != 1 {
		t.Errorf("after restart resend: files = %d, want 1 (dedup persisted)", n)
	}

	// A different message is a new capture.
	if err := sendMail(addr2, nil, testMail+"X"); err != nil {
		t.Fatalf("send different: %v", err)
	}
	if n := len(archiveFiles(t, dir)); n != 2 {
		t.Errorf("after new message: files = %d, want 2", n)
	}
}

// TestAuth: wrong password => SMTP-level auth error (535 path) and nothing
// archived; right password => accepted. PLAIN over a plain connection needs
// the AllowInsecureAuth escape hatch on both ends.
func TestAuth(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startServer(t, config.MWEmail{
		ArchiveDir: dir, Username: "mw", Password: "secret",
	})
	defer stop()
	host, _, _ := net.SplitHostPort(addr)

	// net/smtp refuses PLAIN over non-TLS unless the host is localhost;
	// 127.0.0.1 qualifies.
	if err := sendMail(addr, smtp.PlainAuth("", "mw", "wrong", host), testMail); err == nil {
		t.Fatal("wrong password accepted")
	} else if !strings.Contains(err.Error(), "535") && !strings.Contains(strings.ToLower(err.Error()), "auth") {
		t.Errorf("wrong-password error should be an auth failure, got: %v", err)
	}
	if n := len(archiveFiles(t, dir)); n != 0 {
		t.Errorf("archive after failed auth = %d files, want 0", n)
	}

	// Unauthenticated MAIL must be refused outright when creds are set.
	if err := sendMail(addr, nil, testMail); err == nil {
		t.Fatal("unauthenticated mail accepted despite required auth")
	}

	if err := sendMail(addr, smtp.PlainAuth("", "mw", "secret", host), testMail); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if n := len(archiveFiles(t, dir)); n != 1 {
		t.Errorf("archive after successful auth = %d files, want 1", n)
	}
}

// TestUnparseableStillAccepted: garbage content gets 250 + archived - parse
// problems must never bounce.
func TestUnparseableStillAccepted(t *testing.T) {
	dir := t.TempDir()
	addr, stop := startServer(t, config.MWEmail{ArchiveDir: dir})
	defer stop()
	if err := sendMail(addr, nil, "Subject: Connection restored\r\n\r\nnothing to parse here\r\n"); err != nil {
		t.Fatalf("unparseable mail rejected: %v", err)
	}
	if n := len(archiveFiles(t, dir)); n != 1 {
		t.Errorf("archive = %d files, want 1", n)
	}
}

// TestIngestHook: a real-format fill mail reaches the Ingest hook with the
// parsed Result; test emails and unparseable mail do not. The SMTP reply is
// 250 in every case, including when the hook errors.
func TestIngestHook(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []Result

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := &Server{
		Cfg: config.MWEmail{ArchiveDir: dir, ListenAddr: addr},
		Ingest: func(ctx context.Context, res Result) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, res)
			return "inserted", nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	fill, err := os.ReadFile(filepath.Join("testdata", "fill_real_mw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sendMail(addr, nil, string(fill)); err != nil {
		t.Fatalf("send fill: %v", err)
	}
	testMsg, err := os.ReadFile(filepath.Join("testdata", "test_email.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sendMail(addr, nil, string(testMsg)); err != nil {
		t.Fatalf("send test email: %v", err)
	}
	if err := sendMail(addr, nil, "Subject: Connection restored\r\n\r\nnothing here\r\n"); err != nil {
		t.Fatalf("send unparseable: %v", err)
	}

	mu.Lock()
	if len(got) != 1 {
		mu.Unlock()
		t.Fatalf("ingest hook calls = %d, want 1 (fill only)", len(got))
	}
	r := got[0]
	if r.OrderID != "172416726" || r.Account != "LFE050-EXAMPLE1-TEST018" || r.Qty != 3 {
		t.Errorf("hook got %+v", r)
	}
	mu.Unlock()

	// Live ingest journals the message: a restart over the same archive dir
	// must not re-offer it to the hook via the boot replay.
	cancel()
	<-done
	var replayCalls int
	var rmu sync.Mutex
	replayed := make(chan struct{})
	srv2 := &Server{
		Cfg: config.MWEmail{ArchiveDir: dir, ListenAddr: addr},
		Ingest: func(ctx context.Context, res Result) (string, error) {
			rmu.Lock()
			defer rmu.Unlock()
			replayCalls++
			return "inserted", nil
		},
		replayed: func() { close(replayed) },
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- srv2.Run(ctx2) }()
	select {
	case <-replayed: // the boot sweep has walked the whole archive
	case <-time.After(5 * time.Second):
		t.Fatal("boot replay did not finish")
	}
	cancel2()
	<-done2
	rmu.Lock()
	defer rmu.Unlock()
	if replayCalls != 0 {
		t.Errorf("restart replayed %d live-ingested fills, want 0", replayCalls)
	}
}

// TestArchiveReplay: files already in the archive at boot are replayed
// through the Ingest hook (oldest first) without any SMTP traffic; test
// emails and unparseable captures are filtered out.
func TestArchiveReplay(t *testing.T) {
	dir := t.TempDir()
	fill, err := os.ReadFile(filepath.Join("testdata", "fill_real_mw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	testMsg, err := os.ReadFile(filepath.Join("testdata", "test_email.eml"))
	if err != nil {
		t.Fatal(err)
	}
	// Pre-seed the archive as a previous run would have left it. The second
	// fill is a later cumulative snapshot of the same order (qty 3 -> the
	// timestamps in the names force replay order oldest-first).
	early := strings.Replace(string(fill), "Quantity: 3", "Quantity: 1", 1)
	writeArchived := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeArchived("20260727T071931.000000000Z_aaaaaaaaaaaa.eml", early)
	writeArchived("20260727T071933.000000000Z_bbbbbbbbbbbb.eml", string(fill))
	writeArchived("20260727T080000.000000000Z_cccccccccccc.eml", string(testMsg))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	var mu sync.Mutex
	var qtys []float64
	srv := &Server{
		Cfg: config.MWEmail{ArchiveDir: dir, ListenAddr: addr},
		Ingest: func(ctx context.Context, res Result) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			qtys = append(qtys, res.Qty)
			return "inserted", nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	// The sweep is complete when the JOURNAL holds all three outcomes (two
	// fills + the test email) - that write is the sweep's last act per file.
	// Polling the ingest count instead raced the third file's journal write:
	// a cancel issued right after the second ingest stopped the sweep before
	// the test email was journaled, and the assertion below flaked.
	journal := filepath.Join(dir, replayJournalName)
	journalEntries := func() int {
		raw, _ := os.ReadFile(journal)
		return len(strings.Fields(string(raw))) / 2
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && journalEntries() < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	if len(qtys) != 2 {
		mu.Unlock()
		t.Fatalf("replayed fills = %d, want 2 (test email must be filtered)", len(qtys))
	}
	if qtys[0] != 1 || qtys[1] != 3 {
		t.Errorf("replay order qtys = %v, want [1 3] (oldest first)", qtys)
	}
	mu.Unlock()

	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("replay journal: %v", err)
	}
	if n := len(strings.Fields(strings.ReplaceAll(string(raw), "\n", " "))) / 2; n != 3 {
		t.Fatalf("journal entries = %d, want 3:\n%s", n, raw)
	}

	// Second server over the same dir: everything already_done, hook silent.
	runOnce := func() int {
		var calls int
		var cmu sync.Mutex
		srv2 := &Server{
			Cfg: config.MWEmail{ArchiveDir: dir, ListenAddr: addr},
			Ingest: func(ctx context.Context, res Result) (string, error) {
				cmu.Lock()
				defer cmu.Unlock()
				calls++
				return "inserted", nil
			},
		}
		ctx2, cancel2 := context.WithCancel(context.Background())
		done2 := make(chan error, 1)
		go func() { done2 <- srv2.Run(ctx2) }()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cmu.Lock()
			n := calls
			cmu.Unlock()
			if n >= 2 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel2()
		<-done2
		cmu.Lock()
		defer cmu.Unlock()
		return calls
	}
	if n := runOnce(); n != 0 {
		t.Errorf("second boot replayed %d fills, want 0 (journal must suppress)", n)
	}

	// Escape hatch: deleting the journal forces a full re-pass.
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if n := runOnce(); n != 2 {
		t.Errorf("post-delete boot replayed %d fills, want 2", n)
	}
}

// TestImplicitTLS: tls_mode=implicit serves TLS-wrapped SMTP (465-style).
func TestImplicitTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t)
	addr, stop := startServer(t, config.MWEmail{
		ArchiveDir: dir, TLSMode: "implicit", TLSCert: certPath, TLSKey: keyPath,
	})
	defer stop()

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	host, _, _ := net.SplitHostPort(addr)
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		t.Fatalf("smtp client: %v", err)
	}
	if err := c.Mail("mw@localhost"); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("journal@localhost"); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte(testMail)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("data close: %v", err)
	}
	_ = c.Quit()

	if n := len(archiveFiles(t, dir)); n != 1 {
		t.Errorf("archive over implicit TLS = %d files, want 1", n)
	}
}

// writeSelfSigned generates a throwaway self-signed cert/key pair on disk.
func writeSelfSigned(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	cf, _ := os.Create(certPath)
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	kf, _ := os.Create(keyPath)
	pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return certPath, keyPath
}
