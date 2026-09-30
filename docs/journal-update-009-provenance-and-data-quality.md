# Trading-journal — update 009: data quality and provenance

**Date:** 2026-09-29 · **Engine change:** decisions #38–#46
(`w4jnl/gamma-exposure`) · **Engine version:** v0.11.0. Everything additive;
nothing you read today moved.

## Why

A vendor host move on 2026-09-28 took every fetch down for a full session,
and the review that followed found that degraded rows (the 09:30–09:50
zeroed-quote window, mismatched legs) were feeding the percentile, Δ1H/Δ1D
and flip times unflagged. The engine now says, per row, how much to trust
it and where it came from — and fills outage gaps with flagged rows.

## New fields (all three endpoints: `/v1/levels`, `/v1/profile`, `/v1/snapshots`)

- **`data_quality`** — `{degraded, warnings[], funnel, xcheck, rate_source,
  snapshot_age_seconds, quote_effective_at}`; each warning is
  `{code, severity, symbol, message}`, severity `degraded` | `notice`.
  `degraded: true` means at least one degraded warning (`EMPTY_CHAIN`,
  `PARTIAL_CHAIN`, `STALE_LEG`, `RATIO_DRIFT`, `FROZEN_SPOT`, repeated
  `STALE_PAYLOAD`). Null on rows persisted before v0.11.0.
- **`provenance`** — `{kind, chain_as_of, spot_source, spot_as_of, reason}`;
  `kind` is `measured` (a live cycle), `recomputed` (rebuilt exactly from the
  archived payloads of that slot) or `reconstructed` (an older chain
  repriced at the slot's spot after a feed outage; `chain_as_of` is the
  chain vintage, `as_of` the grid moment the row describes). Null before
  v0.11.0 (= measured).
- **`pc_volume_legs`** — `{SPX: {put, call, ratio}, SPY: {…}}` on blend views:
  the P/C split per leg, so index and ETF contract counts stop mixing.

## What you may want to do with them

1. **Nearest-snapshot picking for a trade**: skip rows with
   `data_quality.degraded == true` when choosing the snapshot nearest an
   entry/exit (step to the previous/next row); the engine's own history
   statistics already do.
2. **Gamma-context annotation**: grey the block, or suffix "(degraded)",
   when the chosen row is degraded; suffix "(reconstructed)" when
   `provenance.kind == "reconstructed"` — its walls are near-exact
   (OI-driven) but its flip/regime carry the older chain's IVs.
3. **Market page / Exposure tab**: an optional small strip from
   `data_quality.warnings` (the dashboard renders code + message with a
   severity class); or embed `/fragments/quality/{market}` like the other
   partials (`hx-trigger="load, every 60s"`).
4. **Nulls**: render as "—" per your convention; never a measured zero.

## Semantics you should know

- **The 16:00 row keeps the 0DTE book** (decision #43): expiry status is now
  judged at the quote-effective moment (payload timestamp − 15 min), so the
  closing row's `expiring_net_gex` is non-zero and `net_gex_change_1d`
  compares like with like. `expected_move_1d` at the close is still the next
  session's straddle, as before.
- **Reconstructed rows sit exactly on the grid minute** (e.g.
  `14:35:00Z`), measured rows 1–3 s after it; your +120 s buffer still
  resolves both.
- **Backfill never overwrites**: a row you have seen does not change; new
  rows may appear in past slots (after an outage) with `kind` set.
- **`/health`** gained `reasons` / `notices` (why `status` is what it is) and
  per-event failure state under `scheduler`; the old keys keep their
  meaning.

## Conventions

Additive only; consumers matching on known `levels[].type` values are
unaffected (no new level types this round).
