package main

import (
	"runtime/debug"
	"strings"
	"time"
)

// Version is stamped at build time via -ldflags "-X main.Version=<tag or sha>". The
// container build (Docker/Dockerfile) and CI (.github/workflows/docker.yml) already pass
// it (release tag, else the commit SHA). It's empty for a plain `go build`.
var Version string

// appVersion resolves the version string shown in the sidebar. Order:
//  1. the build-stamped Version (container/CI);
//  2. otherwise the VCS info Go embeds automatically for any `go build` inside a git
//     checkout (commit + build date + dirty flag) - this is how a hand-compiled binary
//     self-identifies without any ldflags;
//  3. "dev" when neither is available (e.g. `go run`).
func appVersion() string {
	if v := strings.TrimSpace(Version); v != "" {
		return shortenSHA(v)
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, ts string
		var dirty bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				ts = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			v := shortenSHA(rev)
			if dirty {
				v += "-dirty"
			}
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				v += " | " + t.Format("2006-01-02")
			}
			return v
		}
	}
	return "dev"
}

// shortenSHA trims a bare 40-char commit SHA (CI passes the full github.sha) to 12 chars for
// display; a semver tag or any shorter/non-hex string is returned unchanged.
func shortenSHA(v string) string {
	if len(v) >= 40 && isHex(v) {
		return v[:12]
	}
	return v
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
