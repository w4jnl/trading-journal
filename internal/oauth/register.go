package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/w4jnl/trading-journal/internal/models"
)

// registerRequest is the RFC 7591 client metadata we act on. Clients send
// more (client_uri, logo_uri, contacts, software_id, ...); unknown fields
// are ignored rather than rejected so a new client attribute never breaks
// registration.
type registerRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

// RegisterResponse is the RFC 7591 section 3.2.1 registration response. No
// client_secret: every client is public and proves possession via PKCE.
type RegisterResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

const maxClientNameRunes = 64

// RegisterHandler implements dynamic client registration. It is public by
// necessity (a connector registers before any human is involved), so it
// hands out nothing but an identifier: a client_id alone grants no access -
// a token only exists after a signed-in user approves this client at
// /oauth/authorize, and the redirect targets are validated here so that
// approval can never be sent anywhere but a registered https or loopback URI.
func (s *Server) RegisterHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if preflight(w, r, http.MethodPost) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidClientMetadata, "body must be a JSON object")
			return
		}

		if len(req.RedirectURIs) == 0 {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidRedirectURI, "redirect_uris is required")
			return
		}
		uris := make([]string, 0, len(req.RedirectURIs))
		seen := map[string]bool{}
		for _, raw := range req.RedirectURIs {
			raw = strings.TrimSpace(raw)
			if err := ValidateRegisteredRedirectURI(raw, s.Cfg.AllowedRedirectHosts); err != nil {
				writeOAuthError(w, http.StatusBadRequest, ErrInvalidRedirectURI, raw+": "+reason(err))
				return
			}
			if !seen[raw] {
				seen[raw] = true
				uris = append(uris, raw)
			}
		}

		grants := req.GrantTypes
		if len(grants) == 0 {
			grants = []string{"authorization_code", "refresh_token"}
		}
		for _, g := range grants {
			if g != "authorization_code" && g != "refresh_token" {
				writeOAuthError(w, http.StatusBadRequest, ErrInvalidClientMetadata, "unsupported grant_type "+g)
				return
			}
		}
		responses := req.ResponseTypes
		if len(responses) == 0 {
			responses = []string{"code"}
		}
		for _, rt := range responses {
			if rt != "code" {
				writeOAuthError(w, http.StatusBadRequest, ErrInvalidClientMetadata, "unsupported response_type "+rt)
				return
			}
		}
		if m := req.TokenEndpointAuthMethod; m != "" && m != "none" {
			writeOAuthError(w, http.StatusBadRequest, ErrInvalidClientMetadata,
				"only public clients are supported (token_endpoint_auth_method must be \"none\")")
			return
		}

		name := strings.TrimSpace(req.ClientName)
		if utf8.RuneCountInString(name) > maxClientNameRunes {
			rs := []rune(name)
			name = string(rs[:maxClientNameRunes])
		}
		var namePtr *string
		if name != "" {
			namePtr = &name
		}

		clientID, _, err := NewOpaque(PrefixClientID)
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, ErrServerError, "could not generate client_id")
			return
		}
		urisJSON, _ := json.Marshal(uris)
		row, err := s.Queries.CreateOAuthClient(r.Context(), models.CreateOAuthClientParams{
			ClientID: clientID, ClientName: namePtr, RedirectUris: string(urisJSON),
		})
		if err != nil {
			s.logger().Error("oauth: register client", "err", err)
			writeOAuthError(w, http.StatusInternalServerError, ErrServerError, "could not store client")
			return
		}
		s.logger().Info("oauth: client registered", "client_id", clientID[:12]+"...", "name", name, "redirect_hosts", hostsOf(uris))

		writeJSON(w, http.StatusCreated, RegisterResponse{
			ClientID:                row.ClientID,
			ClientIDIssuedAt:        time.Now().Unix(),
			ClientName:              name,
			RedirectURIs:            uris,
			GrantTypes:              grants,
			ResponseTypes:           responses,
			TokenEndpointAuthMethod: "none",
			Scope:                   strings.TrimSpace(req.Scope),
		})
	})
}

// reason strips the ErrRedirectURI sentinel so the client sees just the
// human-readable part.
func reason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i >= 0 {
		return msg[i+1:]
	}
	return msg
}

func hostsOf(uris []string) []string {
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		if p, err := url.Parse(u); err == nil {
			out = append(out, p.Host)
		}
	}
	return out
}

// ClientRedirectURIs decodes the stored JSON list; a corrupt row yields nil,
// which matches nothing.
func ClientRedirectURIs(c models.OauthClient) []string {
	var uris []string
	if err := json.Unmarshal([]byte(c.RedirectUris), &uris); err != nil {
		return nil
	}
	return uris
}

// ClientDisplayName is what the consent page and Settings show for a client.
func ClientDisplayName(c models.OauthClient) string {
	if c.ClientName != nil && strings.TrimSpace(*c.ClientName) != "" {
		return strings.TrimSpace(*c.ClientName)
	}
	id := c.ClientID
	if len(id) > 12 {
		id = id[:12] + "..."
	}
	return "an unnamed app (" + id + ")"
}

var errUnknownClient = errors.New("unknown client")
