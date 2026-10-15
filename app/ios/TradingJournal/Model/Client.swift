// The journal client: the Today state kept live over the server's event stream (SSE on
// /api/v1/events, a version per change), one-off reads for the other screens, and every action
// as a plain HTTP call. Everything runs on the main actor; the stream's awaits suspend, nothing
// blocks. Connection state is part of the model and the UI says it honestly.
import Foundation
import Observation
import OSLog
import UIKit

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
    private(set) var lastError: String?
    /// Set by a notification tap, a deep link or a launch argument: RootView navigates there and clears it.
    var pendingRoute: Route?

    private let session: URLSession
    private let streamSession: URLSession
    private var generation = 0
    private var attempt = 0
    private var streamTask: Task<Void, Never>?
    private var refetch: Task<Void, Never>?

    init(settings: Settings) {
        self.settings = settings
        let cfg = URLSessionConfiguration.default
        cfg.waitsForConnectivity = true
        cfg.timeoutIntervalForRequest = 20
        session = URLSession(configuration: cfg)
        let scfg = URLSessionConfiguration.default
        scfg.timeoutIntervalForRequest = 90 // the server pings every 25 s; silence longer than this is a dead stream
        scfg.timeoutIntervalForResource = .infinity
        streamSession = URLSession(configuration: scfg)
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
    }

    /// After pairing or signing out.
    func reset() {
        stop()
        today = nil
        asOf = nil
        version = 0
        lastError = nil
        start()
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
        guard let api else { return }
        do {
            let t = try await api.get(Today.self, "/today", query: refresh ? [URLQueryItem(name: "refresh", value: "1")] : [], session: session)
            asOf = .now
            lastError = nil
            log.debug("today v=\(t.v) trades=\(t.stats.trades) open=\(t.positions.count) unseen=\(t.nudges.unseen)")
            if t != today {
                today = t
            }
            if t.v > version { version = t.v }
            if settings.server?.timezone != t.timezone, var s = settings.server {
                s.timezone = t.timezone
                settings.server = s
            }
            try? await UNUserNotificationCenterBadge.set(t.nudges.badge)
        } catch TJError.unauthorized {
            connection = .unauthorized
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// The server's facts and the user's settings; also refreshes what Settings caches.
    func me() async throws -> Me {
        guard let api else { throw TJError.notConfigured }
        let me = try await api.get(Me.self, "/me", session: session)
        settings.server = me.server
        settings.user = me.user
        return me
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
        var page = 1

        var query: [URLQueryItem] {
            var q: [URLQueryItem] = [URLQueryItem(name: "page", value: String(page))]
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

    func trades(_ f: TradeFilter) async throws -> TradeList {
        try await get(TradeList.self, "/trades", query: f.query)
    }

    func trade(_ id: Int) async throws -> TradeDetail {
        try await get(TradeDetail.self, "/trades/\(id)")
    }

    func positions() async throws -> [Position] {
        try await get(PositionList.self, "/positions").items
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

    func journal(_ f: JournalFilter) async throws -> [JournalRow] {
        try await get(JournalList.self, "/journal", query: f.query).items
    }

    func entry(_ id: Int) async throws -> JournalDetail {
        try await get(JournalDetail.self, "/journal/\(id)")
    }

    func labels() async throws -> LabelVocabulary {
        try await get(LabelVocabulary.self, "/journal/labels")
    }

    func strategies() async throws -> [Strategy] {
        try await get(StrategyList.self, "/strategies").items
    }

    func dailyStats(from: String, to: String) async throws -> DailyStats {
        try await get(DailyStats.self, "/stats/daily", query: [URLQueryItem(name: "from", value: from), URLQueryItem(name: "to", value: to)])
    }

    func nudges(day: String) async throws -> NudgeList {
        try await get(NudgeList.self, "/nudges", query: [URLQueryItem(name: "day", value: day)])
    }

    func accounts(showRetired: Bool = false) async throws -> [Account] {
        try await get(AccountList.self, "/accounts", query: showRetired ? [URLQueryItem(name: "show_retired", value: "1")] : []).items
    }

    func equity(filter: String = "") async throws -> Equity {
        try await get(Equity.self, "/equity", query: filter.isEmpty ? [] : [URLQueryItem(name: "filter", value: filter)])
    }

    /// An image the API serves (a chart PNG, an attachment), by its server-relative href.
    func image(_ href: String) async throws -> Data {
        guard let api else { throw TJError.notConfigured }
        return try await api.image(href, session: session)
    }

    private func get<T: Decodable & Sendable>(_ type: T.Type, _ path: String, query: [URLQueryItem] = []) async throws -> T {
        guard let api else { throw TJError.notConfigured }
        do {
            return try await api.get(T.self, path, query: query, session: session)
        } catch TJError.unauthorized {
            connection = .unauthorized
            throw TJError.unauthorized
        }
    }

    // MARK: actions

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
