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
    var ai: AIState? // absent from a server before 2.18.2
}

/// GET /me "ai": whether this user can run AI work (the server has AI, the user a provider).
struct AIState: Codable, Sendable, Hashable {
    var ready: Bool
}

/// A note's AI review: running, and the last failure, shown once.
struct ReviewState: Codable, Sendable, Hashable {
    var pending: Bool
    var error: String?
}

/// The model's stop and target for an open position, made while it was open.
struct AILevels: Codable, Sendable, Hashable {
    var stop: Double
    var target: Double
    var target2: Double?
    var scaleOut: Double?
    var trail: String?
    var reasons: [String: String]?
    var summary: String?
    var risk: Double?
    var r1: Double?
    var r2: Double?
    var entry: Double
    var qty: Double
}

/// A position's "ai": a proposal being made, or made.
struct PositionAI: Codable, Sendable, Hashable {
    var pending: Bool
    var error: String?
    var levels: AILevels?
    var model: String?
    var at: Date?
    var outdated: Bool?
    var auto: String?
}

/// The model's draft of a day plan, for today or a later day.
struct PlanDraft: Codable, Sendable, Hashable {
    var pending: Bool
    var error: String?
    var maxTrades: Int?
    var stopTime: String?
    var maxLoss: Double?
    var levels: String?
    var notes: String?
    var reasons: [String]?
    var model: String?
    var createdAt: Date?

    /// A draft exists (as opposed to one being written, or nothing).
    var has: Bool { createdAt != nil }
}

/// The AI day run: every note of a day reviewed, the day's synthesis written.
struct DayRunStatus: Codable, Sendable, Hashable {
    var state: String // idle | running | done | error | cancelled
    var date: String?
    var done: Int?
    var total: Int?
    var current: String?
    var err: String?
    var model: String?

    var running: Bool { state == "running" }
}

struct DayRunResponse: Codable, Sendable { var status: DayRunStatus }
struct ReviewStarted: Codable, Sendable { var entryId: Int; var pending: Bool }

/// POST /ai/review: the note to review, by id or by what it is about.
struct ReviewTarget: Encodable, Sendable, Hashable {
    var entryId: Int?
    var scope: String?
    var tradeId: Int?
    var setId: Int?
    var date: String?
    var week: String?

    static func entry(_ id: Int) -> ReviewTarget { .init(entryId: id) }
    static func trade(_ id: Int) -> ReviewTarget { .init(scope: "trade", tradeId: id) }
    static func set(_ id: Int) -> ReviewTarget { .init(scope: "set", setId: id) }
    static func day(_ date: String) -> ReviewTarget { .init(scope: "day", date: date) }
    static func week(_ week: String) -> ReviewTarget { .init(scope: "week", week: week) }
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
    var ai: PositionAI? // the model's proposal, when the server runs AI
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

/// The day per decision: a copy set counts once. What the counts become when trades are grouped by set.
struct TodayDecisions: Codable, Sendable, Hashable {
    var count: Int
    var wins: Int
    var losses: Int
    var open: Int
}

struct TodayPlan: Codable, Sendable, Hashable {
    var plan: DayPlan?
    var outcome: PlanOutcome?
    var tradesUsed: Int
    var maxTrades: Int?
    var lossUsed: Double
    var maxLoss: Double?
    var worstAccount: String
    var draft: PlanDraft? // the model's draft of today's plan, when the server runs AI

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
    var exchangeTz: String? // the exchange's zone, America/Chicago for CME futures
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
    var decisions: TodayDecisions? // absent from a server before 2.18.7

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
    var suggestions: [Suggestion]?
    var review: ReviewState? // absent from a server before 2.18.2
    var chartPng: String?
    var createdAt: Date
    var updatedAt: Date

    var openSuggestions: [Suggestion] { suggestions ?? [] }
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
    /// The checklist rules a trade of this strategy is held to, in order.
    var checklist: [String]
    var createdAt: Date
    var updatedAt: Date
    var archivedAt: Date?

    init(id: Int, name: String, body: String, attachments: [Attachment] = [], checklist: [String] = [], createdAt: Date, updatedAt: Date, archivedAt: Date? = nil) {
        self.id = id; self.name = name; self.body = body; self.attachments = attachments
        self.checklist = checklist; self.createdAt = createdAt; self.updatedAt = updatedAt; self.archivedAt = archivedAt
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(Int.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        body = try c.decodeIfPresent(String.self, forKey: .body) ?? ""
        attachments = try c.decodeIfPresent([Attachment].self, forKey: .attachments) ?? []
        checklist = try c.decodeIfPresent([String].self, forKey: .checklist) ?? []
        createdAt = try c.decode(Date.self, forKey: .createdAt)
        updatedAt = try c.decode(Date.self, forKey: .updatedAt)
        archivedAt = try c.decodeIfPresent(Date.self, forKey: .archivedAt)
    }

    /// The first words of the playbook, for a list row: a heading that opens the playbook (most
    /// repeat the name) is skipped, so the row shows what the setup is.
    var snippet: String {
        var lines = body.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        while let first = lines.first, first.trimmingCharacters(in: .whitespaces).isEmpty { lines.removeFirst() }
        if let first = lines.first, first.hasPrefix("#") { lines.removeFirst() }
        return Markdown.snippet(lines.joined(separator: "\n"))
    }
}

struct StrategyList: Codable, Sendable { var items: [Strategy] }

// MARK: analytics

/// GET /api/v1/analytics/summary, the parts the phone reads: the breakdowns (by strategy among
/// them) and the checklist compliance per strategy. The totals and the rest stay on the web.
struct AnalyticsSummary: Codable, Sendable, Hashable {
    var from: String
    var to: String
    var trades: Int
    var compliance: [ComplianceRow]
    var breakdowns: [String: [AnalyticsBucket]]

    /// The bucket of a breakdown by its key ("byStrategy" and a strategy's name).
    func bucket(_ breakdown: String, _ key: String) -> AnalyticsBucket? {
        breakdowns[breakdown]?.first { $0.key == key }
    }

    func compliance(for strategy: String) -> ComplianceRow? { compliance.first { $0.strategy == strategy } }
}

struct AnalyticsBucket: Codable, Sendable, Hashable, Identifiable {
    var key: String
    var trades: Int
    var wins: Int
    var losses: Int
    var net: Double
    var gross: Double
    var best: Double
    var worst: Double

    var id: String { key }
    var winRate: Double? { trades > 0 ? Double(wins) / Double(trades) : nil }
}

/// How often a strategy's checklist was ticked whole, and what the complete and the incomplete
/// trades made.
struct ComplianceRow: Codable, Sendable, Hashable, Identifiable {
    var strategy: String
    var trades: Int
    var avgPct: Double
    var complete: Int
    var completeNet: Double
    var incompleteNet: Double

    var id: String { strategy }
}

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

// MARK: writes (device key)

/// PUT /journal/{id}: the whole entry; an absent field clears it (nil tag removes the tag).
struct JournalUpdate: Codable, Sendable, Hashable {
    var body: String
    var tag: JournalTag?
    var mood: Int?
    var strategyId: Int?
    var mistakes: [String]
    var emotions: [String]
    var checks: [Int]
    var updatedAt: Date

    enum CodingKeys: String, CodingKey { case body, tag, mood, strategyId, mistakes, emotions, checks, updatedAt }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(body, forKey: .body)
        if let tag, tag != .unknown { try c.encode(tag, forKey: .tag) }
        if let mood { try c.encode(mood, forKey: .mood) }
        if let strategyId { try c.encode(strategyId, forKey: .strategyId) }
        try c.encode(mistakes, forKey: .mistakes)
        try c.encode(emotions, forKey: .emotions)
        try c.encode(checks, forKey: .checks)
        try c.encode(updatedAt, forKey: .updatedAt)
    }

    /// What the entry holds now, to edit from.
    init(_ e: JournalDetail) {
        body = e.notes
        tag = e.tag == .unknown ? nil : e.tag
        mood = e.mood
        strategyId = e.strategy?.id
        mistakes = e.labels.mistakes
        emotions = e.labels.emotions
        checks = e.checklist?.items.filter(\.checked).map(\.id) ?? []
        updatedAt = e.updatedAt
    }

    /// A draft kept on the phone, as encoded above.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        body = try c.decode(String.self, forKey: .body)
        tag = try c.decodeIfPresent(JournalTag.self, forKey: .tag)
        mood = try c.decodeIfPresent(Int.self, forKey: .mood)
        strategyId = try c.decodeIfPresent(Int.self, forKey: .strategyId)
        mistakes = try c.decodeIfPresent([String].self, forKey: .mistakes) ?? []
        emotions = try c.decodeIfPresent([String].self, forKey: .emotions) ?? []
        checks = try c.decodeIfPresent([Int].self, forKey: .checks) ?? []
        updatedAt = try c.decode(Date.self, forKey: .updatedAt)
    }
}

/// POST /journal: one scope's fields.
struct JournalCreate: Encodable, Sendable, Hashable {
    var scope: JournalScope
    var body: String
    var tag: JournalTag?
    var mood: Int?
    var strategyId: Int?
    var date: String?
    var week: String?
    var tradeId: Int?
    var tradeIds: [Int]?
    var mistakes: [String] = [] // labels from the vocabulary, any scope
    var emotions: [String] = []

    enum CodingKeys: String, CodingKey { case scope, body, tag, mood, strategyId, date, week, tradeId, tradeIds, mistakes, emotions }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(scope, forKey: .scope)
        try c.encode(body, forKey: .body)
        if let tag, tag != .unknown { try c.encode(tag, forKey: .tag) }
        try c.encodeIfPresent(mood, forKey: .mood)
        try c.encodeIfPresent(strategyId, forKey: .strategyId)
        try c.encodeIfPresent(date, forKey: .date)
        try c.encodeIfPresent(week, forKey: .week)
        try c.encodeIfPresent(tradeId, forKey: .tradeId)
        try c.encodeIfPresent(tradeIds, forKey: .tradeIds)
        if !mistakes.isEmpty { try c.encode(mistakes, forKey: .mistakes) }
        if !emotions.isEmpty { try c.encode(emotions, forKey: .emotions) }
    }
}

// In an extension, so the memberwise init stays.
extension JournalCreate: Decodable {
    /// A draft kept on the phone, as encoded above.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        scope = try c.decode(JournalScope.self, forKey: .scope)
        body = try c.decode(String.self, forKey: .body)
        tag = try c.decodeIfPresent(JournalTag.self, forKey: .tag)
        mood = try c.decodeIfPresent(Int.self, forKey: .mood)
        strategyId = try c.decodeIfPresent(Int.self, forKey: .strategyId)
        date = try c.decodeIfPresent(String.self, forKey: .date)
        week = try c.decodeIfPresent(String.self, forKey: .week)
        tradeId = try c.decodeIfPresent(Int.self, forKey: .tradeId)
        tradeIds = try c.decodeIfPresent([Int].self, forKey: .tradeIds)
        mistakes = try c.decodeIfPresent([String].self, forKey: .mistakes) ?? []
        emotions = try c.decodeIfPresent([String].self, forKey: .emotions) ?? []
    }
}

/// POST /journal/attachments → 201.
struct AttachmentUpload: Codable, Sendable, Hashable {
    var id: Int
    var url: String
    var name: String?
    var size: Int?
    var markdown: String
}

struct NoteAppended: Codable, Sendable { var id: Int; var appended: Bool }

/// POST /plan: present fields replace the stored ones (an empty string or 0 clears).
struct DayPlanSave: Encodable, Sendable, Hashable {
    var day: String
    var bias: Bias
    var levels: String
    var maxTrades: Int
    var stopTime: String
    var maxLoss: Double
    var notes: String

    enum CodingKeys: String, CodingKey { case day, bias, levels, maxTrades, stopTime, maxLoss, notes }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(day, forKey: .day)
        try c.encode(bias == .unknown ? "" : bias.rawValue, forKey: .bias)
        try c.encode(levels, forKey: .levels)
        try c.encode(maxTrades, forKey: .maxTrades)
        try c.encode(stopTime, forKey: .stopTime)
        try c.encode(maxLoss, forKey: .maxLoss)
        try c.encode(notes, forKey: .notes)
    }

    init(day: String, plan: DayPlan?) {
        self.day = day
        bias = plan?.bias ?? .none
        levels = plan?.levels ?? ""
        maxTrades = plan?.maxTrades ?? 0
        stopTime = plan?.stopTime ?? ""
        maxLoss = plan?.maxLoss ?? 0
        notes = plan?.notes ?? ""
    }
}

struct DayPlanSaved: Codable, Sendable { var plan: DayPlan? }

// In an extension, so the memberwise init stays.
extension DayPlanSave: Decodable {
    /// A draft kept on the phone, as encoded above (an empty bias is none).
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        day = try c.decode(String.self, forKey: .day)
        let b = try c.decodeIfPresent(String.self, forKey: .bias) ?? ""
        bias = b.isEmpty ? .none : (Bias(rawValue: b) ?? .none)
        levels = try c.decodeIfPresent(String.self, forKey: .levels) ?? ""
        maxTrades = try c.decodeIfPresent(Int.self, forKey: .maxTrades) ?? 0
        stopTime = try c.decodeIfPresent(String.self, forKey: .stopTime) ?? ""
        maxLoss = try c.decodeIfPresent(Double.self, forKey: .maxLoss) ?? 0
        notes = try c.decodeIfPresent(String.self, forKey: .notes) ?? ""
    }
}

/// POST /positions/{account}/{symbol}/plan: the whole plan; a null stop clears it.
struct PositionPlanSave: Encodable, Sendable, Hashable {
    var stop: Double?
    var risk: Double?
    var target: Double?
    var thesis: String
    var allLegs: Bool

    enum CodingKeys: String, CodingKey { case stop, risk, target, thesis, allLegs }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(stop, forKey: .stop) // null on the wire clears
        try c.encode(risk, forKey: .risk)
        try c.encode(target, forKey: .target)
        try c.encode(thesis, forKey: .thesis)
        try c.encode(allLegs, forKey: .allLegs)
    }
}

// In an extension, so the memberwise init stays.
extension PositionPlanSave: Decodable {
    /// A draft kept on the phone, as encoded above.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        stop = try c.decodeIfPresent(Double.self, forKey: .stop)
        risk = try c.decodeIfPresent(Double.self, forKey: .risk)
        target = try c.decodeIfPresent(Double.self, forKey: .target)
        thesis = try c.decodeIfPresent(String.self, forKey: .thesis) ?? ""
        allLegs = try c.decodeIfPresent(Bool.self, forKey: .allLegs) ?? true
    }
}

struct PositionSaved: Codable, Sendable { var position: Position }

struct SettingsUpdate: Encodable, Sendable, Hashable {
    var guardrail: Bool?
    var autoPlan: Int?
}

struct SettingsSaved: Codable, Sendable { var settings: UserSettings }

struct BalanceAnchorAdd: Encodable, Sendable, Hashable {
    var date: String
    var balance: Double
    var note: String
}

struct AccountSaved: Codable, Sendable { var account: Account }

struct NudgeIDs: Encodable, Sendable { var ids: [Int] }

/// The 409 a journal save can meet: an AI review is writing, or the entry changed meanwhile.
struct JournalConflict: Decodable, Sendable {
    var error: String?
    var reason: String?
    var entry: JournalDetail?
    var entryId: Int?
}

// MARK: the AI review's suggestions

struct Suggestion: Codable, Sendable, Hashable, Identifiable {
    var kind: String // tag | strategy | mistake | emotion
    var value: String
    var name: String
    var id: String { kind + ":" + value }
    var title: String {
        switch kind {
        case "tag": "tag \(value)"
        case "strategy": name
        case "mistake": "mistake: \(name)"
        case "emotion": "felt \(name)"
        default: name
        }
    }
}

struct SuggestionAction: Encodable, Sendable {
    var kind: String
    var value: String
    var `do`: String // accept | dismiss
}

// MARK: the weekly digest

struct DigestKPI: Codable, Sendable, Hashable, Identifiable {
    var key: String // net | decisions | winRate | profitFactor | avgR | exitKept
    var label: String
    var value: Double?
    var previous: Double?
    var id: String { key }

    /// The number as the key wants it shown.
    var text: String { Self.format(key, value) }
    var previousText: String { Self.format(key, previous) }

    nonisolated static func format(_ key: String, _ v: Double?) -> String {
        guard let v else { return "—" }
        switch key {
        case "net": return Formatting.money(v)
        case "decisions": return String(Int(v))
        case "winRate", "exitKept": return Formatting.percent(v)
        case "avgR": return Formatting.r(v)
        default: return String(format: "%.2f", v)
        }
    }
}

struct DigestDecision: Codable, Sendable, Hashable {
    var tradeId: Int
    var day: String
    var symbolRoot: String
    var direction: Direction
    var accounts: Int
    var net: Double
}

struct DigestCoach: Codable, Sendable, Hashable, Identifiable {
    var key: String
    var title: String
    var severity: String
    var count: Int
    var days: Int
    var id: String { key }
}

struct DigestRule: Codable, Sendable, Hashable, Identifiable {
    var key: String
    var name: String
    var held: Int
    var total: Int
    var id: String { key }
}

struct DigestPlan: Codable, Sendable, Hashable {
    var tradingDays: Int
    var planned: Int
    var writtenLate: Int
    var rules: [DigestRule]
}

struct DigestLesson: Codable, Sendable, Hashable, Identifiable {
    var mistake: String? // absent for a lesson about no mistake
    var mistakeName: String?
    var count: Int
    var open: Int
    var lesson: String
    var day: String?
    var id: String { (mistake ?? "") + ":" + lesson }
}

struct Digest: Codable, Sendable, Hashable {
    var week: String
    var from: String
    var to: String
    var tradingDays: Int
    var kpis: [DigestKPI]
    var keep: [String]
    var fix: [String]
    var bestDecision: DigestDecision?
    var worstDecision: DigestDecision?
    var coach: [DigestCoach]
    var plan: DigestPlan
    var lessons: [DigestLesson]
}
