package main

import "testing"

func TestShortenSHA(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.2.3", "v1.2.3"},   // semver tag: unchanged
		{"abc1234", "abc1234"}, // short sha: unchanged
		{"0123456789abcdef0123456789abcdef01234567", "0123456789ab"},                                       // 40-hex -> 12
		{"main-0123456789abcdef0123456789abcdef01234567", "main-0123456789abcdef0123456789abcdef01234567"}, // not pure hex: unchanged
		{"", ""},
	}
	for _, c := range cases {
		if got := shortenSHA(c.in); got != c.want {
			t.Errorf("shortenSHA(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAppVersionUsesStamp(t *testing.T) {
	orig := Version
	defer func() { Version = orig }()

	Version = "v9.9.9"
	if got := appVersion(); got != "v9.9.9" {
		t.Errorf("stamped tag: appVersion() = %q, want v9.9.9", got)
	}
	Version = "0123456789abcdef0123456789abcdef01234567"
	if got := appVersion(); got != "0123456789ab" {
		t.Errorf("stamped 40-hex sha: appVersion() = %q, want 0123456789ab", got)
	}
	// Empty stamp -> ReadBuildInfo fallback (a commit under `go test`) or "dev". Either way,
	// never empty.
	Version = ""
	if got := appVersion(); got == "" {
		t.Error("empty stamp: appVersion() must not be empty (fallback or 'dev')")
	}
}
