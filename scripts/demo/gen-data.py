#!/usr/bin/env python3
"""Synthetic broker exports for the demo instance (scripts/demo/seed.sh).

Writes two files into the directory given as the only argument:

  motivewave.csv   a MotiveWave fill export for three accounts: an Apex eval and a
                   Lucid funded account traded together by a copier (Rithmic), and an
                   AMP live account on CQG, where the E-mini S&P is called EP
  topstepx.csv     a TopstepX order export (UTF-8 BOM, order rows, Filled only)

Twelve weeks of ES, MES, NQ and MNQ day trading ending Friday 2026-10-16, two to five
decisions a day inside the regular session, a copy set on most Apex/Lucid decisions,
some scale-ins and partial exits, one long-to-short flip, and one position still open
on the last day. The random seed is fixed, so the files are the same on every run.

Nothing here is real: the accounts, tickets, prices and results are invented.
"""
import csv
import math
import random
import sys
from datetime import date, datetime, timedelta
from pathlib import Path

SEED = 2026
LAST_DAY = date(2026, 10, 16)
WEEKS = 12
HOLIDAYS = {date(2026, 9, 7)}  # Labor Day
OFF_DAYS = {date(2026, 8, 13), date(2026, 8, 14), date(2026, 9, 24)}  # days away from the desk

# Points per contract, tick, and a plausible stop distance in points per root.
SPEC = {
    "ES": {"tick": 0.25, "stop": 4.0, "point": 50.0},
    "MES": {"tick": 0.25, "stop": 4.0, "point": 5.0},
    "NQ": {"tick": 0.25, "stop": 18.0, "point": 20.0},
    "MNQ": {"tick": 0.25, "stop": 18.0, "point": 2.0},
}
ROLL = date(2026, 9, 11)  # from the September to the December contract


def contract(root: str, day: date, digits: int) -> str:
    code = "U" if day < ROLL else "Z"
    year = "26" if digits == 2 else "6"
    return f"{root}{code}{year}"


# Daily front-month closes (Yahoo ES=F and NQ=F) for the demo period, so the invented fills sit
# where the market actually was and the replay charts line up with them.
ES_CLOSE = "07-20:7484.25,07-21:7545.75,07-22:7540.25,07-23:7445.0,07-24:7447.5,07-27:7448.25,07-28:7465.25,07-29:7351.25,07-30:7472.5,07-31:7519.25,08-03:7628.25,08-04:7765.5,08-05:7749.5,08-06:7734.75,08-07:7779.75,08-10:7776.75,08-11:7747.5,08-12:7770.5,08-13:7822.5,08-14:7805.0,08-17:7768.75,08-18:7714.0,08-19:7729.0,08-20:7662.5,08-21:7691.25,08-24:7669.75,08-25:7692.0,08-26:7690.0,08-27:7742.5,08-28:7722.0,08-31:7699.0,09-01:7642.75,09-02:7676.5,09-03:7754.75,09-04:7722.0,09-08:7680.5,09-09:7643.75,09-10:7598.5,09-11:7659.5,09-14:7625.0,09-15:7589.25,09-16:7556.5,09-17:7640.0,09-18:7657.25,09-21:7833.5,09-22:7831.75,09-23:7772.5,09-24:7767.0,09-25:7803.75,09-28:7746.75,09-29:7732.0,09-30:7715.5,10-01:7724.0,10-02:7777.25,10-05:7826.25,10-06:7874.0,10-07:7852.75,10-08:7816.25,10-09:7859.75,10-12:7858.0,10-13:7855.75,10-14:7770.25,10-15:7791.0,10-16:7704.25"
NQ_CLOSE = "07-20:28778.75,07-21:29316.0,07-22:29181.25,07-23:28620.75,07-24:28282.25,07-27:28190.0,07-28:27922.0,07-29:27342.0,07-30:28237.75,07-31:28404.25,08-03:28891.75,08-04:29863.5,08-05:29615.0,08-06:29488.25,08-07:29834.75,08-10:29737.0,08-11:29626.0,08-12:29853.25,08-13:30188.5,08-14:30141.75,08-17:30096.0,08-18:29586.0,08-19:29512.75,08-20:29300.5,08-21:29387.75,08-24:29105.75,08-25:29276.75,08-26:29289.5,08-27:29695.75,08-28:29491.75,08-31:29513.0,09-01:29125.5,09-02:29186.25,09-03:29524.75,09-04:29565.25,09-08:29538.75,09-09:29448.75,09-10:29135.25,09-11:29387.0,09-14:29152.25,09-15:28955.0,09-16:28963.5,09-17:29446.75,09-18:29613.75,09-21:30784.75,09-22:31028.5,09-23:30764.75,09-24:30766.75,09-25:30889.25,09-28:30566.25,09-29:30613.25,09-30:30698.75,10-01:30760.5,10-02:31061.75,10-05:31317.75,10-06:31483.25,10-07:31402.25,10-08:30969.5,10-09:31108.75,10-12:30898.25,10-13:30791.5,10-14:30182.75,10-15:30143.75,10-16:29391.5"


def closes(spec: str) -> dict:
    out = {}
    for item in spec.split(","):
        md, px = item.split(":")
        m, d = md.split("-")
        out[date(2026, int(m), int(d))] = float(px)
    return out


class Market:
    """Intraday prices: a path from the previous close to the day's close with a wave and noise."""

    def __init__(self, rng: random.Random):
        self.rng = rng
        self.closes = {"ES": closes(ES_CLOSE), "NQ": closes(NQ_CLOSE)}

    def price(self, root: str, day: date, minute: int) -> float:
        idx = "ES" if root in ("ES", "MES") else "NQ"
        series = self.closes[idx]
        days = sorted(d for d in series if d <= day)
        close = series[days[-1]]
        prev = series[days[-2]] if len(days) > 1 else close
        swing = 10.0 if idx == "ES" else 45.0
        p = prev + (close - prev) * min(minute, 390) / 390.0 + swing * math.sin(minute / 70.0) + self.rng.gauss(0, swing / 5)
        return snap(p, SPEC[root]["tick"])


def snap(p: float, tick: float) -> float:
    return round(round(p / tick) * tick, 2)


def trading_days():
    d = LAST_DAY - timedelta(weeks=WEEKS) + timedelta(days=3)
    while d <= LAST_DAY:
        if d.weekday() < 5 and d not in HOLIDAYS and d not in OFF_DAYS:
            yield d
        d += timedelta(days=1)


class Fills:
    def __init__(self):
        self.mw = []  # MotiveWave rows
        self.tsx = []  # TopstepX rows
        self.ticket = 7_400_000
        self.order = 2_310_000

    def mw_fill(self, account, root, symbol, when, side, qty, price, source):
        self.ticket += 1
        self.mw.append(
            {
                "Account": account,
                "Ticket": str(self.ticket),
                "Underlying": root,
                "Symbol": symbol,
                "Exchange": "CME",
                "Time": when.strftime("%m/%d/%Y %H:%M:%S.") + f"{when.microsecond // 1000:03d}-0500",
                "Action": "BOT" if side == "buy" else "SLD",
                "Quantity": str(qty),
                "Price": f"{price:.2f}",
                "Source": source,
            }
        )

    def tsx_fill(self, account, symbol, when, side, qty, price, disposition):
        self.order += 1
        self.tsx.append(
            {
                "Id": str(self.order),
                "AccountName": account,
                "ContractName": symbol,
                "Status": "Filled",
                "Size": str(qty),
                "Side": "Bid" if side == "buy" else "Ask",
                "FilledAt": when.strftime("%m/%d/%Y %H:%M:%S") + " -05:00",
                "ExecutePrice": f"{price:.2f}",
                "PositionDisposition": disposition,
                "Type": "Market" if disposition == "Opening" else "Limit",
                "LimitPrice": "",
                "StopPrice": "",
            }
        )


def outcome(rng: random.Random, root: str):
    """Signed points for one round trip: about half the trades win, wins run a little past 1R."""
    stop = SPEC[root]["stop"]
    r = rng.random()
    if r < 0.53:
        mult = rng.choice([0.6, 0.8, 1.0, 1.2, 1.5, 1.8, 2.2])
    elif r < 0.66:
        mult = rng.choice([-0.25, -0.4, 0.1])  # scratches
    else:
        mult = rng.choice([-1.0, -1.0, -1.0, -0.8, -1.2])
    return snap(stop * mult, SPEC[root]["tick"])


def main(out: Path):
    rng = random.Random(SEED)
    mkt = Market(random.Random(SEED + 1))
    fills = Fills()
    days = list(trading_days())

    # the copier: the same decision on the Apex eval and the Lucid funded account
    COPIED = [("APEX-250001-11", 2, "Rithmic"), ("LFE050-000123-4", 1, "Rithmic")]
    AMP = ("120999", 1, "AMP Global Clearing - CQG")
    TSX = "EXPRESS-V2-250000-99"

    flip_day = days[len(days) * 2 // 3]
    for day in days:
        # --- copied decisions on ES/NQ (Apex + Lucid) ---
        n = rng.choice([2, 2, 3, 3, 4, 5])
        minutes = sorted(rng.sample(range(35, 360), n))  # minutes after 08:30 CT
        for i, m in enumerate(minutes):
            root = rng.choice(["ES", "ES", "NQ", "MES"])
            direction = rng.choice(["long", "short"])
            entry_at = datetime(day.year, day.month, day.day, 8, 30) + timedelta(minutes=m, seconds=rng.randint(0, 59), microseconds=rng.randint(0, 999) * 1000)
            hold = rng.choice([4, 7, 11, 15, 22, 31, 48])
            exit_at = entry_at + timedelta(minutes=hold, seconds=rng.randint(0, 59))
            price_in = mkt.price(root, day, m)
            pts = outcome(rng, root)
            price_out = snap(price_in + pts if direction == "long" else price_in - pts, SPEC[root]["tick"])
            sym = contract(root, day, 2)
            scale = rng.random() < 0.18 and pts > 0
            last_open = day == LAST_DAY and i == len(minutes) - 1
            for acct, qty, source in COPIED:
                lag = timedelta(milliseconds=rng.randint(200, 1500))
                buy, sell = ("buy", "sell") if direction == "long" else ("sell", "buy")
                if day == flip_day and i == 1:
                    # a long of qty that flips to a short of qty on one fill, covered later
                    fills.mw_fill(acct, root, sym, entry_at + lag, "buy", qty, price_in, source)
                    flip_at = exit_at + lag
                    flip_px = snap(price_in + abs(pts), SPEC[root]["tick"])
                    fills.mw_fill(acct, root, sym, flip_at, "sell", qty * 2, flip_px, source)
                    cover_at = flip_at + timedelta(minutes=9)
                    cover_px = snap(flip_px - SPEC[root]["stop"] * 0.6, SPEC[root]["tick"])
                    fills.mw_fill(acct, root, sym, cover_at, "buy", qty, cover_px, source)
                    continue
                fills.mw_fill(acct, root, sym, entry_at + lag, buy, qty, price_in, source)
                if last_open:
                    continue  # still open at the end of the file
                if scale and qty > 1:
                    add_at = entry_at + lag + timedelta(minutes=2)
                    add_px = snap(price_in - SPEC[root]["stop"] * 0.3 if direction == "long" else price_in + SPEC[root]["stop"] * 0.3, SPEC[root]["tick"])
                    fills.mw_fill(acct, root, sym, add_at, buy, 1, add_px, source)
                    # two partial exits
                    half_at = exit_at + lag - timedelta(minutes=3)
                    half_px = snap(price_in + pts * 0.6 if direction == "long" else price_in - pts * 0.6, SPEC[root]["tick"])
                    fills.mw_fill(acct, root, sym, half_at, sell, 1, half_px, source)
                    fills.mw_fill(acct, root, sym, exit_at + lag, sell, qty, price_out, source)
                else:
                    fills.mw_fill(acct, root, sym, exit_at + lag, sell, qty, price_out, source)

        # --- AMP live: one or two ES (called EP on CQG) trades, more careful ---
        for m in sorted(rng.sample(range(40, 300), rng.choice([0, 1, 1, 2]))):
            direction = rng.choice(["long", "short"])
            entry_at = datetime(day.year, day.month, day.day, 8, 30) + timedelta(minutes=m, seconds=rng.randint(0, 59))
            exit_at = entry_at + timedelta(minutes=rng.choice([9, 14, 25, 40]))
            price_in = mkt.price("ES", day, m)
            pts = outcome(rng, "ES")
            if rng.random() < 0.15:
                pts = snap(pts * 1.6, 0.25)
            price_out = snap(price_in + pts if direction == "long" else price_in - pts, 0.25)
            sym = contract("EP", day, 2)
            buy, sell = ("buy", "sell") if direction == "long" else ("sell", "buy")
            fills.mw_fill(AMP[0], "EP", sym, entry_at, buy, AMP[1], price_in, AMP[2])
            fills.mw_fill(AMP[0], "EP", sym, exit_at, sell, AMP[1], price_out, AMP[2])

        # --- Topstep eval: micros, two to four a day ---
        for m in sorted(rng.sample(range(30, 330), rng.choice([2, 3, 3, 4]))):
            root = rng.choice(["MNQ", "MNQ", "MES"])
            direction = rng.choice(["long", "short"])
            qty = rng.choice([2, 3, 5])
            entry_at = datetime(day.year, day.month, day.day, 8, 30) + timedelta(minutes=m, seconds=rng.randint(0, 59))
            exit_at = entry_at + timedelta(minutes=rng.choice([3, 6, 10, 17, 26]))
            price_in = mkt.price(root, day, m)
            pts = outcome(rng, root)
            price_out = snap(price_in + pts if direction == "long" else price_in - pts, 0.25)
            sym = contract(root, day, 1)
            buy, sell = ("buy", "sell") if direction == "long" else ("sell", "buy")
            fills.tsx_fill(TSX, sym, entry_at, buy, qty, price_in, "Opening")
            if rng.random() < 0.2 and qty > 2:
                fills.tsx_fill(TSX, sym, exit_at - timedelta(minutes=2), sell, qty - 1, snap(price_in + pts * 0.5 if direction == "long" else price_in - pts * 0.5, 0.25), "Closing")
                fills.tsx_fill(TSX, sym, exit_at, sell, 1, price_out, "Closing")
            else:
                fills.tsx_fill(TSX, sym, exit_at, sell, qty, price_out, "Closing")

    out.mkdir(parents=True, exist_ok=True)
    fills.mw.sort(key=lambda r: (datetime.strptime(r["Time"][:23], "%m/%d/%Y %H:%M:%S.%f"), r["Ticket"]))
    with (out / "motivewave.csv").open("w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["Account", "Ticket", "Underlying", "Symbol", "Exchange", "Time", "Action", "Quantity", "Price", "Source"])
        w.writeheader()
        w.writerows(fills.mw)
    fills.tsx.sort(key=lambda r: (datetime.strptime(r["FilledAt"][:19], "%m/%d/%Y %H:%M:%S"), r["Id"]))
    with (out / "topstepx.csv").open("w", newline="", encoding="utf-8-sig") as f:
        w = csv.DictWriter(f, fieldnames=["Id", "AccountName", "ContractName", "Status", "Size", "Side", "FilledAt", "ExecutePrice", "PositionDisposition", "Type", "LimitPrice", "StopPrice"])
        w.writeheader()
        w.writerows(fills.tsx)
    print(f"{len(fills.mw)} MotiveWave fills, {len(fills.tsx)} TopstepX orders, {len(days)} trading days -> {out}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: gen-data.py OUT_DIR")
    main(Path(sys.argv[1]))
