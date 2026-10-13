package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"strings"
)

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

// StaticHash is a short hash of the static files' paths and contents: the
// cache key of a development build, whose version cannot tell one stylesheet
// from the next. Walked in lexical order, so the same files give the same key.
func StaticHash(fsys fs.FS) string {
	sum := sha256.New()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		io.WriteString(sum, p)
		sum.Write([]byte{0})
		sum.Write(b)
		return nil
	})
	if err != nil {
		return ""
	}
	return hex.EncodeToString(sum.Sum(nil))[:12]
}

// assetQuery is the "?v=..." suffix templates append to /static URLs so a
// new build is fetched immediately while older ones stay cached for a year
// (see staticCacheHeaders in cmd/server). A development build keys on the
// content instead (DevAssetHash): its assets are still served no-cache, but
// a changed file gets a new URL, so no browser or proxy cache can answer it
// with an old copy.
func (h *Handlers) assetQuery() string {
	if v := AssetVersion(h.Version); v != "" {
		return "?v=" + v
	}
	if h.DevAssetHash != "" {
		return "?v=dev-" + h.DevAssetHash
	}
	return ""
}
