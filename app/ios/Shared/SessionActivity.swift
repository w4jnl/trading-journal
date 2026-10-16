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
        var positions: [SessionPosition] // at most three, by |unrealised|
        var openCount: Int
        var nudges: Int // unseen
        var lastFill: Int64 // unix seconds, 0 none
        var updated: Int64
        var stale: Bool // the prices are
        var alert: String
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

    var id: String { "\(a):\(s)" }
    var link: URL { Route.position(account: Int(a), symbol: s).url }
    var hasStop: Bool { tts != 0 || beyond }
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

    /// What the server would send for this Today (push/live.go sessionState), for a local start
    /// and for the activity's refresh while the app is open.
    static func from(_ t: Today, now: Date = .now) -> Self {
        let ranked = t.positions.sorted { abs($0.unrealized ?? 0) > abs($1.unrealized ?? 0) }.prefix(3)
        return Self(
            realized: t.stats.net, unrealized: t.unrealized.total, trades: t.stats.trades, wins: t.stats.wins, losses: t.stats.losses,
            maxTrades: t.plan.maxTrades ?? 0, lossUsed: t.plan.lossUsed, maxLoss: t.plan.maxLoss ?? 0,
            positions: ranked.map { p in
                SessionPosition(a: Int64(p.accountId), s: p.symbol, d: p.direction.rawValue, q: p.qty, avg: p.avgEntry ?? 0, last: p.lastPrice ?? 0,
                                u: p.unrealized ?? 0, tts: p.ticksToStop ?? 0, beyond: p.beyondStop)
            },
            openCount: t.positions.count, nudges: t.nudges.unseen, lastFill: Int64(t.lastFillAt?.timeIntervalSince1970 ?? 0),
            updated: Int64(now.timeIntervalSince1970), stale: t.unrealized.stale, alert: "")
    }
}
