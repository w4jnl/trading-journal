# Development

[‹ README](../README.md)

Go 1.26 or newer, no cgo, no JavaScript build. The iOS app needs Xcode and xcodegen on a Mac.

```sh
make build        # ./trading-journal from ./cmd/server, version from the git tag
make test         # go test -race ./...
make vet          # go vet ./...
make smoke        # boots a throwaway instance on a scratch port, imports a sample, hits every route

make ios-project  # app/ios/TradingJournal.xcodeproj from project.yml (never committed)
make ios-test     # the unit tests on the iPhone simulator
make ios-screens  # screenshots of every tab against a running journal (SERVER=… KEY=… SHOTS=…)
```

Hot reload: `air` with the committed `.air.toml`. CI (`.github/workflows/ci.yml`) runs gofmt,
vet, the race tests, `sqlc diff`, a Docker build and govulncheck on every push and pull request;
`ios.yml` runs the Swift tests when `app/ios` changes; `docker.yml` publishes the multi-arch
image when a release is published.

## Layout

```
cmd/server/          main, the users and mwmail subcommands
internal/
  handlers/          every HTTP route: pages, HTMX partials, /api/v1, /mcp, OAuth, settings
  journal/           the single write path for entries (pages, API, MCP, AI reviews share it)
  importer/          CSV parsers, e-mail ingest, the import service, the inbox watcher
  positions/         the FIFO position builder (executions -> trades)
  copyset/           copy-set detection across accounts
  dayplan/ guardrail/ coach/   the plan, the intraday nudges, the rule checks
  levels/ excursion/   the stop-and-target lab grid, MFE/MAE from cached bars
  ai/                providers, review and task runners, briefs
  marketdata/ chartpng/   bar cache (Yahoo, TopstepX feed), server-side chart PNGs
  topstepx/ ibkr/ mwmail/ syncqueue/ markethours/   live sources and their scheduling
  gex/ gexview/      gex-engine client and the Gamma pages' view models
  push/ (apns/)      APNs client, nudges, silent sync, the Session Live Activity
  auth/ oauth/ secretbox/ qr/   sessions, proxy mode, the OAuth 2.1 server, encryption, QR
  livedata/          per-user data version (page polling, SSE)
  backup/            daily snapshots and pre-migration backups
  config/ db/ models/   YAML config, SQLite open + migration runner, sqlc output
web/
  templates/         html/template pages (*.html) and partials (_*.html), embedded
  static/            app.css (the whole design system), vanilla JS, vendored htmx, uPlot,
                     lightweight-charts, the Inter and JetBrains Mono fonts
app/ios/             the SwiftUI app and its extensions (see app/ios/README.md)
Docker/              Dockerfile, compose file, container config
docs/                these pages; API.md at the root is the API reference
scripts/demo/        a synthetic instance for screenshots (never real data)
assets/              brand files and the README images, with their HTML sources
```

## Database

SQLite through `modernc.org/sqlite`. Migrations are dbmate-format files in
`internal/db/migrations/`, embedded and applied at start; the binary refuses to run against a
schema newer than itself. Queries are raw SQL in `internal/db/queries/` compiled by sqlc into
`internal/models/`; CI fails when the committed output is stale:

```sh
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

Keep the comments in `.sql` files ASCII-only: sqlc miscounts multi-byte characters inside
comments and silently truncates the next query.

Invariants worth knowing before changing anything: all times are stored in UTC and converted to
the configured timezone at render time; `executions` is the system of record and `trades`,
`daily_stats` and the copy sets are derived caches rebuilt by the position builder; futures P&L
always goes through the instrument's tick size and value; every mutation bumps the per-user
data version after commit, or open pages and the guardrail never learn about it; the Content
Security Policy allows no inline script without the per-request nonce. `CLAUDE.md` has the
longer list.

## Running an instance for development

`make smoke` (the driver in `.claude/skills/run-trading-journal/`) boots a throwaway instance
with proxy auth, so no password is needed: HTML requests carry a `Remote-User` header, and an
API key is minted by inserting its SHA-256 into `api_keys`. `KEEP=1` leaves it running and
prints the URL and the key. `scripts/demo/seed.sh up` does the same with twelve weeks of
synthetic trades, notes and plans, which is what the README screenshots are taken from
(`scripts/demo/README.md`). Never point a development instance at a real database.

## Tests

Go tests use a temporary migrated database per test (`internal/db/dbtest`) and `httptest`; the
route smoke test fails on any inline script the CSP would block, and
`TestAPISurfaceDocsInSync` fails when a route is missing from `API.md`, the OpenAPI document or
the discovery index. The iOS tests decode captured fixtures (`make ios-fixtures` recaptures and
scrubs them) and drive the client over a fake transport; with `SERVER` and `KEY` set the live
write tests run against a journal and leave it as found.

## Releasing

The server and the app share one version. Date the `Unreleased` section of `CHANGELOG.md`, set
`MARKETING_VERSION` in `app/ios/project.yml`, tag `vX.Y.Z` and publish a GitHub release with the
changelog section as its notes; the Docker workflow builds and pushes the image from the release.
`make ios-testflight` archives and uploads the app from the tag.

## Images

`assets/brand/render.sh` renders the hero, social preview and avatar from their HTML with
headless Chrome; `assets/readme/render.sh` renders the diagrams in both themes; `scripts/demo/`
seeds an instance and takes the screenshots and the GIF. Every image in the README has its
source next to it.
