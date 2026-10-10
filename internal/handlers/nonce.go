package handlers

import "context"

// The per-request CSP nonce. securityHeaders (cmd/server) mints one per
// request, puts it in the policy's script-src and in the request context;
// renderPage / renderPageFragment hand it to templates as .Nonce, and every
// inline <script> carries nonce="{{$.Nonce}}". Scripts htmx swaps in later
// get it from htmx.config.inlineScriptNonce (set from the same value in
// base.html). Nothing else in the page may run inline.
type nonceKey struct{}

// WithNonce stores the request's script nonce on ctx.
func WithNonce(ctx context.Context, nonce string) context.Context {
	return context.WithValue(ctx, nonceKey{}, nonce)
}

// NonceFrom returns the request's script nonce ("" outside a request).
func NonceFrom(ctx context.Context) string {
	n, _ := ctx.Value(nonceKey{}).(string)
	return n
}
