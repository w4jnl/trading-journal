// What a journal push carries besides its text (API.md, "What is pushed"): the custom keys the
// app, the service extension and the content extension all read the same way.
import Foundation

enum PushCategory: String, CaseIterable, Sendable {
    case nudge = "TJ_NUDGE"
    case trade = "TJ_TRADE"
    case position = "TJ_POSITION"
}

struct PushPayload: Sendable, Equatable {
    enum Kind: String, Sendable { case nudge, trade, sync, unknown }

    var kind: Kind
    /// A nudge's id (0 for the plan-missing reminder, which has none).
    var id: Int
    /// A nudge's key: "max-trades", "past-stop-live", "plan-missing", …
    var key: String
    var route: String
    var day: String
    var lesson: String
    /// A closed trade.
    var tradeId: Int
    var symbolRoot: String
    var direction: String
    var netPnl: Double?
    var accountId: Int
    /// A silent sync.
    var v: Int
    var badge: Int?
    var unseen: [Int]

    init?(_ info: [AnyHashable: Any]) {
        func str(_ k: String) -> String { info[k] as? String ?? "" }
        func int(_ k: String) -> Int { (info[k] as? Int) ?? (info[k] as? Double).map(Int.init) ?? Int(str(k)) ?? 0 }
        guard let k = Kind(rawValue: str("kind")) else { return nil }
        kind = k
        id = int("id")
        key = str("key")
        route = str("route")
        day = str("day")
        lesson = str("lesson")
        tradeId = int("tradeId")
        symbolRoot = str("symbolRoot")
        direction = str("direction")
        netPnl = (info["netPnl"] as? Double) ?? (info["netPnl"] as? Int).map(Double.init)
        accountId = int("accountId")
        v = int("v")
        badge = (info["badge"] as? Int) ?? (info["badge"] as? Double).map(Int.init)
        unseen = (info["unseen"] as? [Int]) ?? (info["unseen"] as? [Double])?.map(Int.init) ?? []
    }

    init(kind: Kind, id: Int = 0, key: String = "", route: String = "", day: String = "", lesson: String = "", tradeId: Int = 0,
         symbolRoot: String = "", direction: String = "", netPnl: Double? = nil, accountId: Int = 0, v: Int = 0, badge: Int? = nil, unseen: [Int] = []) {
        self.kind = kind
        self.id = id
        self.key = key
        self.route = route
        self.day = day
        self.lesson = lesson
        self.tradeId = tradeId
        self.symbolRoot = symbolRoot
        self.direction = direction
        self.netPnl = netPnl
        self.accountId = accountId
        self.v = v
        self.badge = badge
        self.unseen = unseen
    }

    /// Where a tap goes: the server's route, else Today.
    var destination: Route { Route.from(route: route) ?? .today }

    /// A nudge about an open position (the TJ_POSITION category).
    var isPositionNudge: Bool { kind == .nudge && route.hasPrefix("position/") }

    /// The position a position nudge is about.
    var position: (account: Int, symbol: String)? {
        if case .position(let a, let s) = destination { return (a, s) }
        return nil
    }
}
