package handlers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
)

func TestIbkrGatewayHostAllowed(t *testing.T) {
	h := &Handlers{IbkrGatewayHosts: []string{"ibgw.example.internal", "192.168.30.0/24", " 10.1.2.3 "}}
	user := auth.WithUser(context.Background(), &auth.User{ID: 2, Username: "u"})
	admin := auth.WithUser(context.Background(), &auth.User{ID: 1, Username: "a", IsAdmin: true})
	for _, tc := range []struct {
		ctx  context.Context
		host string
		ok   bool
	}{
		{user, "ibgw.example.internal", true},
		{user, "IBGW.EXAMPLE.INTERNAL", true}, // hostnames are case-insensitive
		{user, "192.168.30.21", true},         // inside the CIDR
		{user, "192.168.31.21", false},
		{user, "10.1.2.3", true}, // IP literal entry, trimmed
		{user, "traefik.internal", false},
		{user, "127.0.0.1", false},
		{user, "", false},
		{admin, "anything.at.all", true}, // admins may register any host
		{admin, "127.0.0.1", true},
	} {
		if got := h.ibkrGatewayHostAllowed(tc.ctx, tc.host); got != tc.ok {
			t.Errorf("host %q (admin=%v) = %v, want %v", tc.host, auth.UserFrom(tc.ctx).IsAdmin, got, tc.ok)
		}
	}
	// No allowlist: non-admins cannot add gateways at all.
	if (&Handlers{}).ibkrGatewayHostAllowed(user, "ibgw.example.internal") {
		t.Error("empty allowlist admitted a non-admin host")
	}
}

// A staged path must be exactly <uploads>/upload-u<uid>-*/<file>; anything
// else is refused, and the cleanup never removes a non-staging directory.
func TestStagedPathShapeAndCleanup(t *testing.T) {
	root := t.TempDir()
	h := &Handlers{UploadsDir: root}
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	mine := mk("upload-u7-abc123/fills.csv")
	theirs := mk("upload-u8-def456/fills.csv")
	stray := mk("loose.csv")
	nested := mk("upload-u7-zzz/sub/fills.csv")
	outside := filepath.Join(filepath.Dir(root), "elsewhere.csv")

	for _, tc := range []struct {
		p   string
		uid int64
		ok  bool
	}{
		{mine, 7, true},
		{theirs, 7, false},  // another user's staging dir
		{theirs, 8, true},   // ... is fine for its owner
		{stray, 7, false},   // file directly under the root
		{nested, 7, false},  // one level deep only
		{outside, 7, false}, // outside the root
		{root, 7, false},    // the root itself
		{"", 7, false},
		{filepath.Join(root, "upload-u7-abc123"), 7, false}, // the dir, not a file in it
	} {
		if got := h.stagedPathSafe(tc.p, tc.uid); got != tc.ok {
			t.Errorf("stagedPathSafe(%q, %d) = %v, want %v", tc.p, tc.uid, got, tc.ok)
		}
	}

	// Cleanup of a hostile path leaves the uploads root alone...
	h.removeStagedDir(stray)
	h.removeStagedDir(filepath.Join(root, "x"))
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("cleanup of a stray path removed other staging dirs: %v", err)
	}
	// ... while a genuine staging dir is removed.
	h.removeStagedDir(mine)
	if _, err := os.Stat(filepath.Dir(mine)); !os.IsNotExist(err) {
		t.Errorf("staging dir not removed: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("uploads root removed: %v", err)
	}

	// stageUpload stamps the owner into the directory name.
	p, cleanup, err := stageUpload(strings.NewReader("a,b\n"), "x.csv", root, 42)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !h.stagedPathSafe(p, 42) || h.stagedPathSafe(p, 7) {
		t.Errorf("staged file %q not scoped to its uploader", p)
	}
}
