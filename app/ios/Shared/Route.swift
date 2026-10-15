// One route format for everything that opens a screen: the tj:// scheme (a notification tap,
// the Live Activity's widgetURL, a widget Link, the -open launch argument), the server's own
// `route` strings on nudges, and the web hrefs older servers send. Pure, tested.
import Foundation

enum Route: Hashable, Sendable {
    case today
    case day(String)
    case trade(Int)
    case position(account: Int, symbol: String)
    case journal(Int)
    case nudges
    case plan
    case accounts
    case settings
    case digest(String?) // an ISO week, nil for the last one
    case pair(server: String?, code: String?)

    /// The tab a route lives in.
    var tab: Tab {
        switch self {
        case .today, .position, .nudges, .plan, .accounts: .today
        case .trade: .trades
        case .journal, .digest: .journal
        case .day: .calendar
        case .settings, .pair: .settings
        }
    }

    enum Tab: String, Hashable, CaseIterable, Sendable {
        case today, trades, journal, calendar, settings
        var title: String {
            switch self {
            case .today: "Today"
            case .trades: "Trades"
            case .journal: "Journal"
            case .calendar: "Calendar"
            case .settings: "Settings"
            }
        }
        var symbol: String {
            switch self {
            case .today: "sun.max"
            case .trades: "list.bullet.rectangle"
            case .journal: "book"
            case .calendar: "calendar"
            case .settings: "gearshape"
            }
        }
    }

    /// The route's tj:// URL.
    var url: URL {
        var s = "tj://open/"
        switch self {
        case .today: s += "today"
        case .day(let d): s += "day/\(d)"
        case .trade(let id): s += "trade/\(id)"
        case .position(let a, let sym): s += "position/\(a)/\(sym)"
        case .journal(let id): s += "journal/\(id)"
        case .nudges: s += "nudges"
        case .plan: s += "plan"
        case .accounts: s += "accounts"
        case .settings: s += "settings"
        case .digest(let w): s += w.map { "digest/\($0)" } ?? "digest"
        case .pair(let server, let code):
            var c = URLComponents(string: "tj://pair")!
            c.queryItems = [URLQueryItem(name: "server", value: server ?? ""), URLQueryItem(name: "code", value: code ?? "")]
            return c.url!
        }
        return URL(string: s)!
    }

    /// tj://open/<kind>/<id…> and tj://pair?server=&code=; nil for anything else.
    nonisolated static func parse(_ url: URL) -> Route? {
        guard url.scheme?.lowercased() == "tj" else { return nil }
        switch url.host?.lowercased() {
        case "pair":
            let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems ?? []
            let server = items.first { $0.name == "server" }?.value
            let code = items.first { $0.name == "code" }?.value
            return .pair(server: server?.isEmpty == false ? server : nil, code: code?.isEmpty == false ? code : nil)
        case "open":
            let parts = url.pathComponents.filter { $0 != "/" }
            return from(parts: parts)
        default:
            return nil
        }
    }

    /// The server's route string on a nudge: "day/2026-10-14", "position/6/MESZ6", "trade/5", "today".
    nonisolated static func from(route: String) -> Route? {
        let parts = route.split(separator: "/").map(String.init)
        return from(parts: parts)
    }

    nonisolated private static func from(parts: [String]) -> Route? {
        guard let kind = parts.first?.lowercased() else { return nil }
        let rest = Array(parts.dropFirst())
        switch kind {
        case "today": return .today
        case "day": return rest.first.map { .day($0) }
        case "trade": return rest.first.flatMap(Int.init).map { .trade($0) }
        case "position":
            guard rest.count >= 2, let acc = Int(rest[0]) else { return nil }
            return .position(account: acc, symbol: rest[1])
        case "journal": return rest.first.flatMap(Int.init).map { .journal($0) }
        case "nudges": return .nudges
        case "plan": return .plan
        case "accounts": return .accounts
        case "settings": return .settings
        case "digest": return .digest(rest.first)
        default: return nil
        }
    }

    /// A web href the server sends on a nudge, for servers without `route`:
    /// "/journal/2026-10-14" → day; "/trades?status=open&sheet=pos:6:MESZ6" → position;
    /// "/trades?sheet=123" → trade; anything else → today.
    nonisolated static func from(href: String) -> Route {
        guard let c = URLComponents(string: href) else { return .today }
        let path = c.path.split(separator: "/").map(String.init)
        let sheet = c.queryItems?.first { $0.name == "sheet" }?.value
        if path.first == "journal", path.count >= 2, path[1].count == 10, path[1].split(separator: "-").count == 3 {
            return .day(path[1])
        }
        if let sheet {
            if sheet.hasPrefix("pos:") || sheet.hasPrefix("posset:") {
                let bits = sheet.split(separator: ":").map(String.init)
                if bits.count >= 3, let acc = Int(bits[1]) { return .position(account: acc, symbol: bits[2]) }
            }
            if let id = Int(sheet) { return .trade(id) }
        }
        if path.first == "journal", path.count >= 2, let id = Int(path[1]) { return .journal(id) }
        return .today
    }

    /// The route a nudge opens: the server's route first, its href as a fallback.
    nonisolated static func of(_ n: Nudge) -> Route {
        from(route: n.route) ?? from(href: n.href)
    }
}
