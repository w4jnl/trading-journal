package handlers

import "net/http"

// RenderLogin renders the login page using the standard template pipeline.
// Exported so auth.LocalAuth can hand it off as its LoginTemplate callback.
// The request is intentionally nil: the login page is reached pre-auth, so
// there's no CurrentUser to inject. CSRF is also unnecessary here - the
// /login POST is exempt from the CSRF check (no existing session to bind to).
func (h *Handlers) RenderLogin(w http.ResponseWriter, data map[string]any) {
	h.renderPage(w, nil, "login.html", data)
}
