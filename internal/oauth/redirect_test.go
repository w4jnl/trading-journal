package oauth

import (
	"errors"
	"testing"
)

func TestValidateRegisteredRedirectURI(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		allowed []string
		wantErr bool
	}{
		{"chatgpt https", "https://chatgpt.com/connector_platform_oauth_redirect", nil, false},
		{"claude.ai https", "https://claude.ai/api/mcp/auth_callback", nil, false},
		{"claude code loopback localhost", "http://localhost:52341/callback", nil, false},
		{"claude code loopback 127.0.0.1", "http://127.0.0.1:52341/callback", nil, false},
		{"loopback ipv6", "http://[::1]:8080/callback", nil, false},
		{"https allowlisted", "https://claude.ai/api/mcp/auth_callback", []string{"chatgpt.com", "claude.ai"}, false},
		{"https not allowlisted", "https://evil.example/cb", []string{"chatgpt.com", "claude.ai"}, true},
		{"plain http real host", "http://example.com/cb", nil, true},
		{"plain http private ip", "http://192.168.1.5/cb", nil, true},
		{"fragment", "https://claude.ai/cb#frag", nil, true},
		{"relative", "/callback", nil, true},
		{"custom scheme", "myapp://callback", nil, true},
		{"empty", "", nil, true},
	}
	for _, c := range cases {
		err := ValidateRegisteredRedirectURI(c.raw, c.allowed)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: ValidateRegisteredRedirectURI(%q) err=%v, wantErr=%v", c.name, c.raw, err, c.wantErr)
		}
		if err != nil && !errors.Is(err, ErrRedirectURI) {
			t.Errorf("%s: error must wrap ErrRedirectURI, got %v", c.name, err)
		}
	}
}

// https redirects match byte-for-byte; loopback http redirects ignore the port
// (RFC 8252 7.3) but nothing else; plain http never matches.
func TestRedirectURIMatches(t *testing.T) {
	cases := []struct {
		name       string
		registered string
		presented  string
		want       bool
	}{
		{"https exact", "https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback", true},
		{"https trailing slash differs", "https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/", false},
		{"https query differs", "https://chatgpt.com/cb", "https://chatgpt.com/cb?x=1", false},
		{"https port differs", "https://claude.ai/cb", "https://claude.ai:8443/cb", false},
		{"https case of host differs is strict", "https://claude.ai/cb", "https://CLAUDE.ai/cb", false},
		{"loopback any port", "http://localhost:1234/callback", "http://localhost:9876/callback", true},
		{"loopback registered without port", "http://localhost/callback", "http://localhost:41234/callback", true},
		{"loopback 127.0.0.1 any port", "http://127.0.0.1:5555/callback", "http://127.0.0.1:6666/callback", true},
		{"loopback ipv6 any port", "http://[::1]:5555/callback", "http://[::1]:6666/callback", true},
		{"loopback path differs", "http://localhost:1234/callback", "http://localhost:1234/other", false},
		{"loopback host differs", "http://localhost:1234/callback", "http://127.0.0.1:1234/callback", false},
		{"loopback query differs", "http://localhost/cb", "http://localhost/cb?x=1", false},
		{"http non-loopback never", "http://example.com/cb", "http://example.com/cb", false},
		{"scheme differs", "https://localhost/cb", "http://localhost/cb", false},
		{"fragment rejected", "https://claude.ai/cb", "https://claude.ai/cb#x", false},
		{"empty", "", "https://claude.ai/cb", false},
	}
	for _, c := range cases {
		if got := RedirectURIMatches(c.registered, c.presented); got != c.want {
			t.Errorf("%s: RedirectURIMatches(%q, %q) = %v, want %v", c.name, c.registered, c.presented, got, c.want)
		}
	}
	reg := []string{"https://claude.ai/api/mcp/auth_callback", "http://localhost/callback"}
	if !MatchesAnyRedirectURI(reg, "http://localhost:3000/callback") || MatchesAnyRedirectURI(reg, "https://evil.example/cb") {
		t.Errorf("MatchesAnyRedirectURI set semantics wrong")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for h, want := range map[string]bool{"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "::1": true,
		"127.0.0.2": false, "example.com": false, "10.0.0.1": false, "": false} {
		if got := IsLoopbackHost(h); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", h, got, want)
		}
	}
}
