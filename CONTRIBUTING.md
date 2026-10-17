# Contributing

The trading journal is a personal tool, published because the approach may be useful to others.
Read [Scope and status](README.md#scope-and-status) first: features are the ones my own trading
needs, and requests that do not fit (other asset classes, order entry, a single-page
app) will probably be declined, politely.

What is welcome:

- Bug reports with a reproduction and the relevant log lines, secrets and account numbers
  removed (the issue form asks for them). A CSV that the importer mishandles is the most useful
  attachment there is; replace the account refs and keep the rows.
- Fixes with a test. Go tests use a temporary migrated database per test
  (`internal/db/dbtest`) and `httptest`, never a mock of the database; `make vet`, `make test`
  (`go test -race ./...`) and `make smoke` must pass, and CI also checks gofmt, `sqlc diff`, the
  Docker build and govulncheck. Swift changes pass `make ios-test`.
- An importer for a platform you trade on, when it fits the existing shape: a parser into the
  same `Fill` records, detection from the header row, a test against a scrubbed sample.
- Small, focused pull requests. Describe what changed and what you verified, in plain words.

Before a larger change, open an issue and ask; it saves both of us a rewrite.

Things that are easy to break by accident, all enforced or documented somewhere:

- Times are stored in UTC and converted at render time. `executions` is the system of record;
  `trades`, `daily_stats` and the copy sets are rebuilt from it. Futures P&L goes through the
  instrument's tick size and value.
- Every mutation bumps the per-user data version after commit, or open pages, the phone and the
  guardrail never learn about it.
- No inline script without the per-request nonce; the route smoke test fails on one.
- Comments in `internal/db/queries/*.sql` and the migrations are ASCII only (sqlc miscounts
  multi-byte characters inside comments and truncates the next query).
- A new API route is listed in `API.md`, `api_openapi.json` and the discovery index, or
  `TestAPISurfaceDocsInSync` fails.

`CLAUDE.md` is the longer list of conventions, written for an AI coding assistant and valid for
a human too. The [Development](docs/development.md) page has the layout and the make targets.
