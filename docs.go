// Package tradingjournal is the module root. It exists only to embed the
// repo-root documentation that lives next to the README - go:embed cannot
// reach a parent directory, and API.md must stay at the root (README links
// it) as the single source of truth for the HTTP API, which the app also
// serves in-app under Settings -> API docs.
package tradingjournal

import _ "embed"

// APIDoc is API.md: the human reference for /api/v1.
//
//go:embed API.md
var APIDoc []byte
