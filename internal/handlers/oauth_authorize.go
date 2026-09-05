package handlers

import (
	"net/http"
	"net/url"

	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/oauth"
)

// OAuthAuthorizePage handles GET /oauth/authorize - the browser step of the
// OAuth flow. It sits on the session/proxy chain, so by the time it runs the
// human is authenticated (Authelia's Remote-User in proxy mode, the local
// session otherwise) and auth.MustUser(ctx) is the journal user the grant
// will be bound to. Validation happens in the open-redirect-safe order
// (client and redirect_uri first, never redirecting on those failures), then
// either the consent page is shown or, with mcp.oauth.consent=false, the code
// is issued straight away.
func (h *Handlers) OAuthAuthorizePage(w http.ResponseWriter, r *http.Request) {
	if h.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	req := oauth.ParseAuthorizeRequest(r.URL.Query())
	client, aerr := h.OAuth.ValidateAuthorize(r.Context(), req)
	if aerr != nil {
		h.oauthAuthorizeFail(w, r, req, aerr)
		return
	}
	if !h.OAuth.Cfg.Consent {
		h.oauthIssueAndRedirect(w, r, req)
		return
	}
	h.renderConsent(w, r, req, client)
}

// OAuthAuthorizeApprove handles the consent form POST. The hidden fields are
// re-validated from scratch - a form is just another untrusted request -
// and only an explicit "allow" mints a code; anything else is a denial the
// client learns about through the standard access_denied redirect.
func (h *Handlers) OAuthAuthorizeApprove(w http.ResponseWriter, r *http.Request) {
	if h.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	req := oauth.ParseAuthorizeRequest(r.PostForm)
	if _, aerr := h.OAuth.ValidateAuthorize(r.Context(), req); aerr != nil {
		h.oauthAuthorizeFail(w, r, req, aerr)
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		denied := &oauth.AuthorizeError{Code: oauth.ErrAccessDenied, Description: "the user denied the request", Redirectable: true}
		http.Redirect(w, r, oauth.ErrorRedirect(req.RedirectURI, denied, req.State), http.StatusFound)
		return
	}
	h.oauthIssueAndRedirect(w, r, req)
}

func (h *Handlers) oauthIssueAndRedirect(w http.ResponseWriter, r *http.Request, req oauth.AuthorizeRequest) {
	code, err := h.OAuth.IssueCode(r.Context(), userID(r.Context()), req)
	if err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, oauth.SuccessRedirect(req.RedirectURI, code, req.State), http.StatusFound)
}

// oauthAuthorizeFail reports a validation failure. Redirectable errors go
// back to the (already verified) redirect_uri as OAuth error parameters;
// everything else is rendered on our own page with a 400, because sending
// the browser to an unverified URI is exactly the open redirect the order
// of checks exists to prevent.
func (h *Handlers) oauthAuthorizeFail(w http.ResponseWriter, r *http.Request, req oauth.AuthorizeRequest, aerr *oauth.AuthorizeError) {
	if aerr.Redirectable {
		http.Redirect(w, r, oauth.ErrorRedirect(req.RedirectURI, aerr, req.State), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	h.renderPage(w, r, "oauth_consent.html", map[string]any{
		"NavActive": "",
		"Error":     aerr.Description,
	})
}

// renderConsent shows who is asking and what they get, with the whole
// request carried in hidden fields for the POST.
func (h *Handlers) renderConsent(w http.ResponseWriter, r *http.Request, req oauth.AuthorizeRequest, client *models.OauthClient) {
	scope, _ := oauth.NormalizeScope(req.Scope)
	host := ""
	if u, err := url.Parse(req.RedirectURI); err == nil {
		host = u.Host
	}
	h.renderPage(w, r, "oauth_consent.html", map[string]any{
		"NavActive":    "",
		"ClientName":   oauth.ClientDisplayName(*client),
		"RedirectHost": host,
		"Scope":        scope,
		"Fields":       req.Values(),
	})
}
