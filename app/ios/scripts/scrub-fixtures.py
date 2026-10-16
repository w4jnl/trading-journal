#!/usr/bin/env python3
"""Scrub the wire-test fixtures after `make ios-fixtures` captured them from a journal.

The fixtures keep their shape and the values the tests assert (ids, counts, dates, per-trade
P&L, prices) but lose what identifies the trader: account numbers are mapped to example ids
elsewhere (see the capture target), capital figures are scaled, and every free text written
by the trader or the AI about the trader's trades is replaced by a sentence of the same kind.
Run from the repository root: python3 app/ios/scripts/scrub-fixtures.py
"""
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1] / "TJTests" / "Fixtures"
SCALE = 0.37  # capital figures: balances, equity, drawdowns

NOTES = (
    "---\n\n_Note added 2026-10-14 21:16 CEST_\n\n### AI review — 2026-10-14\n"
    "*example-model via Example provider*\n\n"
    "- **Setup & entry:** a reclaim of the overnight high taken with the plan's size.\n"
    "- **Exit:** the target was hit on the first push; the stop never came into play.\n"
    "- **Lesson:** the same setup on a quieter day would deserve a smaller size.\n"
)
SNIPPET = NOTES  # the list's snippet is the note's head: block structure matters to Markdown.snippet
LESSON = "Close the platform at the stop time and log the day; the next review should show no entries after it."
THESIS = "Reclaim of the overnight high; stop under the pullback low."


def scale(x):
    return None if x is None else round(x * SCALE, 2)


def load(name):
    return json.loads((ROOT / name).read_text())


def save(name, data):
    (ROOT / name).write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")


def scrub_user(me):
    me["user"]["username"] = "trader"
    me["user"]["displayName"] = "trader"
    me["user"]["email"] = ""


def scrub_equity_block(eq):
    for k in ("equityNow", "now", "dayChange", "currentDrawdown"):
        if k in eq and isinstance(eq[k], (int, float)):
            eq[k] = scale(eq[k])
    for t in eq.get("byType", []):
        t["total"] = scale(t["total"])
    for s in eq.get("series", []):
        s["equity"] = scale(s["equity"])
    d = eq.get("drawdown")
    if isinstance(d, dict):
        for k in ("maxDrawdown", "currentDrawdown"):
            if isinstance(d.get(k), (int, float)):
                d[k] = scale(d[k])


def scrub_nudges(items):
    for n in items:
        if n.get("lesson"):
            n["lesson"] = LESSON


def scrub_positions(items):
    for p in items:
        if p.get("thesis"):
            p["thesis"] = THESIS
        for leg in p.get("setLegs") or []:
            if isinstance(leg, dict) and leg.get("thesis"):
                leg["thesis"] = THESIS


def account_map(names):
    """One example id per real account name, keeping the firm's shape: APEX-EXAMPLE-1,
    PA-APEX-EXAMPLE-2, LFF050-EXAMPLE-3, EXPRESS-V2-EXAMPLE-4, U00000005, 120006."""
    out = {}
    for i, name in enumerate(sorted(set(names), key=len, reverse=True), 1):
        if name.isdigit():
            out[name] = f"12{i:04d}"
        elif name[0] == "U" and name[1:].isdigit():
            out[name] = f"U{i:08d}"
        else:
            head = name.split("-")
            prefix = "-".join(head[:2]) if head[0] in ("PA", "EXPRESS") else head[0]
            out[name] = f"{prefix}-EXAMPLE-{i}"
    return out


def replace_everywhere(mapping):
    """Account names appear in every fixture: as fields, in routes and in text."""
    for f in ROOT.glob("*.json"):
        text = f.read_text()
        for real, fake in mapping.items():  # longest first, so a name never eats a longer one
            text = text.replace(real, fake)
        f.write_text(text)


def main():
    replace_everywhere(account_map(a["name"] for a in load("accounts.json")["items"]))
    me = load("me.json"); scrub_user(me); save("me.json", me)

    acc = load("accounts.json")
    for a in acc["items"]:
        b = a.get("balance")
        if b:
            b["balance"] = scale(b["balance"])
            for an in b.get("anchors", []):
                an["balance"] = scale(an["balance"])
                if an.get("note"):
                    an["note"] = "opening balance"
    save("accounts.json", acc)

    eq = load("equity.json"); scrub_equity_block(eq); save("equity.json", eq)

    today = load("today.json")
    scrub_equity_block(today.get("equity", {}))
    scrub_nudges(today.get("nudges", {}).get("items", []))
    scrub_positions(today.get("positions", []))
    save("today.json", today)

    pos = load("positions.json"); scrub_positions(pos["items"]); save("positions.json", pos)
    for name in ("nudges.json", "nudges_day.json"):
        n = load(name); scrub_nudges(n["items"]); save(name, n)

    j = load("journal.json")
    for row in j["items"]:
        if row.get("snippet"):
            row["snippet"] = SNIPPET
    save("journal.json", j)

    d = load("journal_detail.json")
    if d.get("notes"):
        d["notes"] = NOTES
    if d.get("body"):
        d["body"] = NOTES
    save("journal_detail.json", d)

    g = load("digest.json")
    for l in g.get("lessons", []):
        if l.get("lesson"):
            l["lesson"] = LESSON
    save("digest.json", g)

    s = load("strategies.json")
    for st in s["items"]:
        st["body"] = f"# {st['name']}\n\nA playbook in three lines: the market, the trigger, the exit. Example text for the wire tests.\n"
    save("strategies.json", s)
    print("scrubbed", ROOT)


if __name__ == "__main__":
    sys.exit(main())
