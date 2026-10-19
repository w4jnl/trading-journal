// What the widgets show: the last Today the app fetched, cut down and written to the App Group
// after every fetch (and reloaded by the silent sync push). The widgets never call the server.
import Foundation

struct TodayCache: Codable, Equatable, Sendable {
    struct DayCell: Codable, Equatable, Sendable, Identifiable {
        var date: String
        var net: Double
        var trades: Int
        var id: String { date }
    }

    var day: String
    var tz: String
    var asOf: Date
    var net: Double
    var unrealized: Double
    var stale: Bool
    var trades: Int
    var wins: Int
    var losses: Int
    var openCount: Int
    var hasPlan: Bool
    var maxTrades: Int
    var tradesUsed: Int
    var lossUsed: Double
    var maxLoss: Double
    var unseen: Int
    var inGlobex: Bool
    /// The last trading days, oldest first, for the medium widget's strip; may be empty.
    var days: [DayCell]
    // The same per decision (a copy set counts once); absent from a cache written before them.
    var decisions: Int?
    var setWins: Int?
    var setLosses: Int?
    var openSets: Int?

    var total: Double { net + unrealized }

    /// The cache as a trader who groups trades by set reads it: decisions for trades, wins and
    /// losses per decision, open decisions for open legs. Itself when nothing per decision is known.
    func display(bySet: Bool) -> TodayCache {
        guard bySet else { return self }
        var g = self
        if let d = decisions { g.trades = d }
        if let w = setWins { g.wins = w }
        if let l = setLosses { g.losses = l }
        if let o = openSets { g.openCount = o }
        return g
    }
    var weekNet: Double { days.suffix(5).reduce(0) { $0 + $1.net } }
    var timeZone: TimeZone { TimeZone(identifier: tz) ?? .current }

    static let suite = "group.nl.w4j.tradingjournal"
    static let key = "today"

    static func from(_ t: Today, days: [TodayCache.DayCell] = [], asOf: Date = .now) -> TodayCache {
        TodayCache(day: t.day, tz: t.timezone, asOf: asOf, net: t.stats.net, unrealized: t.positions.isEmpty ? 0 : t.unrealized.total, stale: t.unrealized.stale,
                   trades: t.stats.trades, wins: t.stats.wins, losses: t.stats.losses, openCount: t.positions.count,
                   hasPlan: t.plan.plan != nil, maxTrades: t.plan.maxTrades ?? 0, tradesUsed: t.plan.tradesUsed, lossUsed: t.plan.lossUsed, maxLoss: t.plan.maxLoss ?? 0,
                   unseen: t.nudges.unseen, inGlobex: t.session.inGlobex, days: days,
                   decisions: t.decisions?.count ?? t.plan.tradesUsed, setWins: t.decisions?.wins, setLosses: t.decisions?.losses, openSets: t.positions.openSets)
    }

    nonisolated static func read(defaults: UserDefaults? = UserDefaults(suiteName: suite)) -> TodayCache? {
        guard let data = defaults?.data(forKey: key) else { return nil }
        return try? JSONDecoder().decode(TodayCache.self, from: data)
    }

    nonisolated func write(defaults: UserDefaults? = UserDefaults(suiteName: suite)) {
        guard let data = try? JSONEncoder().encode(self) else { return }
        defaults?.set(data, forKey: Self.key)
    }

    static let preview = TodayCache(day: "2026-10-15", tz: "Europe/Amsterdam", asOf: .now, net: 120.5, unrealized: 13.75, stale: false, trades: 4, wins: 3, losses: 1, openCount: 1,
                                    hasPlan: true, maxTrades: 6, tradesUsed: 4, lossUsed: 84.2, maxLoss: 300, unseen: 1, inGlobex: true,
                                    days: [("2026-10-05", -1543.75, 86), ("2026-10-06", 140, 122), ("2026-10-07", 803, 86), ("2026-10-08", 451, 60), ("2026-10-09", 639, 67),
                                           ("2026-10-12", 135, 19), ("2026-10-13", -313, 51), ("2026-10-14", 1751.76, 69)].map { DayCell(date: $0.0, net: $0.1, trades: $0.2) })
}
