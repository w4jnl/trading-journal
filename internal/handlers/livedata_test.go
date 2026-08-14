package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/livedata"
)

// TestDataVersion covers the poll endpoint: per-user isolation, the global
// bump, and the no-store header that keeps proxies from caching a version.
func TestDataVersion(t *testing.T) {
	n := livedata.NewNotifier()
	h := &Handlers{LiveData: n}
	call := func(uid int64) (string, *httptest.ResponseRecorder) {
		ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})
		req := httptest.NewRequest("GET", "/data/version", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.DataVersion(rec, req)
		return strings.TrimSpace(rec.Body.String()), rec
	}

	body, rec := call(7)
	if body != `{"v":0}` {
		t.Errorf("fresh version = %s, want {\"v\":0}", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	// A bump for user 7 is invisible to user 8; BumpAll reaches both.
	n.Bump(7)
	if body, _ := call(7); body != `{"v":1}` {
		t.Errorf("after Bump(7): user 7 = %s, want {\"v\":1}", body)
	}
	if body, _ := call(8); body != `{"v":0}` {
		t.Errorf("after Bump(7): user 8 = %s, want {\"v\":0}", body)
	}
	n.BumpAll()
	if body, _ := call(7); body != `{"v":2}` {
		t.Errorf("after BumpAll: user 7 = %s, want {\"v\":2}", body)
	}
	if body, _ := call(8); body != `{"v":1}` {
		t.Errorf("after BumpAll: user 8 = %s, want {\"v\":1}", body)
	}
}
