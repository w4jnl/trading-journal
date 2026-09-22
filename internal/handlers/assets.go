package handlers

import "strings"

// AssetVersion reduces a build version to a cache key safe for both an ETag
// (RFC 7232 forbids spaces and quotes) and a query string: only
// [A-Za-z0-9._-] survive, runs of anything else collapse to one dash.
// Empty for development builds ("dev", or a "-dirty" checkout), whose
// assets may change without the version changing and therefore must not be
// cached under a fixed key.
func AssetVersion(version string) string {
	v := strings.TrimSpace(version)
	if v == "" || v == "dev" || strings.Contains(v, "dirty") {
		return ""
	}
	var b strings.Builder
	dash := false
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// assetQuery is the "?v=..." suffix templates append to /static URLs so a
// new build is fetched immediately while older ones stay cached for a year
// (see staticCacheHeaders in cmd/server). Empty for development builds.
func (h *Handlers) assetQuery() string {
	if v := AssetVersion(h.Version); v != "" {
		return "?v=" + v
	}
	return ""
}
