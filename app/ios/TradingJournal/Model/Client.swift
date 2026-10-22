// The journal client: the Today state kept live over the server's event stream (SSE on
// /api/v1/events, a version per change), one-off reads for the other screens, and every action
// as a plain HTTP call. Everything runs on the main actor; the stream's awaits suspend, nothing
// blocks. Connection state is part of the model and the UI says it honestly.
//
// Every read goes through the Store: a live answer is kept, and when the journal is out of
// reach the kept one comes back with the time it arrived, so the screens show the last known
// state instead of an error. While the stream is down the kept answer comes first, without a
// wait on the network; the screens load again when the stream is back.
import Foundation
import Network
import Observation
import OSLog
import UIKit
import WidgetKit

@MainActor
@Observable
final class Client {
    enum Connection: Equatable {
        case off // not paired
        case connecting
        case live
        case down(since: Date)
        case unauthorized

        var isLive: Bool { self == .live }
        /// The journal is known to be out of reach (or not yet reached): a stored answer comes
        /// first, without a wait on the network.
        var isOffline: Bool {
            switch self {
            case .connecting, .down: true
            case .off, .live, .unauthorized: false
            }
        }
    }

    static let shared = Client(settings: .shared)
    private let log = Logger(subsystem: "nl.w4j.tradingjournal", category: "client")

    let settings: Settings
    private(set) var today: Today?
    private(set) var connection: Connection = .off
    /// The last data version seen (from the stream or /today); the stream's event moves it.
    private(set) var version: Int = 0
    /// When /today last answered, to say "no news since 09:12" when the stream is down.
    private(set) var asOf: Date?
    /// When `today` is the stored one: the time the server gave it. Nil while it is live.
    private(set) var todayAsOf: Date?
    /// Counts the stream's openings: a screen on a stored answer loads again when it moves.
    private(set) var reconnects = 0
    private(set) var lastError: String?
    /// Set by a notification tap, a deep link or a launch argument: RootView navigates there and clears it.
    var pendingRoute: Route?
    /// The last trading days for the widgets' strip, refreshed every quarter hour at most.
    private var recentDays: [TodayCache.DayCell] = []
    private var recentDaysAt: Date?

    private let session: URLSession
    private let streamSession: URLSession
    private var generation = 0
    private var attempt = 0
    private var streamTask: Task<Void, Never>?
    private var refetch: Task<Void, Never>?
    /// The stores of the current pairing, opened on first use and again when the pairing changes.
    private let storeRoot: URL
    private var stores: (scope: String, json: Store, images: Store)?
    private var warmTask: Task<Void, Never>?
    private var warmedVersion = -1
    /// The network's state: when it comes back the stream is tried at once, not after the backoff.
    private let path = NWPathMonitor()
    private var networkUp = true

    /// `protocolClasses` lets a test answer the requests itself; `storeRoot` keeps a test's
    /// store apart from the app's.
    init(settings: Settings, storeRoot: URL = Store.defaultRoot, protocolClasses: [AnyClass] = []) {
        self.settings = settings
        self.storeRoot = storeRoot
        let cfg = URLSessionConfiguration.default
        cfg.waitsForConnectivity = true
        cfg.timeoutIntervalForRequest = 20
        cfg.timeoutIntervalForResource = 60 // waiting for connectivity ends here, not days later
        if !protocolClasses.isEmpty { cfg.protocolClasses = protocolClasses }
        session = URLSession(configuration: cfg)
        let scfg = URLSessionConfiguration.default
        scfg.timeoutIntervalForRequest = 90 // the server pings every 25 s; silence longer than this is a dead stream
        scfg.timeoutIntervalForResource = .infinity
        if !protocolClasses.isEmpty { scfg.protocolClasses = protocolClasses }
        streamSession = URLSession(configuration: scfg)
        path.pathUpdateHandler = { [weak self] p in
            let up = p.status == .satisfied
            Task { @MainActor [weak self] in self?.network(up: up) }
        }
        path.start(queue: DispatchQueue(label: "nl.w4j.tradingjournal.path", qos: .utility))
    }

    /// The network came or went. Back after being gone, with the stream down: try it now.
    private func network(up: Bool) {
        let was = networkUp
        networkUp = up
        if up, !was, connection.isOffline, streamTask != nil {
            log.info("network back: reconnecting")
            reconnectNow()
        }
    }

    /// Drops the current stream attempt (and its backoff) and connects again at once.
    private func reconnectNow() {
        guard settings.isPaired else { return }
        generation += 1
        let gen = generation
        streamTask?.cancel()
        attempt = 0
        connection = .connecting
        streamTask = Task { await self.run(gen: gen) }
    }

    /// A read could not reach the journal while the stream seemed live: the stream's socket
    /// has not noticed yet (it would, after 90 s of silence). Say so now and probe again, so
    /// the screens learn of the return as soon as it comes.
    private func lostReach() {
        guard connection.isLive else { return }
        connection = .down(since: .now)
        reconnectNow()
    }

    var api: TJAPI? { settings.api }
    var tz: TimeZone { today?.tz ?? settings.tz }

    // MARK: lifecycle

    /// Fetches Today and opens the event stream; called when the app comes to the front.
    func start() {
        stop()
        guard settings.isPaired else {
            connection = .off
            return
        }
        generation += 1
        let gen = generation
        connection = .connecting
        let st = store()
        Task(priority: .utility) { await st.json.sweep(); await st.images.sweep() }
        Task { await self.refreshToday() }
        streamTask = Task { await self.run(gen: gen) }
    }

    /// Closes the stream; called when the app goes to the background.
    func stop() {
        generation += 1
        streamTask?.cancel()
        streamTask = nil
        refetch?.cancel()
        refetch = nil
        warmTask?.cancel()
        warmTask = nil
    }

    /// After pairing or signing out.
    func reset() {
        stop()
        today = nil
        asOf = nil
        todayAsOf = nil
        version = 0
        warmedVersion = -1
        lastError = nil
        start()
    }

    // MARK: the store

    /// The stores of the pairing as it is now; a change of server or key opens new ones and
    /// removes every other pairing's answers.
    private func store() -> (scope: String, json: Store, images: Store) {
        let scope = Store.scope(serverURL: settings.serverURL, key: settings.apiKey)
        if let stores, stores.scope == scope { return stores }
        let opened = Store.open(root: storeRoot, scope: scope)
        stores = (scope, opened.json, opened.images)
        Store.wipeOthers(root: storeRoot, than: scope)
        return stores!
    }

    /// How much the phone keeps, for Settings.
    func storedBytes() async -> Int {
        let st = store()
        return await st.json.bytes + st.images.bytes
    }

    /// Forgets every stored answer; the screens fetch again.
    func clearStore() async {
        let st = store()
        await st.json.wipe()
        await st.images.wipe()
        warmedVersion = -1
    }

    /// A failure to reach the journal, as against an answer from it: the stored answer stands in.
    nonisolated static func isReachability(_ error: Error) -> Bool {
        if error is URLError { return true }
        if case TJError.http(let code, _) = error { return code == 502 || code == 503 || code == 504 }
        return false
    }

    private func run(gen: Int) async {
        while generation == gen, !Task.isCancelled {
            guard let api else { return }
            var failure: Connection = .down(since: .now)
            do {
                var req = api.request("GET", "/events", timeout: 90)
                req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                req.setValue("no-cache", forHTTPHeaderField: "Cache-Control")
                let (bytes, resp) = try await streamSession.bytes(for: req)
                let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
                if code == 401 {
                    connection = .unauthorized
                    return
                }
                guard code == 200 else { throw TJError.http(code, "") }
                guard generation == gen else { return }
                log.info("stream open")
                connection = .live
                attempt = 0
                reconnects += 1
                // The day shown may be the stored one; the version event alone would leave it
                // so when nothing changed meanwhile.
                if todayAsOf != nil { Task { await self.refreshToday() } }
                var event = ""
                // The server's events are one data line each, so an event is complete at its
                // data line; AsyncLineSequence is not relied on to deliver the blank line after it.
                for try await line in bytes.lines {
                    guard generation == gen else { return }
                    if line.hasPrefix("event:") {
                        event = line.dropFirst(6).trimmingCharacters(in: .whitespaces)
                    } else if line.hasPrefix("data:") {
                        let data = line.dropFirst(5).trimmingCharacters(in: .whitespaces)
                        if let ev = SSE.parse(event: event, data: data) {
                            log.debug("stream event \(String(describing: ev), privacy: .public)")
                            handle(ev)
                        }
                        event = ""
                    }
                    // ": ping" and "retry:" lines fall through
                }
                log.info("stream closed by the server")
            } catch {
                if generation != gen { return }
                if case TJError.unauthorized = error {
                    connection = .unauthorized
                    return
                }
                failure = .down(since: .now)
                lastError = (error as? URLError)?.localizedDescription ?? error.localizedDescription
                log.error("stream failed: \(error.localizedDescription, privacy: .public)")
            }
            guard generation == gen else { return }
            connection = failure
            let delay = min(30.0, pow(2.0, Double(min(attempt, 5))))
            attempt += 1
            try? await Task.sleep(for: .seconds(delay))
        }
    }

    private func handle(_ ev: SSE.Event) {
        switch ev {
        case .version(let v):
            guard v != version else { return }
            version = v
            // Several bumps in a row (an import's fills) become one fetch.
            refetch?.cancel()
            refetch = Task {
                try? await Task.sleep(for: .milliseconds(500))
                guard !Task.isCancelled else { return }
                await self.refreshToday()
            }
        }
    }

    // MARK: today

    /// One fetch of Today (pull to refresh; the stream triggers the others).
    func refreshToday(refresh: Bool = false) async {
        guard api != nil else { return }
        // The stored day shows at once; the live one replaces it when it comes.
        if today == nil, let (data, at) = await store().json.get(Store.key("/today")), let t = try? Wire.decoder.decode(Today.self, from: data) {
            today = t
            todayAsOf = at
        }
        do {
            let l = try await get(Today.self, "/today", query: refresh ? [URLQueryItem(name: "refresh", value: "1")] : [], fresh: true)
            let t = l.value
            if t != today {
                today = t
            }
            if l.isStale {
                todayAsOf = l.asOf
                return
            }
            todayAsOf = nil
            asOf = .now
            lastError = nil
            log.debug("today v=\(t.v) trades=\(t.stats.trades) open=\(t.positions.count) unseen=\(t.nudges.unseen)")
            if t.v > version { version = t.v }
            if settings.server?.timezone != t.timezone, var s = settings.server {
                s.timezone = t.timezone
                settings.server = s
            }
            try? await UNUserNotificationCenterBadge.set(t.nudges.badge)
            await Notifications.clearDelivered(unseen: Set(t.nudges.items.filter(\.unseen).map(\.id)))
            await refreshRecentDays(t)
            TodayCache.from(t, days: recentDays).write()
            WidgetCenter.shared.reloadAllTimelines()
            LiveActivities.shared.ensureSession(t, allowed: settings.notificationPrefs.showsActivity)
            warm(t)
        } catch TJError.unauthorized {
            connection = .unauthorized
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Keeps what the other screens will want, once per data version, at a low priority: the
    /// month's calendar, the first page of trades and of the journal, the notes and trades of
    /// the last two weeks, the vocabularies, accounts, equity, the digest. So the phone has them
    /// when the journal is out of reach, without the trader having opened every screen first.
    private func warm(_ t: Today) {
        guard t.v != warmedVersion, warmTask == nil else { return }
        warmedVersion = t.v
        warmTask = Task(priority: .utility) { [weak self] in
            defer { Task { @MainActor [weak self] in self?.warmTask = nil } }
            guard let self else { return }
            let tz = t.tz
            let from = Formatting.day(Date().addingTimeInterval(-14 * 86400), tz: tz)
            let monthStart = String(t.day.prefix(7)) + "-01"
            _ = try? await self.dailyStats(from: monthStart, to: t.day)
            _ = try? await self.trades(.init())
            _ = try? await self.journal(.init())
            _ = try? await self.labels()
            _ = try? await self.strategies()
            _ = try? await self.accounts(showRetired: true)
            _ = try? await self.equity()
            _ = try? await self.nudges(day: "today")
            _ = try? await self.digest()
            guard !Task.isCancelled else { return }
            if let recent = try? await self.journal(.init(from: from, to: t.day)) {
                for row in recent.value.prefix(30) where !Task.isCancelled {
                    _ = try? await self.entry(row.id)
                }
            }
            if let recent = try? await self.trades(.init(from: from, to: t.day)) {
                for row in recent.value.items.prefix(30) where !Task.isCancelled {
                    _ = try? await self.trade(row.id)
                }
            }
        }
    }

    /// The last ten trading days before today, for the widgets; at most every quarter hour.
    private func refreshRecentDays(_ t: Today) async {
        if let at = recentDaysAt, Date.now.timeIntervalSince(at) < 15 * 60 { return }
        guard let api else { return }
        let from = Formatting.day(Date().addingTimeInterval(-16 * 86400), tz: t.tz)
        if let s = try? await api.get(DailyStats.self, "/stats/daily", query: [URLQueryItem(name: "from", value: from), URLQueryItem(name: "to", value: "yesterday")], session: session) {
            recentDays = s.items.suffix(10).map { TodayCache.DayCell(date: $0.date, net: $0.net, trades: $0.trades) }
            recentDaysAt = .now
        }
    }

    /// The server's facts and the user's settings; also refreshes what Settings caches.
    func me() async throws -> Me {
        guard let api else { throw TJError.notConfigured }
        let me = try await api.get(Me.self, "/me", session: session)
        settings.server = me.server
        settings.user = me.user
        settings.aiReady = me.ai?.ready ?? false
        return me
    }

    /// Whether this user can run AI work, so the AI buttons show; the web hides them the same way.
    var aiReady: Bool { settings.aiReady }

    // MARK: AI from the phone
    //
    // Every call runs on the server with the user's own provider; the phone starts work and
    // reads the result where it lands (the note, the position, the plan), refetched when the
    // server's version moves.

    enum AIError: Error {
        /// No provider for this user: Settings > AI in the web app.
        case off
        /// A review of this note runs already.
        case reviewPending(entryId: Int?)
        /// The day run is going.
        case dayRun
    }

    private func aiCall<T>(_ work: () async throws -> T) async throws -> T {
        do {
            return try await work()
        } catch TJError.conflict(let reason, let message, let body) {
            switch reason {
            case "ai-off": throw AIError.off
            case "review-pending": throw AIError.reviewPending(entryId: (try? Wire.decoder.decode(JournalConflict.self, from: body))?.entryId)
            case "day-run": throw AIError.dayRun
            default: throw TJError.conflict(reason: reason, message: message, body: body)
            }
        } catch TJError.unauthorized {
            connection = .unauthorized
            throw TJError.unauthorized
        }
    }

    /// Reviews a note in the background; the review lands on the note. Returns the note's id,
    /// the note created first for a trade, set, day or week without one.
    func startReview(_ target: ReviewTarget) async throws -> Int {
        guard let api else { throw TJError.notConfigured }
        return try await aiCall { try await api.send("POST", "/ai/review", json: target, as: ReviewStarted.self, session: session) }.entryId
    }

    func dayRunStatus() async throws -> DayRunStatus {
        guard let api else { throw TJError.notConfigured }
        return try await api.get(DayRunResponse.self, "/ai/day", session: session).status
    }

    /// Starts the day run: every note of the day reviewed, the day's synthesis written.
    func startDayRun(_ day: String, all: Bool = false) async throws -> DayRunStatus {
        guard let api else { throw TJError.notConfigured }
        return try await aiCall { try await api.send("POST", "/ai/day/\(day)", json: ["mode": all ? "all" : ""], as: DayRunResponse.self, session: session) }.status
    }

    func cancelDayRun() async throws -> DayRunStatus {
        guard let api else { throw TJError.notConfigured }
        let data = try await aiCall { try await api.send("DELETE", "/ai/day", session: session) }
        return try Wire.decoder.decode(DayRunResponse.self, from: data).status
    }

    /// Asks for a stop and target on the open position; the position's ai shows the result.
    func requestLevels(account: Int, symbol: String) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await aiCall { try await api.send("POST", "/positions/\(account)/\(symbol)/levels", session: session) }
    }

    /// Writes the proposal into the plan; the server answers with the position.
    func applyLevels(account: Int, symbol: String) async throws -> Position {
        guard let api else { throw TJError.notConfigured }
        let data = try await aiCall { try await api.send("POST", "/positions/\(account)/\(symbol)/levels/apply", session: session) }
        return try Wire.decoder.decode(Position.self, from: data)
    }

    func dismissLevels(account: Int, symbol: String) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await aiCall { try await api.send("DELETE", "/positions/\(account)/\(symbol)/levels", session: session) }
    }

    /// Drafts the day's plan; Today's plan.draft shows it to take over or discard.
    func draftPlan(_ day: String) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await aiCall { try await api.send("POST", "/plan/\(day)/draft", session: session) }
    }

    func discardDraft(_ day: String) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await aiCall { try await api.send("DELETE", "/plan/\(day)/draft", session: session) }
    }

    // MARK: reads for the other screens

    struct TradeFilter: Hashable, Sendable {
        var symbolRoot = ""
        var direction: Direction?
        var outcome = "" // win | loss | flat | open
        var from = ""
        var to = ""
        var sort = ""
        var dir = ""
        var accountId: Int?
        var page = 1

        var query: [URLQueryItem] {
            var q: [URLQueryItem] = [URLQueryItem(name: "page", value: String(page))]
            if let accountId { q.append(URLQueryItem(name: "account_id", value: String(accountId))) }
            if !symbolRoot.isEmpty { q.append(URLQueryItem(name: "symbol_root", value: symbolRoot)) }
            if let direction, direction != .unknown { q.append(URLQueryItem(name: "direction", value: direction.rawValue)) }
            if !outcome.isEmpty { q.append(URLQueryItem(name: "outcome", value: outcome)) }
            if !from.isEmpty { q.append(URLQueryItem(name: "from", value: from)) }
            if !to.isEmpty { q.append(URLQueryItem(name: "to", value: to)) }
            if !sort.isEmpty { q.append(URLQueryItem(name: "sort", value: sort)) }
            if !dir.isEmpty { q.append(URLQueryItem(name: "dir", value: dir)) }
            return q
        }
    }

    func trades(_ f: TradeFilter, fresh: Bool = false) async throws -> Loaded<TradeList> {
        try await get(TradeList.self, "/trades", query: f.query, fresh: fresh)
    }

    func trade(_ id: Int, fresh: Bool = false) async throws -> Loaded<TradeDetail> {
        try await get(TradeDetail.self, "/trades/\(id)", fresh: fresh)
    }

    func positions() async throws -> Loaded<[Position]> {
        try await get(PositionList.self, "/positions").map(\.items)
    }

    struct JournalFilter: Hashable, Sendable {
        var scopes: [JournalScope] = []
        var tag: JournalTag?
        var from = ""
        var to = ""
        var limit = 50
        var offset = 0

        var query: [URLQueryItem] {
            var q = [URLQueryItem(name: "limit", value: String(limit)), URLQueryItem(name: "offset", value: String(offset))]
            for s in scopes { q.append(URLQueryItem(name: "scope", value: s.rawValue)) }
            if let tag, tag != .unknown { q.append(URLQueryItem(name: "tag", value: tag.rawValue)) }
            if !from.isEmpty { q.append(URLQueryItem(name: "from", value: from)) }
            if !to.isEmpty { q.append(URLQueryItem(name: "to", value: to)) }
            return q
        }
    }

    func journal(_ f: JournalFilter, fresh: Bool = false) async throws -> Loaded<[JournalRow]> {
        try await get(JournalList.self, "/journal", query: f.query, fresh: fresh).map(\.items)
    }

    func entry(_ id: Int, fresh: Bool = false) async throws -> Loaded<JournalDetail> {
        try await get(JournalDetail.self, "/journal/\(id)", fresh: fresh)
    }

    func labels() async throws -> Loaded<LabelVocabulary> {
        try await get(LabelVocabulary.self, "/journal/labels")
    }

    func strategies(fresh: Bool = false) async throws -> Loaded<[Strategy]> {
        try await get(StrategyList.self, "/strategies", fresh: fresh).map(\.items)
    }

    func strategy(_ id: Int, fresh: Bool = false) async throws -> Loaded<Strategy> {
        try await get(Strategy.self, "/strategies/\(id)", fresh: fresh)
    }

    /// The analytics summary over a range of trading days (YYYY-MM-DD, inclusive).
    func analytics(from: String, to: String, fresh: Bool = false) async throws -> Loaded<AnalyticsSummary> {
        try await get(AnalyticsSummary.self, "/analytics/summary", query: [URLQueryItem(name: "from", value: from), URLQueryItem(name: "to", value: to)], fresh: fresh)
    }

    /// The last 90 trading-timezone days ending today, the window the Library's numbers use.
    var libraryRange: (from: String, to: String) {
        let today = self.today?.day ?? Formatting.day(.now, tz: tz)
        let from = Formatting.day(Date.now.addingTimeInterval(-90 * 86400), tz: tz)
        return (from, today)
    }

    /// The weekly edge digest; nil week: the week of the last trading day.
    func digest(week: String? = nil, fresh: Bool = false) async throws -> Loaded<Digest> {
        try await get(Digest.self, "/digest", query: week.map { [URLQueryItem(name: "week", value: $0)] } ?? [], fresh: fresh)
    }

    func dailyStats(from: String, to: String, fresh: Bool = false) async throws -> Loaded<DailyStats> {
        try await get(DailyStats.self, "/stats/daily", query: [URLQueryItem(name: "from", value: from), URLQueryItem(name: "to", value: to)], fresh: fresh)
    }

    func nudges(day: String, fresh: Bool = false) async throws -> Loaded<NudgeList> {
        try await get(NudgeList.self, "/nudges", query: [URLQueryItem(name: "day", value: day)], fresh: fresh)
    }

    func accounts(showRetired: Bool = false, fresh: Bool = false) async throws -> Loaded<[Account]> {
        try await get(AccountList.self, "/accounts", query: showRetired ? [URLQueryItem(name: "show_retired", value: "1")] : [], fresh: fresh).map(\.items)
    }

    func equity(filter: String = "", fresh: Bool = false) async throws -> Loaded<Equity> {
        try await get(Equity.self, "/equity", query: filter.isEmpty ? [] : [URLQueryItem(name: "filter", value: filter)], fresh: fresh)
    }

    /// An image the API serves (a chart PNG, an attachment), by its server-relative href. Kept
    /// on the phone: a kept image younger than a few hours (or any kept one while the journal
    /// is out of reach) is used as it is; a chart of an open trade is drawn again after that.
    func image(_ href: String) async throws -> Data {
        guard let api else { throw TJError.notConfigured }
        let st = store().images
        let kept = await st.get(href)
        if let (data, at) = kept, connection.isOffline || Date.now.timeIntervalSince(at) < 6 * 3600 {
            return data
        }
        do {
            let data = try await api.image(href, session: session)
            await st.put(href, data)
            return data
        } catch let error where Self.isReachability(error) {
            lostReach()
            if let (data, _) = kept { return data }
            throw error
        }
    }

    /// GET through the store. The live answer is kept and returned; when the journal cannot be
    /// reached, the kept one comes back with its time. While the stream is down the kept one
    /// comes first, without a wait on the network, unless `fresh` (a pull to refresh) insists.
    private func get<T: Decodable & Sendable>(_ type: T.Type, _ path: String, query: [URLQueryItem] = [], fresh: Bool = false) async throws -> Loaded<T> {
        guard let api else { throw TJError.notConfigured }
        let key = Store.key(path, query: query)
        let st = store().json
        if !fresh, connection.isOffline, let kept = await stored(T.self, key, in: st) {
            return kept
        }
        do {
            let data = try await api.send("GET", path, query: query, session: session)
            let v: T
            do {
                v = try Wire.decoder.decode(T.self, from: data)
            } catch {
                throw TJError.decoding(String(describing: error).prefix(200).description)
            }
            await st.put(key, data)
            return Loaded(value: v)
        } catch TJError.unauthorized {
            connection = .unauthorized
            throw TJError.unauthorized
        } catch let error where Self.isReachability(error) {
            lostReach()
            if let kept = await stored(T.self, key, in: st) { return kept }
            throw error
        }
    }

    private func stored<T: Decodable & Sendable>(_ type: T.Type, _ key: String, in st: Store) async -> Loaded<T>? {
        guard let (data, at) = await st.get(key), let v = try? Wire.decoder.decode(T.self, from: data) else { return nil }
        return Loaded(value: v, asOf: at)
    }

    // MARK: actions

    /// What a journal save can run into, for the editor to ask the trader about.
    enum JournalSaveError: Error {
        /// An AI review is writing the note: append instead, or wait.
        case reviewPending
        /// The entry changed since it was loaded; here is the current one.
        case stale(JournalDetail)
        /// A singleton entry already exists for this day, week or trade.
        case exists(id: Int)
    }

    private func journalWrite<T: Decodable & Sendable>(_ work: () async throws -> T) async throws -> T {
        do {
            return try await work()
        } catch TJError.conflict(let reason, _, let body) {
            let c = try? Wire.decoder.decode(JournalConflict.self, from: body)
            if reason == "review-pending" { throw JournalSaveError.reviewPending }
            if reason == "stale", let e = c?.entry { throw JournalSaveError.stale(e) }
            if let id = c?.entryId { throw JournalSaveError.exists(id: id) }
            throw TJError.conflict(reason: reason, message: c?.error ?? "", body: body)
        } catch TJError.unauthorized {
            connection = .unauthorized
            throw TJError.unauthorized
        }
    }

    /// Replaces an entry the way a page save does; the result is the entry after the save.
    func saveJournal(_ id: Int, _ u: JournalUpdate) async throws -> JournalDetail {
        guard let api else { throw TJError.notConfigured }
        return try await journalWrite { try await api.send("PUT", "/journal/\(id)", json: u, as: JournalDetail.self, session: session) }
    }

    func deleteJournal(_ id: Int) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await journalWrite { try await api.send("DELETE", "/journal/\(id)", session: session) }
    }

    /// Creates an entry; a singleton that exists already comes back as JournalSaveError.exists.
    func createJournal(_ c: JournalCreate) async throws -> JournalDetail {
        guard let api else { throw TJError.notConfigured }
        return try await journalWrite { try await api.send("POST", "/journal", json: c, as: JournalDetail.self, session: session) }
    }

    func appendNote(_ id: Int, _ note: String) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await journalWrite { try await api.send("POST", "/journal/\(id)/notes", json: ["note": note], as: NoteAppended.self, session: session) }
    }

    /// Stores an image and returns the markdown line to put in a note.
    func upload(image: Data, name: String, mimeType: String) async throws -> AttachmentUpload {
        guard let api else { throw TJError.notConfigured }
        return try await journalWrite { try await api.upload("/journal/attachments", file: image, name: name, mimeType: mimeType, as: AttachmentUpload.self, session: session) }
    }

    /// Accepts or dismisses one of the AI review's suggestions; the result is the entry after.
    func suggestion(_ entryId: Int, _ s: Suggestion, accept: Bool) async throws -> JournalDetail {
        guard let api else { throw TJError.notConfigured }
        return try await journalWrite { try await api.send("POST", "/journal/\(entryId)/suggestions", json: SuggestionAction(kind: s.kind, value: s.value, do: accept ? "accept" : "dismiss"), as: JournalDetail.self, session: session) }
    }

    /// Writes or amends a day's plan; nil when it ended up empty.
    func savePlan(_ p: DayPlanSave) async throws -> DayPlan? {
        guard let api else { throw TJError.notConfigured }
        let saved = try await journalWrite { try await api.send("POST", "/plan", json: p, as: DayPlanSaved.self, session: session) }
        await refreshToday()
        return saved.plan
    }

    /// Records the plan on an open position; the server answers with the position.
    func savePositionPlan(account: Int, symbol: String, _ p: PositionPlanSave) async throws -> Position {
        guard let api else { throw TJError.notConfigured }
        let saved = try await journalWrite { try await api.send("POST", "/positions/\(account)/\(symbol)/plan", json: p, as: PositionSaved.self, session: session) }
        await refreshToday()
        return saved.position
    }

    func updateSettings(_ u: SettingsUpdate) async throws -> UserSettings {
        guard let api else { throw TJError.notConfigured }
        return try await journalWrite { try await api.send("PUT", "/settings", json: u, as: SettingsSaved.self, session: session) }.settings
    }

    func addBalance(account: Int, _ b: BalanceAnchorAdd) async throws -> Account {
        guard let api else { throw TJError.notConfigured }
        return try await journalWrite { try await api.send("POST", "/accounts/\(account)/balances", json: b, as: AccountSaved.self, session: session) }.account
    }

    func deleteBalance(account: Int, anchor: Int) async throws {
        guard let api else { throw TJError.notConfigured }
        _ = try await journalWrite { try await api.send("DELETE", "/accounts/\(account)/balances/\(anchor)", session: session) }
    }

    // MARK: devices

    /// Hands the server this phone's APNs token with its settings; the token the app registered
    /// before (an Xcode build TestFlight replaced) is forgotten on the server.
    func registerDevice(apnsToken: String) async throws -> Bool {
        guard let api else { throw TJError.notConfigured }
        let sandbox: Bool
        #if DEBUG
        sandbox = true
        #else
        sandbox = false
        #endif
        let reg = DeviceRegistration(token: apnsToken, name: UIDevice.current.name, sandbox: sandbox,
                                     bundleId: Bundle.main.bundleIdentifier ?? "nl.w4j.tradingjournal", prefs: settings.notificationPrefs)
        let r = try await api.send("POST", "/devices", json: reg, as: DeviceRegistered.self, session: session)
        let before = settings.registeredToken
        settings.registered = "\(settings.serverURL)|\(apnsToken)"
        if let before, before != apnsToken {
            _ = try? await api.send("DELETE", "/devices/\(before)", session: session)
        }
        return r.push
    }

    /// Sends the settings whole; before the phone registered there is nothing to update.
    func putPrefs() async throws {
        guard let api, let token = settings.registeredToken else { return }
        do {
            _ = try await api.send("PUT", "/devices/\(token)/prefs", json: settings.notificationPrefs, as: DeviceSaved.self, session: session)
        } catch TJError.http(404, _) { // the server forgot this phone: register again, with them
            settings.registered = nil
            throw TJError.http(404, "the journal forgot this phone; it registers again at the next start")
        }
    }

    /// Hands the server this phone's Live Activity tokens, whole.
    func reportActivities(start: String?, activities: [DeviceActivity]) async {
        guard let api, let token = settings.registeredToken else { return }
        _ = try? await api.send("PUT", "/devices/\(token)/activity", json: ActivityReport(startToken: start, activities: activities), as: DeviceSaved.self, session: session)
    }

    /// Forgets this phone on the server (sign out).
    func deleteDevice() async {
        guard let api, let token = settings.registeredToken else { return }
        _ = try? await api.send("DELETE", "/devices/\(token)", session: session)
        settings.registered = nil
    }

    /// Marks several nudges seen in one call.
    func markSeen(_ ids: [Int]) async throws {
        guard let api, !ids.isEmpty else { return }
        try await api.send("POST", "/nudges/seen", body: Wire.encoder.encode(NudgeIDs(ids: ids)), session: session)
        if var t = today {
            for i in t.nudges.items.indices where ids.contains(t.nudges.items[i].id) && t.nudges.items[i].seenAt == nil {
                t.nudges.items[i].seenAt = .now
                t.nudges.unseen = max(0, t.nudges.unseen - 1)
            }
            today = t
        }
    }

    /// Marks a nudge seen; Today follows over the stream.
    func markSeen(_ id: Int) async throws {
        guard let api else { throw TJError.notConfigured }
        try await api.send("POST", "/nudges/\(id)/seen", session: session)
        if var t = today, let i = t.nudges.items.firstIndex(where: { $0.id == id }), t.nudges.items[i].seenAt == nil {
            t.nudges.items[i].seenAt = .now
            t.nudges.unseen = max(0, t.nudges.unseen - 1)
            today = t
        }
    }

    /// Exchanges a pairing code for this phone's key and starts the client on it.
    func pair(serverURL: String, code: String) async throws {
        let name = UIDevice.current.name
        let r = try await TJAPI.pair(serverURL: serverURL, code: code, deviceName: name, session: session)
        settings.serverURL = serverURL
        settings.apiKey = r.key
        settings.server = r.server
        settings.user = r.user
        reset()
    }

    func signOut() {
        Task { await deleteDevice() }
        if let stores {
            Task { await stores.json.wipe(); await stores.images.wipe() }
        }
        settings.signOut()
        reset()
    }
}

/// The server's event stream, line by line.
enum SSE {
    enum Event: Equatable, Sendable {
        case version(Int)
    }

    /// One complete event: "version" with {"v": N}; anything else is ignored.
    nonisolated static func parse(event: String, data: String) -> Event? {
        guard event == "version" || event.isEmpty, let d = data.data(using: .utf8), let v = try? JSONDecoder().decode(DataVersion.self, from: d) else { return nil }
        return .version(v.v)
    }
}

/// The app icon's badge, set from what the server counts as unseen.
enum UNUserNotificationCenterBadge {
    static func set(_ n: Int) async throws {
        try await UNUserNotificationCenter.current().setBadgeCount(n)
    }
}

import UserNotifications
