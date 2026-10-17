#!/usr/bin/env python3
"""Fill the demo instance with what a trader writes: account names, strategies, day plans,
notes with tags and labels, a group note on a copy set, a plan on the open position, balance
anchors and a few lessons. Runs after the CSVs are imported.

    seed-content.py DB_PATH BASE_URL API_KEY

Writes that the REST API offers go through it (device key); what the API refuses by design
(past day plans, account names, lessons) goes straight into the SQLite file. Everything is
invented; see gen-data.py for the fills.
"""
import json
import sqlite3
import sys
import urllib.request

DB, BASE, KEY = sys.argv[1], sys.argv[2].rstrip("/"), sys.argv[3]


def api(method, path, body=None):
    req = urllib.request.Request(BASE + path, method=method, data=json.dumps(body).encode() if body is not None else None)
    req.add_header("Authorization", "Bearer " + KEY)
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


db = sqlite3.connect(DB)
db.row_factory = sqlite3.Row
cur = db.cursor()

# --- accounts: names, types, the copier group -------------------------------------------
ACCOUNTS = {
    "APEX-250001-11": ("Apex eval 50k", "prop_eval", "apex", "copier"),
    "LFE050-000123-4": ("Lucid funded", "prop_funded", "lucid", "copier"),
    "120999": ("AMP live", "live", "amp", None),
    "EXPRESS-V2-250000-99": ("Topstep 50k", "prop_eval", "topstep", None),
}
for ref, (name, typ, firm, grp) in ACCOUNTS.items():
    cur.execute("UPDATE accounts SET name=?, type=?, firm=?, needs_setup=0, copy_group=? WHERE account_ref=?", (name, typ, firm, grp, ref))
acct = {r["account_ref"]: r["id"] for r in cur.execute("SELECT id, account_ref FROM accounts")}

# --- strategies ---------------------------------------------------------------------------
STRATEGIES = {
    "ORB fade": "## Opening-range fade\n\nWhen the first fifteen minutes run more than one ATR from the open and stall at a level from the plan, fade the extreme back towards the open. Stop beyond the extreme, first target the open, runner to VWAP.\n\n- Only on days with an overnight range inside the prior day\n- Not after a gap of more than half an ATR\n",
    "VWAP reclaim": "## VWAP reclaim\n\nAfter a drive away from VWAP and a return, enter on the first pullback that holds the reclaim. Stop under the pullback low. Target the high of the drive.\n",
    "Trend pullback": "## Trend pullback\n\nTrend day only (price one side of VWAP all morning). Enter on the pullback to the 20 EMA on the two-minute chart with a hold above the prior swing. Stop under the swing, trail with the EMA.\n",
}
for name, body in STRATEGIES.items():
    cur.execute("INSERT OR IGNORE INTO strategies (user_id, name, body) VALUES (1, ?, ?)", (name, body))
strat = {r["name"]: r["id"] for r in cur.execute("SELECT id, name FROM strategies")}

# --- day plans for the last weeks (past days are written on the day page, not via the API) --
days = [r["d"] for r in cur.execute("SELECT DISTINCT date(datetime(substr(exit_time,1,19), '-5 hours')) AS d FROM trades WHERE status='closed' ORDER BY d DESC LIMIT 14")]
PLANS = [
    ("long", "7700 overnight low\n7745 VWAP from Thursday\n7790 call wall", 4, "11:30", 400, "Friday: size down, two decisions before the open range is set."),
    ("neutral", "7760 prior close\n7805 week high", 5, "12:00", 400, "Range day likely. Fade the edges, nothing in the middle."),
    ("short", "7850 gap fill\n7810 overnight low\n7770 put wall", 4, "11:30", 400, ""),
    ("long", "7820 open\n7860 call wall", 4, "11:00", 300, "News at 09:30 CT. No entries five minutes either side."),
    ("neutral", "7850\n7900", 5, "12:00", 400, ""),
    ("long", "7740 reclaim level\n7800", 4, "11:30", 400, "Trend pullback only if VWAP holds."),
    ("flat", "", None, "", None, "Travel day. No trades."),
    ("short", "7780\n7740 Monday low", 4, "11:30", 400, ""),
    ("long", "7700\n7760", 5, "12:00", 400, ""),
    ("neutral", "7720\n7780", 4, "11:30", 400, "Reduce to one contract after two losses."),
]
for d, (bias, levels, max_trades, stop_time, max_loss, notes) in zip(days, PLANS):
    cur.execute(
        "INSERT OR REPLACE INTO day_plans (user_id, day, bias, levels, max_trades, stop_time, max_loss, notes, created_at, updated_at) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ? || ' 12:45:00 +0000 UTC', ? || ' 12:45:00 +0000 UTC')",
        (d, bias, levels, max_trades, stop_time, max_loss, notes, d, d),
    )

# --- lessons (an in-app review names one per note; here a few from past reviews) ----------
LESSONS = [
    (days[3], "trade", "early-exit", "Let the runner reach the planned target; the first target is for the first half only.", None),
    (days[6], "trade", "chased", "Wait for the retest instead of buying the break.", None),
    (days[9], "day", "overtraded", "After the fourth decision the edge is gone; close the platform.", days[2]),
    (days[11], "trade", "moved-stop", "A stop moved once is a stop you did not believe in. Take the loss where planned.", None),
]
for day, scope, key, text, learned in LESSONS:
    cur.execute("INSERT INTO lessons (user_id, day, scope, mistake_key, text, learned_at) VALUES (1, ?, ?, ?, ?, ?)", (day, scope, key, text, (learned + " 15:00:00 +0000 UTC") if learned else None))
db.commit()

# --- balance anchors ------------------------------------------------------------------------
first_day = cur.execute("SELECT date(datetime(substr(min(entry_time),1,19), '-5 hours')) AS d FROM trades").fetchone()["d"]
for ref, bal in (("APEX-250001-11", 50000), ("LFE050-000123-4", 50000), ("120999", 25000), ("EXPRESS-V2-250000-99", 50000)):
    api("POST", f"/api/v1/accounts/{acct[ref]}/balances", {"date": first_day, "balance": bal, "note": "start of the demo period"})
api("POST", f"/api/v1/accounts/{acct['LFE050-000123-4']}/balances", {"date": days[8], "balance": 52400, "note": "after payout"})

# --- day notes --------------------------------------------------------------------------------
DAY_NOTES = [
    ("good", 4, ["calm", "focused"], [], "Plan trade at the overnight low worked on the second touch. Sat through the chop at midday instead of forcing a third decision.\n\n**Kept the plan.** Two decisions, both inside the first ninety minutes."),
    ("neutral", 3, ["anxious"], ["hesitated"], "Missed the first move waiting for a cleaner retest, then took a smaller version later. Net small green.\n\n- The hesitation came from Tuesday's loss, not from the chart\n- VWAP reclaim was valid at 09:12; I had the alert and did not click"),
    ("bad", 2, ["frustrated", "tilted"], ["overtraded", "revenge"], "Four decisions after the plan said three. The fourth was pure revenge after the stop on the short.\n\nClosing the platform at the stop time is the fix; the lesson has been in the ledger twice."),
    ("good", 5, ["confident"], [], "Trend day as planned. Three pullback entries, two winners, the loser was the late one at 11:20 which the plan already said not to take.\n\nBest day of the month on the Lucid account."),
    ("neutral", 3, ["bored"], [], "Range day. One fade worked, one did not. Flat by 10:30 and that is fine."),
    ("good", 4, ["calm"], ["early-exit"], "Took profit on the ES long one point before the target, again. The runner would have paid for the day.\n\nOtherwise clean: stop honoured on the NQ short."),
    ("bad", 2, ["rushed"], ["chased", "no-stop"], "Chased the open on NQ with no stop recorded; the guardrail nudge came at 08:41 and I was already down. Took the stop late.\n\nThe rest of the day was waiting for the loss back, which never works."),
    ("neutral", 3, ["focused"], [], "Light day before the number. One decision, scratched. Watched more than traded."),
]
entries = {}
for d, (tag, mood, emotions, mistakes, body) in zip(days[:8], DAY_NOTES):
    code, out = api("POST", "/api/v1/journal", {"scope": "day", "date": d, "body": body, "tag": tag, "mood": mood, "emotions": emotions, "mistakes": mistakes})
    if code == 201:
        entries[("day", d)] = out["id"]

# --- trade notes on recent trades, mixed outcomes ------------------------------------------
rows = cur.execute(
    "SELECT t.id, t.symbol_root, t.direction, t.net_pnl, a.account_ref, date(datetime(substr(t.exit_time,1,19), '-5 hours')) AS d FROM trades t JOIN accounts a ON a.id=t.account_id "
    "WHERE t.status='closed' AND a.account_ref IN ('APEX-250001-11','120999','EXPRESS-V2-250000-99') ORDER BY t.exit_time DESC LIMIT 120"
).fetchall()
def spread(rs, n):
    """one trade per day, newest first"""
    seen, out = set(), []
    for r in rs:
        if r["d"] in seen:
            continue
        seen.add(r["d"])
        out.append(r)
        if len(out) == n:
            break
    return out


winners = spread([r for r in rows if (r["net_pnl"] or 0) > 0], 6)
losers = spread([r for r in rows if (r["net_pnl"] or 0) < 0], 5)
TRADE_NOTES = [
    ("good", "ORB fade", [], ["calm"], "Fade of the opening drive into the plan level. Entry on the second rejection wick, stop above the high, target the open: filled in eleven minutes."),
    ("good", "Trend pullback", ["early-exit"], ["anxious"], "Right idea, left early. The pullback held the EMA and I took one point when the target was four away."),
    ("good", "VWAP reclaim", [], ["focused"], "Reclaim held on the retest. Scaled in on the second bar, half off at the drive high, runner to the next level."),
    ("neutral", "ORB fade", [], ["bored"], "Scratch. The fade never got going; out at the entry when the range extended."),
    ("good", "Trend pullback", [], ["confident"], "Second pullback of the trend day. Trailed with the EMA, out on the first close through it."),
    ("good", "VWAP reclaim", ["late-exit"], ["greedy"], "Gave back half of the move waiting for the round number. Target was the drive high; take it next time."),
    ("bad", "ORB fade", ["chased"], ["rushed"], "Chased the second leg of the drive instead of waiting for the stall at the level. Stopped at the high of the day."),
    ("bad", "Trend pullback", ["against-plan"], ["frustrated"], "The plan said no entries after 11:00; this was 11:20. Stopped on the lunch reversal."),
    ("bad", None, ["no-stop", "moved-stop"], ["tilted"], "No stop recorded at entry, then a mental stop moved twice. Took twice the planned loss."),
    ("bad", "VWAP reclaim", ["revenge"], ["tilted"], "Re-entry one minute after the stop, same direction, no new signal. The guardrail flagged it and I took it anyway."),
    ("neutral", "ORB fade", ["hesitated"], ["anxious"], "Entered on the third touch instead of the first and got the small version of the move."),
]
for r, (tag, sname, mistakes, emotions, body) in zip(winners + losers, TRADE_NOTES):
    payload = {"scope": "trade", "tradeId": r["id"], "body": body, "tag": tag, "mistakes": mistakes, "emotions": emotions}
    if sname:
        payload["strategyId"] = strat[sname]
    api("POST", "/api/v1/journal", payload)

# --- a group note on a recent copy set -----------------------------------------------------
sets = cur.execute(
    "SELECT m.set_id, group_concat(m.trade_id) AS ids, max(t.exit_time) AS x FROM trade_set_members m JOIN trades t ON t.id=m.trade_id WHERE t.status='closed' AND t.net_pnl > 0 GROUP BY m.set_id HAVING count(*) = 2 ORDER BY x DESC LIMIT 1"
).fetchone()
if sets:
    api("POST", "/api/v1/journal", {"scope": "group", "tradeIds": [int(i) for i in sets["ids"].split(",")], "body": "One decision on both prop accounts. The copier lagged 800 ms on the Lucid leg, a tick worse; still the cleanest trade of the week.", "tag": "good", "strategyId": strat["ORB fade"], "emotions": ["calm"]})

# --- a week note ------------------------------------------------------------------------------
import datetime as _dt

y, w, _ = _dt.date.fromisoformat(days[0]).isocalendar()
api("POST", "/api/v1/journal", {"scope": "week", "week": f"{y}-W{w:02d}", "body": "Green week on three of four accounts. The eval is close to the target; one more week at this size, then size down and stop giving it back on Fridays.\n\nMistakes this week: early exits (3), one revenge entry. Lessons applied: stop honoured every time it was recorded.", "tag": "good"})

# --- a plan on the open position ------------------------------------------------------------
code, pos = api("GET", "/api/v1/positions")
for p in pos.get("items", []):
    if p["accountId"] == acct["APEX-250001-11"]:
        px = p["avgEntry"]
        api("POST", p["planUrl"], {"stop": round(px - 6, 2), "target": round(px + 14, 2), "thesis": "Friday close held the week's low; holding the long into Monday's open with the stop under it."})
        break

# --- the trader wrote these, not an agent: mark the provenance and date the notes to their day --
cur.execute("UPDATE journal_entries SET tag_source='trader' WHERE tag IS NOT NULL")
cur.execute("UPDATE journal_entries SET strategy_source='trader' WHERE strategy_id IS NOT NULL")
cur.execute("UPDATE journal_entry_labels SET source='trader'")
cur.execute("UPDATE journal_entries SET created_at = substr(entry_date,1,10) || ' 21:40:00 +0000 UTC', updated_at = substr(entry_date,1,10) || ' 21:40:00 +0000 UTC'")
cur.execute("UPDATE position_plans SET source='trader'")
cur.execute("UPDATE users SET guardrail=1, display_name='Demo trader' WHERE id=1")
db.commit()
n = cur.execute("SELECT count(*) FROM journal_entries").fetchone()[0]
print(f"seeded: {len(strat)} strategies, {len(days[:len(PLANS)])} day plans, {n} journal entries, {len(LESSONS)} lessons")
