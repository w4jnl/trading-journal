#!/usr/bin/env bash
# A throwaway trading journal full of invented trades, for screenshots and demos.
#
#   scripts/demo/seed.sh up      build, boot on port 18777 with a fresh database, import the
#                                synthetic exports, write notes, plans and strategies
#   scripts/demo/seed.sh url     print the base URL
#   scripts/demo/seed.sh down    stop the server and delete the scratch directory
#
# Everything lives under $TJ_DEMO_DIR (default $TMPDIR/tj-demo); the repository's own data/
# directory is never touched. Pages are read with the proxy-auth header `Remote-User: demo`,
# the API with the key printed at the end (kept in $TJ_DEMO_DIR/key).
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
PORT=${PORT:-18777}
DIR=${TJ_DEMO_DIR:-${TMPDIR:-/tmp}/tj-demo}
BASE="http://127.0.0.1:$PORT"
TOKEN="tj_demo_0000000000000000000000000000"

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'; else shasum -a 256 | awk '{print $1}'; fi; }

down() {
  if [ -f "$DIR/pid" ]; then kill "$(cat "$DIR/pid")" 2>/dev/null || true; fi
  rm -rf "$DIR"
}

up() {
  down
  mkdir -p "$DIR/data" "$DIR/imports/inbox" "$DIR/oi_dashboard"
  (cd "$REPO" && go build -o "$DIR/trading-journal" ./cmd/server)
  python3 "$HERE/gen-data.py" "$DIR/gen" >/dev/null
  cp "$DIR/gen/"*.csv "$DIR/imports/inbox/"
  cat >"$DIR/config.yaml" <<YAML
server:
  port: $PORT
database:
  path: "$DIR/data/trading.db"
imports:
  root: "$DIR/imports"
oi_dashboards:
  root: "$DIR/oi_dashboard"
backup:
  enabled: false
market_data:
  source: "yahoo"
  enabled: true
  prefetch: false
mcp:
  enabled: true
ai:
  enabled: false
timezone: "America/Chicago"
auth:
  mode: "proxy"
  bootstrap_user: "demo"
  session_secret: "demo-only-secret-0123456789-abcdefghij"
  cookie_secure: false
  proxy:
    user_header: "Remote-User"
    groups_header: "Remote-Groups"
    admin_group: "admin"
    trusted_proxies: ["127.0.0.1/32", "::1/128"]
    auto_provision: true
YAML
  "$DIR/trading-journal" -config "$DIR/config.yaml" >"$DIR/server.log" 2>&1 &
  echo $! >"$DIR/pid"
  curl -sf --retry 60 --retry-delay 1 --retry-connrefused "$BASE/healthz" >/dev/null
  # the boot sweep imports the inbox; wait until both files have moved and trades exist
  for _ in $(seq 1 60); do
    left=$(ls "$DIR/imports/inbox" | wc -l | tr -d ' ')
    n=$(sqlite3 "$DIR/data/trading.db" "SELECT count(*) FROM trades;" 2>/dev/null || echo 0)
    [ "$left" = "0" ] && [ "$n" -gt 0 ] && break
    sleep 1
  done
  if ls "$DIR/imports/failed" >/dev/null 2>&1 && [ -n "$(ls "$DIR/imports/failed" 2>/dev/null)" ]; then
    echo "seed: an import failed:" >&2; cat "$DIR"/imports/failed/*.error.txt >&2; exit 1
  fi
  sqlite3 "$DIR/data/trading.db" "INSERT INTO api_keys (user_id, name, key_prefix, key_sha256, kind, device_name) VALUES (1, 'demo phone', 'tj_demo', '$(printf '%s' "$TOKEN" | sha256)', 'device', 'Demo iPhone');"
  printf '%s' "$TOKEN" >"$DIR/key"
  python3 "$HERE/seed-content.py" "$DIR/data/trading.db" "$BASE" "$TOKEN"
  echo "demo journal: $BASE  (pages: header 'Remote-User: demo'; API key: $TOKEN)"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  url) echo "$BASE" ;;
  *) echo "usage: $0 up|down|url" >&2; exit 2 ;;
esac
