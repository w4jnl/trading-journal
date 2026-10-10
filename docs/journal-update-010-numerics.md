# Trading-journal — update 010: numerics round 2

**Date:** 2026-10-09 · **Engine change:** decisions #49–#58
(`w4jnl/gamma-exposure`) · **Engine version:** v0.12.0 (v0.12.1 adds `data_quality.straddle`). Everything
additive; two fields gain a sibling, one label changes meaning, and the
*values* of the 0DTE-sensitive levels move.

## Why

Review round 2 found the engine pricing the vendor's calendar-time IV on
trading-time T — 5× too much variance for a mid-session 0DTE, so ATM 0DTE
gamma came out at 0.45× and the rest sat in the wings. v0.12.0 prices on
the calendar clock (validated against the market's own straddle to 0.1%
and against MenthorQ's 0DTE levels on the P0 sessions, where it now hits
them exactly). The archived sessions are being recomputed in place.

## What moves (same fields, new values)

- **0DTE levels**: `call_wall` / `put_wall` / `gamma_wall` with
  `bucket: "0dte"` and `"0dte_vol"`, `flip_0dte`, `flip_intraday`,
  `net_gex_dte`, `expiring_net_gex`, `skew.*`. On the archived sessions the
  0DTE gamma wall moved a median 0–2 level buckets (p90 +10 on ES), the
  0DTE net ×1.5–1.7, `skew.dte0_pct` by −10 to −13 points.
- **Whole-book walls** (`bucket: "all"`): unchanged on ~60% of cycles; where
  the front-week wings used to co-determine the wall it moves to the
  long-dated magnet (ES 10-09: put wall 7700 → 7500). The ≥6-day book is
  stable (95% within one bucket). `regime.net_gex_total` ×1.04 (ES) /
  ×1.20 (NQ): the strike window is tenor-scaled, so the total approaches
  an all-strike sum.
- **Repaired rows**: the sessions since 2026-09-29 (and the ten most
  recent archived ones) are recomputed in place. A repaired row keeps its
  `as_of` (±1–3 s: the recompute's own stamp), carries `provenance.kind:
  "recomputed"` with `reason: "v0.12.0 numerics"`, `engine_version` and
  `settings_hash`, and keeps any degraded verdict the old row had. **A row
  you have cached may therefore change** — re-read the gamma context for
  trades on or after 2026-09-29 if you want the corrected levels; nothing
  forces you to.

## New fields

- **`expected_move_next_session`** (`/v1/levels`, `/v1/snapshots`): the
  ATM straddle of the first expiry settling after the session's cash close,
  index points — the true next-session move. `expected_move_1d` is
  unchanged in value: it is the front live expiry's straddle, which
  intraday is the move *to this session's close* (it shrinks through the
  day) and at the close the next session's. If you ever used
  `expected_move_1d` from an intraday row as a one-day move, switch to the
  new field. Null before v0.12.0.
- **`skew.dte0_rr_pts` / `skew.m1_rr_pts`**: the 25Δ risk reversal in vol
  points (put IV − call IV) beside `dte0_pct` / `m1_pct`; a tenor is null
  when its wings' deltas do not match.
- **`provenance.engine_version`, `settings_hash`, `replaced[]`,
  `carried_degraded[]`** on repaired rows (`/v1/snapshots`).
- **`data_quality.straddle`** (v0.12.1, decision #59): `{expiry, root,
  strike, market_mid, model_mid, rel_err, t_hours, at_floor}` — the index
  leg's front ATM straddle repriced with its vendor IVs on the engine's
  clock vs the market mid, the clock's own health check (runs within ±1%;
  a `STRADDLE_MISPRICED` notice beyond 10% outside the last half hour).
  Null on rows from before v0.12.1. Optional to show; the quality strip
  renders it as "straddle ±x.x%".

## Semantics you should know

- **Post-close rows** (16:20–16:30 and any reconstruction after the cash
  close) are priced at the official close, never at a futures mark; the
  16:15 row keeps the day's 0DTE book. The 1D band anchors on a measured
  (or exactly recomputed) closing row, never on a reconstruction.
- **AM-settled monthlies** stay in the book on their Thursday evening
  (they settle at Friday's 09:30 print); Thursday-evening `net_gex_dte`
  and `expiring_net_gex` change accordingly.
- **Vendor comparison**: MenthorQ/SpotGamma levels are per underlying;
  compare the `SPX` / `NDX` markets with them, not the ES/NQ blend.

## Conventions

Additive only; no new `levels[].type` values; `expected_move_1d` and
every existing field keep their meaning. Rows persisted before v0.12.0
carry `null` for the new fields unless repaired.
