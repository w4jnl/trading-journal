# Demo instance, screenshots and the README recording

A throwaway trading journal full of invented data, so the README images can be made again on
any machine without showing anyone's real trades.

```sh
scripts/demo/seed.sh up                 # build, boot on :18777, import, write notes and plans
cd scripts/demo && npm install          # Playwright for Node; uses the installed Chrome
node shots.mjs                          # assets/readme/*.png
npx playwright install ffmpeg           # once: the helper Playwright records video with
node demo.mjs                           # assets/readme/demo.gif (needs ffmpeg on the PATH too)
cd ../.. && scripts/demo/seed.sh down   # stop it and delete the scratch directory
```

What `seed.sh up` does: `gen-data.py` writes a MotiveWave fill export (an Apex eval and a Lucid
funded account traded together by a copier, and an AMP live account on CQG, where the E-mini is
called EP) and a TopstepX order export, twelve weeks of ES, MES, NQ and MNQ day trading ending
on 2026-10-16, with copy sets, scale-ins, a long-to-short flip and a position still open. The
server imports them from its inbox like any drop; `seed-content.py` then names the accounts,
adds strategies, day plans, notes with tags and labels, a group note on a copy set, a plan on
the open position, balance anchors and a few lessons. The random seed is fixed, so the result
is the same every time.

Everything is synthetic: account references, tickets, prices and results are made up. The
daily closes come from Yahoo's ES=F and NQ=F history so the fills sit where the market was,
but the intraday prices around them are invented, which is why the scripts leave the replay
charts unloaded. The instance runs under `$TMPDIR/tj-demo` (or `TJ_DEMO_DIR`) with proxy auth
(`Remote-User: demo`) and an API key of kind `device` printed at the end; the repository's own
`data/` directory is never touched.

Screenshots are 1440x900 CSS px at two device pixels per CSS px (`TJ_SHOT_SCALE=1.5` for
smaller files); the theme comes from the app's `?theme=` parameter. `TJ_DEMO_DAY` picks the day
page.
