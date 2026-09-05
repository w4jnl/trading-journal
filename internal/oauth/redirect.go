package oauth

import (
	"errors"
	"net/url"
	"strings"
)

// ErrRedirectURI is returned (wrapped with a reason) when a redirect URI is
// not acceptable for registration.
var ErrRedirectURI = errors.New("invalid redirect_uri")

// IsLoopbackHost reports whether host (already stripped of any port, i.e.
// url.URL.Hostname()) is a loopback address. Claude Code's native-app style
// callback lives on one of these; plain http is acceptable there because the
// traffic never leaves the user's machine (RFC 8252 section 8.3).
func IsLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ValidateRegisteredRedirectURI decides whether a client may register raw as a
// redirect target. Rules: absolute URL, no fragment (RFC 6749 3.1.2); https for
// any host (or only allowedHosts when that list is non-empty); http only for
// loopback hosts. Anything else - plain http to a real host, custom schemes,
// relative paths - is refused, which is the open-redirect guard at the door.
func ValidateRegisteredRedirectURI(raw string, allowedHosts []string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return errors.Join(ErrRedirectURI, errors.New("must be an absolute URL"))
	}
	if u.Fragment != "" {
		return errors.Join(ErrRedirectURI, errors.New("must not contain a fragment"))
	}
	host := u.Hostname()
	switch strings.ToLower(u.Scheme) {
	case "https":
		if len(allowedHosts) == 0 {
			return nil
		}
		for _, h := range allowedHosts {
			if strings.EqualFold(strings.TrimSpace(h), host) {
				return nil
			}
		}
		return errors.Join(ErrRedirectURI, errors.New("host is not in mcp.oauth.allowed_redirect_hosts"))
	case "http":
		if IsLoopbackHost(host) {
			return nil
		}
		return errors.Join(ErrRedirectURI, errors.New("plain http is only allowed for loopback hosts"))
	default:
		return errors.Join(ErrRedirectURI, errors.New("scheme must be https (or http on loopback)"))
	}
}

// RedirectURIMatches reports whether the redirect_uri a client presents at
// /oauth/authorize corresponds to one it registered. https URIs must match
// byte-for-byte (RFC 6749 3.1.2.3 "simple string comparison"). Loopback http
// URIs match on scheme + host + path + query but ignore the port, because
// native apps such as Claude Code bind an ephemeral port per run and RFC 8252
// section 7.3 requires servers to allow any port for loopback redirects. The
// hostname itself must still agree (localhost vs 127.0.0.1 are different
// registrations), and a non-loopback http URI never matches anything.
func RedirectURIMatches(registered, presented string) bool {
	if registered == "" || presented == "" {
		return false
	}
	ru, err := url.Parse(registered)
	if err != nil {
		return false
	}
	pu, err := url.Parse(presented)
	if err != nil {
		return false
	}
	if ru.Fragment != "" || pu.Fragment != "" {
		return false
	}
	if !strings.EqualFold(ru.Scheme, pu.Scheme) {
		return false
	}
	switch strings.ToLower(ru.Scheme) {
	case "https":
		return registered == presented
	case "http":
		rh, ph := ru.Hostname(), pu.Hostname()
		if !IsLoopbackHost(rh) || !IsLoopbackHost(ph) {
			return false
		}
		return strings.EqualFold(rh, ph) && ru.Path == pu.Path && ru.RawQuery == pu.RawQuery
	default:
		return false
	}
}

// MatchesAnyRedirectURI is the authorize-time check against the registered set.
func MatchesAnyRedirectURI(registered []string, presented string) bool {
	for _, r := range registered {
		if RedirectURIMatches(r, presented) {
			return true
		}
	}
	return false
}
