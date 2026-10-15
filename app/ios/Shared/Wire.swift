// The journal's wire types, as the app reads them: the JSON of /api/v1 (API.md) decoded as is,
// camelCase keys, RFC 3339 times. Display logic lives beside the data as computed properties;
// there is no second model. Every enum the server may extend decodes unknown values to a
// catch-all so a newer server never blanks the app. Optional on the wire is optional here.
import Foundation

enum Wire {
    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let s = try decoder.singleValueContainer().decode(String.self)
            if let date = Wire.parseDate(s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "not a date: \(s)"))
        }
        return d
    }()

    static let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .custom { date, encoder in
            var c = encoder.singleValueContainer()
            try c.encode(Wire.withFraction.string(from: date))
        }
        return e
    }()

    nonisolated(unsafe) private static let withFraction: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    nonisolated(unsafe) private static let plain: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    /// RFC 3339 as Go writes it: fractional seconds and a zone offset, or neither.
    nonisolated static func parseDate(_ s: String) -> Date? {
        withFraction.date(from: s) ?? plain.date(from: s)
    }

    nonisolated static func rfc3339(_ d: Date) -> String { withFraction.string(from: d) }
}

// MARK: enums with a catch-all

enum Direction: String, Codable, Sendable, Hashable {
    case long, short, unknown
    init(from decoder: Decoder) throws {
        self = Direction(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
    var label: String { rawValue }
}

enum JournalScope: String, Codable, Sendable, Hashable {
    case day, week, trade, group, unknown
    init(from decoder: Decoder) throws {
        self = JournalScope(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
}

enum JournalTag: String, Codable, Sendable, Hashable {
    case good, neutral, bad, unknown
    init(from decoder: Decoder) throws {
        self = JournalTag(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
}

enum Bias: String, Codable, Sendable, Hashable {
    case long, short, neutral, flat, none = "", unknown
    init(from decoder: Decoder) throws {
        self = Bias(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
    var label: String { self == .none ? "no bias" : rawValue }
}

enum AccountType: String, Codable, Sendable, Hashable {
    case live, propLive = "prop_live", propFunded = "prop_funded", propEval = "prop_eval", paper, unknown
    init(from decoder: Decoder) throws {
        self = AccountType(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
}

enum FillSide: String, Codable, Sendable, Hashable {
    case buy, sell, unknown
    init(from decoder: Decoder) throws {
        self = FillSide(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
}

enum FillRole: String, Codable, Sendable, Hashable {
    case entry, exit, unknown
    init(from decoder: Decoder) throws {
        self = FillRole(rawValue: try decoder.singleValueContainer().decode(String.self)) ?? .unknown
    }
}

// MARK: me, pairing

struct UserInfo: Codable, Sendable, Hashable {
    var id: Int
    var username: String
    var displayName: String
    var email: String
    var name: String { displayName.isEmpty ? username : displayName }
}

struct ServerFeatures: Codable, Sendable, Hashable {
    var ai: Bool
    var charts: Bool
    var guardrail: Bool
    var push: Bool
}

struct ServerInfo: Codable, Sendable, Hashable {
    var url: String
    var version: String
    var timezone: String
    var features: ServerFeatures
    var tz: TimeZone { TimeZone(identifier: timezone) ?? .current }
}

struct UserSettings: Codable, Sendable, Hashable {
    var guardrail: Bool
    var autoPlan: Int
}

struct KeyInfo: Codable, Sendable, Hashable {
    var kind: String
    var name: String
    var createdAt: Date
}

struct Me: Codable, Sendable, Hashable {
    var user: UserInfo
    var server: ServerInfo
    var settings: UserSettings
    var key: KeyInfo
}

struct PairRequest: Encodable, Sendable {
    var code: String
    var deviceName: String
}

struct PairResponse: Codable, Sendable {
    var key: String
    var server: ServerInfo
    var user: UserInfo
}

struct DataVersion: Codable, Sendable { var v: Int }

// MARK: stats

struct DayNet: Codable, Sendable, Hashable {
    var date: String
    var net: Double
}

struct StatsTotals: Codable, Sendable, Hashable {
    var trades: Int
    var net: Double
    var gross: Double
    var commission: Double
    var wins: Int
    var losses: Int
    var winRate: Double?
    var winDays: Int?
    var lossDays: Int?
    var bestDay: DayNet?
    var worstDay: DayNet?
    var largestWin: Double?
    var largestLoss: Double?
}

struct DailyStatsItem: Codable, Sendable, Hashable, Identifiable {
    var date: String
    var trades: Int
    var net: Double
    var gross: Double
    var commission: Double
    var wins: Int
    var losses: Int
    var winRate: Double?
    var largestWin: Double?
    var largestLoss: Double?
    var id: String { date }
}

struct DailyStats: Codable, Sendable, Hashable {
    var from: String
    var to: String
    var timezone: String
    var totals: StatsTotals
    var items: [DailyStatsItem]
}

// MARK: positions

struct CycleFill: Codable, Sendable, Hashable {
    var time: Date
    var side: FillSide
    var qty: Double
    var price: Double
}

struct Position: Codable, Sendable, Hashable, Identifiable {
    var accountId: Int
    var account: String
    var firm: String?
    var symbol: String
    var symbolRoot: String
    var direction: Direction
    var qty: Double
    var avgEntry: Double?
    var openedAt: Date?
    var fills: Int
    var lastPrice: Double?
    var lastAt: Date?
    var priceSource: String?
    var unrealized: Double?
    var stop: Double?
    var risk: Double?
    var target: Double?
    var thesis: String?
    var planSource: String?
    var setPlan: Bool
    var ticksToStop: Double?
    var beyondStop: Bool
    var setLegs: [Int]
    var stale: Bool
    var planUrl: String
    var tickSize: Double
    var tickValue: Double
    var pointValue: Double
    var rewardRisk: Double?
    var cycleFills: [CycleFill]
    var openPrice: Double
    var addedFills: Int
    var addedQty: Double

    var id: String { "\(accountId):\(symbol)" }
    var hasPlan: Bool { stop != nil }
    /// "long 3 @ 7766.33"
    var headline: String { "\(direction.label) \(Formatting.qty(qty)) @ \(Formatting.price(avgEntry, tickSize: tickSize))" }
    var isSuggestion: Bool { planSource == "ai" || planSource == "api" }
    var thesisText: String { thesis ?? "" }
    var planSourceName: String { planSource ?? "" }
    var priceSourceName: String { priceSource ?? "" }
}

struct PositionList: Codable, Sendable { var items: [Position] }

// MARK: the day plan and nudges

struct DayPlan: Codable, Sendable, Hashable {
    var day: String
    var bias: Bias
    var levels: String
    var maxTrades: Int?
    var stopTime: String
    var maxLoss: Double?
    var notes: String
    var createdAt: Date
    var updatedAt: Date
}

struct PlanCheck: Codable, Sendable, Hashable, Identifiable {
    var key: String
    var rule: String
    var actual: String
    var held: Bool
    var id: String { key }
}

struct PlanOutcome: Codable, Sendable, Hashable {
    var plan: DayPlan
    var checks: [PlanCheck]
    var held: Int
    var writtenLate: Bool
    var editedLate: Bool
}

struct TodayPlan: Codable, Sendable, Hashable {
    var plan: DayPlan?
    var outcome: PlanOutcome?
    var tradesUsed: Int
    var maxTrades: Int?
    var lossUsed: Double
    var maxLoss: Double?
    var worstAccount: String

    /// 0…1 of the max-loss budget used, nil without a budget.
    var lossFraction: Double? {
        guard let maxLoss, maxLoss > 0 else { return nil }
        return min(1, lossUsed / maxLoss)
    }
}

struct Nudge: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var day: String
    var key: String
    var ref: String
    var severity: String
    var title: String
    var detail: String
    var lesson: String?
    var href: String
    var route: String
    var createdAt: Date
    var deliveredAt: Date?
    var seenAt: Date?

    var unseen: Bool { seenAt == nil }
    var isWarning: Bool { severity == "warn" || severity == "error" }
}

struct NudgeList: Codable, Sendable {
    var day: String
    var unseen: Int
    var items: [Nudge]
}

struct TodayNudges: Codable, Sendable, Hashable {
    var unseen: Int
    var badge: Int
    var items: [Nudge]
}

// MARK: today

struct TradingSession: Codable, Sendable, Hashable {
    var rthStart: Date
    var rthEnd: Date
    var inGlobex: Bool
    var inRth: Bool
}

struct Unrealized: Codable, Sendable, Hashable {
    var total: Double
    var stale: Bool
    var priced: Int
    var unpriced: Int
}

struct LastTrade: Codable, Sendable, Hashable {
    var id: Int
    var symbolRoot: String
    var direction: Direction
    var netPnl: Double
    var exitTime: Date
    var account: String
}

struct CurvePoint: Codable, Sendable, Hashable {
    var t: Date
    var net: Double
}

struct EquityByType: Codable, Sendable, Hashable, Identifiable {
    var type: String
    var label: String
    var total: Double
    var accounts: Int
    var id: String { type }
}

struct TodayEquity: Codable, Sendable, Hashable {
    var known: Bool
    var now: Double
    var byType: [EquityByType]
    var dayChange: Double
    var currentDrawdown: Double
    var underwater: Bool
}

struct Today: Codable, Sendable, Hashable {
    var day: String
    var timezone: String
    var v: Int
    var asOf: Date
    var session: TradingSession
    var stats: StatsTotals
    var unrealized: Unrealized
    var positions: [Position]
    var plan: TodayPlan
    var nudges: TodayNudges
    var lastFillAt: Date?
    var lastTrade: LastTrade?
    var curve: [CurvePoint]
    var equity: TodayEquity

    var tz: TimeZone { TimeZone(identifier: timezone) ?? .current }
    /// Realised net plus the open positions' unrealised.
    var total: Double { stats.net + unrealized.total }
    var openCount: Int { positions.count }
    /// Something is happening today: a trade closed or a position is open.
    var active: Bool { stats.trades > 0 || !positions.isEmpty }
}

// MARK: trades

struct TradeRow: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var accountId: Int
    var account: String
    var firm: String?
    var symbol: String
    var symbolRoot: String
    var direction: Direction
    var qty: Double
    var entryPrice: Double
    var exitPrice: Double?
    var entryTime: Date
    var exitTime: Date?
    var grossPnl: Double?
    var commission: Double?
    var netPnl: Double?
    var plannedStop: Double?
    var plannedRisk: Double?
    var r: Double?
    var mae: Double?
    var mfe: Double?
    var maePoints: Double?
    var mfePoints: Double?
    var exitEfficiency: Double?
    var excursionBars: String?
    var setId: Int?
    var setSize: Int
    var status: String
    var retired: Bool?
    var hasJournal: Bool
    var journalTag: String?
    var groupCount: Int

    var isOpen: Bool { status == "open" }
    /// The moment that places the trade in a day: its exit, or its entry while open.
    var when: Date { exitTime ?? entryTime }
}

struct PageInfo: Codable, Sendable, Hashable {
    var page: Int
    var pageSize: Int
    var pages: Int
    var total: Int
}

struct TradeList: Codable, Sendable {
    var items: [TradeRow]
    var page: PageInfo
}

struct Execution: Codable, Sendable, Hashable, Identifiable {
    var time: Date
    var side: FillSide
    var role: FillRole
    var qty: Double
    var price: Double
    var id: String { "\(time.timeIntervalSince1970) \(side.rawValue) \(price)" }
}

struct TradeDetail: Codable, Sendable, Hashable {
    var id: Int
    var accountId: Int
    var account: String
    var firm: String?
    var symbol: String
    var symbolRoot: String
    var direction: Direction
    var qty: Double
    var entryPrice: Double
    var exitPrice: Double?
    var entryTime: Date
    var exitTime: Date?
    var grossPnl: Double?
    var commission: Double?
    var netPnl: Double?
    var plannedStop: Double?
    var plannedRisk: Double?
    var r: Double?
    var mae: Double?
    var mfe: Double?
    var maePoints: Double?
    var mfePoints: Double?
    var exitEfficiency: Double?
    var excursionBars: String?
    var setId: Int?
    var setSize: Int
    var status: String
    var brokerTradeId: String?
    var hasJournal: Bool
    var journalEntryId: Int?
    var journalTag: String?
    var groupCount: Int
    var executions: [Execution]
    var chartPng: String?

    var isOpen: Bool { status == "open" }
}

// MARK: journal

struct Labels: Codable, Sendable, Hashable {
    var mistakes: [String]
    var emotions: [String]
}

struct Summary: Codable, Sendable, Hashable {
    var trades: Int
    var net: Double
    var gross: Double
    var commission: Double
    var wins: Int
    var losses: Int
}

struct StrategyRef: Codable, Sendable, Hashable {
    var id: Int
    var name: String
}

struct JournalLinks: Codable, Sendable, Hashable {
    var `self`: String?
    var chartPng: String?
}

struct JournalRow: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var scope: JournalScope
    var date: String
    var tag: JournalTag?
    var mood: Int?
    var snippet: String
    var summary: Summary?
    /// The strategy's name (the list carries the name, the detail the id and name).
    var strategy: String?
    var labels: Labels?
    var links: JournalLinks?

    var plainSnippet: String { Markdown.snippet(snippet) }
}

struct JournalList: Codable, Sendable {
    var items: [JournalRow]
}

struct ChecklistItem: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var text: String
    var checked: Bool
}

struct Checklist: Codable, Sendable, Hashable {
    var strategyId: Int
    var done: Int
    var total: Int
    var items: [ChecklistItem]
}

struct JournalTrade: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var symbol: String
    var symbolRoot: String
    var direction: Direction
    var qty: Double
    var account: String
    var entryTime: Date
    var exitTime: Date?
    var grossPnl: Double?
    var commission: Double?
    var netPnl: Double?
}

struct Attachment: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var url: String
}

struct JournalDetail: Codable, Sendable, Hashable {
    var id: Int
    var scope: JournalScope
    var date: String
    var tag: JournalTag?
    var mood: Int?
    var notes: String
    var labels: Labels
    var checklist: Checklist?
    var strategy: StrategyRef?
    var summary: Summary?
    var trades: [JournalTrade]
    var attachments: [Attachment]
    var chartPng: String?
    var createdAt: Date
    var updatedAt: Date
}

struct LabelOption: Codable, Sendable, Hashable, Identifiable {
    var key: String
    var name: String
    var id: String { key }
}

struct LabelVocabulary: Codable, Sendable, Hashable {
    var mistakes: [LabelOption]
    var emotions: [LabelOption]
}

struct Strategy: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var name: String
    var body: String
    var attachments: [Attachment]
    var createdAt: Date
    var updatedAt: Date
}

struct StrategyList: Codable, Sendable { var items: [Strategy] }

// MARK: accounts and equity

struct BalanceAnchor: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var date: String
    var balance: Double
    var note: String
}

struct AccountBalance: Codable, Sendable, Hashable {
    var balance: Double
    var asOf: String
    var anchorDate: String
    var unchangedSinceAnchor: Bool
    var anchors: [BalanceAnchor]
}

struct Account: Codable, Sendable, Hashable, Identifiable {
    var id: Int
    var name: String
    var firm: String?
    var type: AccountType?
    var typeLabel: String
    var currency: String
    var active: Bool
    var retired: Bool
    var excludedFromStats: Bool
    var needsSetup: Bool
    var copyGroup: String?
    var importSource: String
    var balance: AccountBalance?
    var dayNet: Double?
}

struct AccountList: Codable, Sendable { var items: [Account] }

struct EquityPoint: Codable, Sendable, Hashable {
    var date: String
    var equity: Double
}

struct Drawdown: Codable, Sendable, Hashable {
    var maxDrawdown: Double
    var maxDrawdownPeakAt: Date?
    var maxDrawdownTroughAt: Date?
    var maxDrawdownRecoverAt: Date?
    var maxDrawdownDays: Int
    var currentDrawdown: Double
    var underwater: Bool
    var peakAt: Date?
}

struct Equity: Codable, Sendable, Hashable {
    var asOf: String
    var equityNow: Double
    var byType: [EquityByType]
    var included: Int
    var excluded: Int
    var series: [EquityPoint]
    var drawdown: Drawdown
    var dayChange: Double
}
