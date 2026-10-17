#!/usr/bin/env bash
# Renders the README diagrams from their HTML sources with headless Chrome, at 2x, dark and light:
#   architecture.html -> architecture-dark.png, architecture-light.png
#   ecosystem.html    -> ecosystem-dark.png, ecosystem-light.png
# The fonts are the repo's own woff2 files, loaded over file:// (hence the file-access flag);
# nothing is fetched from the network. Each page fixes its own height in a <style> rule, and H
# below must match it.
set -euo pipefail
cd "$(dirname "$0")"
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
shot() { # page H theme
  local out="${1%.html}-$3.png"
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --allow-file-access-from-files \
    --force-device-scale-factor=2 --window-size="1200,$2" --virtual-time-budget=10000 \
    --screenshot="$PWD/$out" "file://$PWD/$1?theme=$3" 2>/dev/null
  printf '%s ' "$out"; sips -g pixelWidth -g pixelHeight "$out" | tail -2 | awk '{printf "%s ", $2}'; echo
}
for t in dark light; do
  shot architecture.html 834 "$t"
  shot ecosystem.html 724 "$t"
done
