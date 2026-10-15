#!/bin/sh
# Answers the screenshot walker's push requests: it writes SHOTS/push-<name> when it wants a
# payload delivered, and only simctl can reach the simulator. Runs beside `make ios-screens`:
#   SHOTS=/dir app/ios/TJScreens/push-driver.sh [udid]
# until the walker writes SHOTS/done or 15 minutes pass.
set -eu
shots=${SHOTS:?SHOTS, the walker screenshot directory}
udid=${1:-booted}
payloads=$(dirname "$0")/../TJTests/Payloads
deadline=$(( $(date +%s) + 900 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  for f in "$shots"/push-*; do
    [ -e "$f" ] || continue
    name=${f##*/push-}
    if [ -f "$payloads/$name.apns" ]; then
      xcrun simctl push "$udid" nl.w4j.tradingjournal "$payloads/$name.apns" >/dev/null 2>&1 && echo "pushed $name" || echo "push $name failed"
    else
      echo "no payload $name"
    fi
    rm -f "$f"
  done
  [ -e "$shots/done" ] && exit 0
  sleep 1
done
