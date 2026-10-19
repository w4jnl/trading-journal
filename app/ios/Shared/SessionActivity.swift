// The Session Live Activity (Dynamic Island, Lock Screen): one per trading day, its attributes
// and content state decoded by ActivityKit from the server's pushes (internal/push/live.go:
// SessionAttributes, SessionState, the same names; every field is always sent). The app
// builds the same state from Today for a local start, so the first frame is right.
import ActivityKit
import Foundation

struct SessionActivityAttributes: ActivityAttributes {
    struct ContentState: Codable, Hashable, Sendable {
        var realized: Double
        var unrealized: Double
        var trades: Int
        var wins: Int
        var losses: Int
        var maxTrades: Int // 0: no cap
        var lossUsed: Double // positive money
        var maxLoss: Double // 0: no budget
        var positions: [SessionPosition] // the legs of at most three decisions, the largest at stake first, a set's legs together
        var openCount: Int // legs
        var nudges: Int // unseen
        var lastFill: Int64 // unix seconds, 0 none
        var updated: Int64
        var stale: Bool // the prices are
        var alert: String
        // The same per decision (a copy set counts once); absent from a server before them.
        var decisions: Int?
        var setWins: Int?
        var setLosses: Int?
        var openSets: Int?
    }

    var day: String
    var tz: String
    var rthStart: Int64 // unix seconds
    var rthEnd: Int64
    var exchangeTz: String? // absent from a payload before 2.18.1

    var timeZone: TimeZone { TimeZone(identifier: tz) ?? .current }
    /// The exchange's zone: Chicago for CME futures unless the server says otherwise.
    var exchangeZone: TimeZone { exchangeTz.flatMap(TimeZone.init(identifier:)) ?? TimeZone(identifier: "America/Chicago") ?? .current }
    var rthRange: ClosedRange<Date> {
        let a = Date(timeIntervalSince1970: TimeInterval(rthStart)), b = Date(timeIntervalSince1970: TimeInterval(rthEnd))
        return a...max(b, a.addingTimeInterval(60))
    }

    static func from(_ t: Today) -> SessionActivityAttributes {
        SessionActivityAttributes(day: t.day, tz: t.timezone, rthStart: Int64(t.session.rthStart.timeIntervalSince1970), rthEnd: Int64(t.session.rthEnd.timeIntervalSince1970), exchangeTz: t.session.exchangeTz)
    }

    static let preview = SessionActivityAttributes(day: "2026-10-15", tz: "Europe/Amsterdam", rthStart: 1_792_000_000, rthEnd: 1_792_023_400)
}

struct SessionPosition: Codable, Hashable, Sendable, Identifiable {
    var a: Int64 // account id
    var s: String // contract
    var d: String // long | short
    var q: Double
    var avg: Double
    var last: Double
    var u: Double // unrealised
    var tts: Double // ticks to the stop; 0 without a stop
    var beyond: Bool // price through the stop
    var set: [Int64]? // the copied decision's account ids, sorted, this one included; empty or absent on its own
    /// How many legs this row stands for: 1 as sent, more once folded by set. Not on the wire.
    var legs = 1

    enum CodingKeys: String, CodingKey { case a, s, d, q, avg, last, u, tts, beyond, set }

    init(a: Int64, s: String, d: String, q: Double, avg: Double, last: Double, u: Double, tts: Double, beyond: Bool, set: [Int64]? = nil, legs: Int = 1) {
        self.a = a; self.s = s; self.d = d; self.q = q; self.avg = avg; self.last = last; self.u = u; self.tts = tts; self.beyond = beyond; self.set = set; self.legs = legs
    }

    var id: String { "\(a):\(s)" }
    var link: URL { Route.position(account: Int(a), symbol: s).url }
    var hasStop: Bool { tts != 0 || beyond }
    /// The decision this leg belongs to; the leg itself on its own. push.SessionPosition.SetKey.
    var setKey: String {
        guard let set, !set.isEmpty else { return "\(a):\(s)" }
        return set.map(String.init).joined(separator: ",") + ":" + s
    }
}

extension SessionActivityAttributes.ContentState {
    var total: Double { realized + unrealized }
    var anyBeyond: Bool { positions.contains(where: \.beyond) }
    /// The glance: warn when a nudge waits or a stop is breached, else the sign of the total.
    var kind: Kind {
        if nudges > 0 || anyBeyond { return .warn }
        if openCount == 0 && trades == 0 { return .quiet }
        if total < 0 { return .neg }
        return .pos
    }
    enum Kind { case quiet, pos, neg, warn }

    /// 0…1 of the max-loss budget used, nil without a budget.
    var lossFraction: Double? { maxLoss > 0 ? min(1, lossUsed / maxLoss) : nil }
    var lastFillDate: Date? { lastFill > 0 ? Date(timeIntervalSince1970: TimeInterval(lastFill)) : nil }
    var updatedDate: Date { Date(timeIntervalSince1970: TimeInterval(updated)) }

    static let quiet = Self(realized: 0, unrealized: 0, trades: 0, wins: 0, losses: 0, maxTrades: 0, lossUsed: 0, maxLoss: 0, positions: [], openCount: 0, nudges: 0, lastFill: 0, updated: 0, stale: false, alert: "")

    /// What the server would send for this Today (push/live.go SessionState, Arrange and trim),
    /// for a local start and for the activity's refresh while the app is open.
    static func from(_ t: Today, now: Date = .now) -> Self {
        let legs = t.positions.map { p in
            SessionPosition(a: Int64(p.accountId), s: p.symbol, d: p.direction.rawValue, q: p.qty, avg: p.avgEntry ?? 0, last: p.lastPrice ?? 0,
                            u: p.unrealized ?? 0, tts: p.ticksToStop ?? 0, beyond: p.beyondStop,
                            set: p.setLegs.isEmpty ? nil : (p.setLegs + [p.accountId]).sorted().map(Int64.init))
        }
        return Self(
            realized: t.stats.net, unrealized: t.unrealized.total, trades: t.stats.trades, wins: t.stats.wins, losses: t.stats.losses,
            maxTrades: t.plan.maxTrades ?? 0, lossUsed: t.plan.lossUsed, maxLoss: t.plan.maxLoss ?? 0,
            positions: Self.arranged(legs),
            openCount: t.positions.count, nudges: t.nudges.unseen, lastFill: Int64(t.lastFillAt?.timeIntervalSince1970 ?? 0),
            updated: Int64(now.timeIntervalSince1970), stale: t.unrealized.stale, alert: "",
            decisions: t.decisions?.count ?? t.plan.tradesUsed, setWins: t.decisions?.wins, setLosses: t.decisions?.losses, openSets: t.positions.openSets)
    }

    static let maxSets = 3, maxLegs = 12

    /// The server's order and cut: the decision with the most unrealised at stake first, its
    /// legs together and the largest first; the legs of at most three decisions, twelve in all.
    static func arranged(_ legs: [SessionPosition]) -> [SessionPosition] {
        var exposure: [String: Double] = [:]
        for p in legs { exposure[p.setKey, default: 0] += abs(p.u) }
        let sorted = legs.sorted { a, b in
            let ka = a.setKey, kb = b.setKey
            if ka != kb { return exposure[ka]! != exposure[kb]! ? exposure[ka]! > exposure[kb]! : ka < kb }
            return abs(a.u) > abs(b.u)
        }
        var out: [SessionPosition] = []
        var last = ""
        var sets = 0
        for p in sorted {
            if p.setKey != last {
                if sets == maxSets { break }
                sets += 1
                last = p.setKey
            }
            if out.count == maxLegs { break }
            out.append(p)
        }
        return out
    }

    /// The state as a trader who groups trades by set reads it: one row per decision (size and
    /// unrealised summed, the lead leg's prices, the stop distance of the leg nearest its stop,
    /// through the stop if any leg is, "no stop" only when no leg has one), decisions in place of
    /// trades, wins and losses per decision, open decisions in place of open legs. The same
    /// state when the server did not send the per-decision counts and no leg names a set.
    var grouped: Self {
        var g = self
        var rows: [SessionPosition] = []
        var index: [String: Int] = [:]
        for p in positions {
            let k = p.setKey
            if let i = index[k] {
                rows[i].q += p.q
                rows[i].u += p.u
                rows[i].legs += 1
                rows[i].beyond = rows[i].beyond || p.beyond
                if p.tts != 0, rows[i].tts == 0 || abs(p.tts) < abs(rows[i].tts) { rows[i].tts = p.tts }
            } else {
                index[k] = rows.count
                rows.append(p)
            }
        }
        g.positions = rows
        if let d = decisions { g.trades = d }
        if let w = setWins { g.wins = w }
        if let l = setLosses { g.losses = l }
        if let o = openSets { g.openCount = o }
        return g
    }

    /// The state for the faces: folded when the trader groups by set.
    func display(bySet: Bool) -> Self { bySet ? grouped : self }
}
