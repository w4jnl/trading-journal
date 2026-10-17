#!/usr/bin/env bash
# Renders hero.html, social.html and avatar.html to their PNGs with headless Chrome. The fonts
# are the repo's own woff2 files under web/static/fonts, loaded over file://, which is why the
# file-access flag is there; nothing is fetched from the network.
set -euo pipefail
cd "$(dirname "$0")"
CHROME=${CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
shot() { # page WxH out
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --allow-file-access-from-files \
    --default-background-color="${BG:-14181A}" --force-device-scale-factor=1 --window-size="$2" --virtual-time-budget=10000 \
    --screenshot="$PWD/$3" "file://$PWD/$1" 2>/dev/null
  printf '%s ' "$3"; sips -g pixelWidth -g pixelHeight "$3" | tail -2 | awk '{printf "%s ", $2}'; echo
}
shot hero.html 1280,320 trading-journal-hero.png
shot social.html 1280,640 trading-journal-social.png
BG=00000000 shot avatar.html 460,460 trading-journal-avatar.png   # transparent corners
