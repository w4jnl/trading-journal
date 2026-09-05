#!/usr/bin/env bash
# Driver/smoke harness for the trading-journal app.
#
# Boots a THROWAWAY instance (fresh scratch DB + scratch port, NEVER the
# repo's ./data/trading.db), seeds a real sample CSV so the importer/position
# builder run, then drives the app over HTTP: authenticated HTML pages (via
# proxy-auth header), the JSON API (via a directly-minted API key), and a
# server-rendered chart PNG. Prints PASS/FAIL per check.
#
# Why proxy auth + direct key insert instead of logging in: `users reset` is an
# interactive password prompt (no flag, reads the tty) that hangs under a
# non-tty/agent shell. Proxy mode authenticates HTML requests from a
# `Remote-User` header (no password), and the API uses key auth (independent of
# session) which we satisfy by inserting sha256(token) straight into api_keys.
#
# Usage:
#   .claude/skills/run-trading-journal/driver.sh            # build, smoke, teardown
#   PORT=19000 .claude/skills/run-trading-journal/driver.sh # custom port
#   KEEP=1 .claude/skills/run-trading-journal/driver.sh     # leave server + scratch up
#   SEED=0 .claude/skills/run-trading-journal/driver.sh     # skip CSV import
set -u

REPO=$(cd "$(dirname "$0")/../../.." && pwd)
PORT=${PORT:-18099}
SEED=${SEED:-1}
KEEP=${KEEP:-0}
SCRATCH=${SCRATCH:-${TMPDIR:-/tmp}/tj-run-smoke}
BIN="$SCRATCH/trading-journal"
BASE="http://127.0.0.1:$PORT"
TOKEN="tj_runskill_$(date +%s)_abcdefghij"
SRV=""
fails=0

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'; else shasum -a 256 | awk '{print $1}'; fi; }
say()   { printf '%s\n' "$*"; }
check() { if [ "$2" = "$3" ]; then say "PASS  $1 ($3)"; else say "FAIL  $1 (got $2, want $3)"; fails=$((fails+1)); fi; }
code()  { curl -s -o /dev/null -w '%{http_code}' "$@"; }   # last arg = url
cleanup() {
  [ -n "$SRV" ] && kill "$SRV" 2>/dev/null
  if [ "$KEEP" = "1" ]; then say ""; say "KEEP=1: server pid $SRV on $BASE, scratch $SCRATCH (not torn down)";
  else rm -rf "$SCRATCH"; fi
}
[ "$KEEP" = "1" ] || trap cleanup EXIT

say "== build =="
rm -rf "$SCRATCH"; mkdir -p "$SCRATCH/data" "$SCRATCH/imports/inbox" "$SCRATCH/oi_dashboard"
( cd "$REPO" && go build -o "$BIN" ./cmd/server ) || { say "FAIL build"; exit 1; }
say "built $BIN"

if [ "$SEED" = "1" ]; then
  cp "$REPO/exportData/MotivewaveExport.csv" "$SCRATCH/imports/inbox/" 2>/dev/null || true
  cp "$REPO/exportData/TopstepxExport.csv"   "$SCRATCH/imports/inbox/" 2>/dev/null || true
  say "seeded inbox: $(ls "$SCRATCH/imports/inbox/" | tr '\n' ' ')"
fi

cat > "$SCRATCH/config.yaml" <<YAML
server:
  port: $PORT
database:
  path: "$SCRATCH/data/trading.db"
imports:
  root: "$SCRATCH/imports"
oi_dashboards:
  root: "$SCRATCH/oi_dashboard"
backup:
  enabled: false
market_data:
  source: "yahoo"
  enabled: true
  prefetch: false
timezone: "America/Chicago"
mcp:
  enabled: true
  oauth:
    enabled: true
    issuer: "http://127.0.0.1:$PORT"
auth:
  mode: "proxy"
  bootstrap_user: "admin"
  session_secret: "run-skill-secret-0123456789-abcdefghij"
  cookie_secure: false
  proxy:
    user_header: "Remote-User"
    groups_header: "Remote-Groups"
    admin_group: "admin"
    trusted_proxies: ["127.0.0.1/32", "::1/128"]
    auto_provision: true
YAML

say ""; say "== boot (fresh DB auto-migrates; boot-sweep imports inbox) =="
"$BIN" -config "$SCRATCH/config.yaml" >"$SCRATCH/server.log" 2>&1 &
SRV=$!
curl -sf --retry 60 --retry-delay 1 --retry-connrefused "$BASE/healthz" >/dev/null \
  && check "healthz" 200 200 || { check "healthz" 000 200; say "--- server.log ---"; tail -20 "$SCRATCH/server.log"; exit 1; }

ntr=$(sqlite3 "$SCRATCH/data/trading.db" "SELECT count(*) FROM trades;")
say "trades imported: $ntr"
[ "$SEED" = "1" ] && check "import created trades" "$([ "$ntr" -gt 0 ] && echo yes || echo no)" yes

say ""; say "== MCP OAuth discovery (public documents, no auth) =="
check "/.well-known/oauth-authorization-server"     "$(code "$BASE/.well-known/oauth-authorization-server")" 200
check "AS metadata issuer == base URL"              "$(curl -s "$BASE/.well-known/oauth-authorization-server" | grep -c "\"issuer\":\"$BASE\"")" 1
check "/.well-known/oauth-protected-resource"       "$(code "$BASE/.well-known/oauth-protected-resource")" 200
check "/.well-known/oauth-protected-resource/mcp"   "$(code "$BASE/.well-known/oauth-protected-resource/mcp")" 200
check "PRM resource == base URL + /mcp"             "$(curl -s "$BASE/.well-known/oauth-protected-resource" | grep -c "\"resource\":\"$BASE/mcp\"")" 1

say ""; say "== authenticated HTML (proxy-auth header) =="
H=(-H "Remote-User: admin" -H "Remote-Groups: admin")
check "/dashboard"  "$(code "${H[@]}" "$BASE/dashboard")" 200
check "/trades"     "$(code "${H[@]}" "$BASE/trades")"    200
check "/journal"    "$(code "${H[@]}" "$BASE/journal")"   200
check "/calendar"   "$(code "${H[@]}" "$BASE/calendar")"  200
check "/settings/api-docs" "$(code "${H[@]}" "$BASE/settings/api-docs")" 200
curl -s "${H[@]}" "$BASE/settings/api-docs" | grep -q "Daily stats" && say "PASS  /settings/api-docs renders API.md" || { say "FAIL  /settings/api-docs"; fails=$((fails+1)); }
check "/ no header -> 401" "$(code "$BASE/")" 401
curl -s "${H[@]}" "$BASE/trades" | grep -q "<title>" && say "PASS  /trades renders HTML" || { say "FAIL  /trades HTML"; fails=$((fails+1)); }

say ""; say "== JSON API (key auth) =="
SHA=$(printf '%s' "$TOKEN" | sha256)
sqlite3 "$SCRATCH/data/trading.db" "INSERT INTO api_keys (user_id,name,key_prefix,key_sha256) VALUES (1,'runskill','tj_run','$SHA');"
A=(-H "Authorization: Bearer $TOKEN")
check "/api/v1 no key -> 401" "$(code "$BASE/api/v1")" 401
check "/api/v1 with key"      "$(code "${A[@]}" "$BASE/api/v1")" 200
check "/api/v1/trades"        "$(code "${A[@]}" "$BASE/api/v1/trades?page=1")" 200
check "/api/v1/strategies"    "$(code "${A[@]}" "$BASE/api/v1/strategies")" 200
check "/api/v1/stats/daily"   "$(code "${A[@]}" "$BASE/api/v1/stats/daily")" 200
check "/api/v1/stats/daily bad from -> 400" "$(code "${A[@]}" "$BASE/api/v1/stats/daily?from=nope")" 400
check "/api/v1/version"       "$(code "${A[@]}" "$BASE/api/v1/version")" 200

say ""; say "== chart PNG (visual artifact; warms bars from Yahoo -- needs network) =="
TID=$(sqlite3 "$SCRATCH/data/trading.db" "SELECT id FROM trades WHERE status='closed' LIMIT 1;")
if [ -n "$TID" ]; then
  curl -s -o /dev/null --max-time 25 "${H[@]}" "$BASE/charts/trade/$TID?load=1" || true   # warm cache
  png_code=$(curl -s -o "$SCRATCH/chart.png" -w '%{http_code}' "${A[@]}" "$BASE/api/v1/trades/$TID/chart.png")
  if [ "$png_code" = "200" ]; then say "PASS  chart.png rendered -> $SCRATCH/chart.png ($(wc -c <"$SCRATCH/chart.png" | tr -d ' ') bytes)";
  else say "SKIP  chart.png $png_code (no cached bars / offline; expected without Yahoo access)"; fi
fi

say ""; if [ "$fails" -eq 0 ]; then say "RESULT: ALL PASS"; else say "RESULT: $fails FAILED"; fi
exit "$fails"
