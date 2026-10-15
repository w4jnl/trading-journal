// The pure helpers: formatting, routes, markdown, URLs and the settings' defaults.
import Foundation
import Testing
@testable import TradingJournal

struct FormattingTests {
    @Test func moneyReadsLikeTheWebApp() {
        #expect(Formatting.money(1327.4399) == "+1327.44")
        #expect(Formatting.money(-33.3) == "-33.30")
        #expect(Formatting.money(0) == "+0.00", "zero is a gain, never -0.00")
        #expect(Formatting.money(-0.0) == "+0.00")
        #expect(Formatting.money(nil) == "—")
        #expect(Formatting.moneyShort(-1543.75) == "-1544")
        #expect(Formatting.balance(171487.57) == "171,487.57" || Formatting.balance(171487.57).hasSuffix("487,57") || Formatting.balance(171487.57).hasSuffix("487.57"))
    }

    @Test func pricesSnapToTheTick() {
        #expect(Formatting.price(7766.5, tickSize: 0.25) == "7766.50")
        #expect(Formatting.price(7766.333, tickSize: 0.25) == "7766.33")
        #expect(Formatting.price(21500, tickSize: 1) == "21500")
        #expect(Formatting.price(63.42, tickSize: 0.01) == "63.42")
        #expect(Formatting.price(nil) == "—")
        #expect(Formatting.decimals(0.25) == 2 && Formatting.decimals(0.1) == 1 && Formatting.decimals(0.005) == 3 && Formatting.decimals(1) == 0)
    }

    @Test func smallFigures() {
        #expect(Formatting.r(1.84) == "1.8R" && Formatting.r(nil) == "—")
        #expect(Formatting.ticks(76) == "76 ticks" && Formatting.ticks(1) == "1 tick" && Formatting.ticks(-3) == "-3 ticks")
        #expect(Formatting.qty(3) == "3" && Formatting.qty(0.5) == "0.50")
        #expect(Formatting.percent(0.9173) == "92%" && Formatting.percent(nil) == "—")
    }

    @Test func riskMirrorsTheServer() {
        // MES: tick 0.25 worth 1.25, so 5 a point; 9 points on 3 contracts = 135
        #expect(Formatting.risk(avgEntry: 7766.25, stop: 7757.25, qty: 3, pointValue: 5) == 135)
        #expect(abs(Formatting.risk(avgEntry: 7766.333333, stop: 7757.25, qty: 3, pointValue: 5) - 136.25) < 0.001, "the fixture's position: not snapped")
    }

    @Test func clocksAreInTheTradingTimezone() {
        let tz = TimeZone(identifier: "Europe/Amsterdam")!
        let d = Wire.parseDate("2026-10-14T19:57:43Z")!
        #expect(Formatting.clock(d, tz: tz) == "21:57")
        #expect(Formatting.day(d, tz: tz) == "2026-10-14")
        #expect(Formatting.day(Wire.parseDate("2026-10-14T22:30:00Z")!, tz: tz) == "2026-10-15", "after midnight in Amsterdam")
        #expect(Formatting.parseDay("2026-10-14", tz: tz) != nil && Formatting.parseDay("today", tz: tz) == nil)
        #expect(Formatting.dayLabel("nonsense", tz: tz) == "nonsense")
        #expect(Formatting.age(Date(timeIntervalSinceNow: -125)) == "2 min")
    }
}

struct RouteTests {
    @Test func parsesTheScheme() {
        #expect(Route.parse(URL(string: "tj://open/today")!) == .today)
        #expect(Route.parse(URL(string: "tj://open/day/2026-10-14")!) == .day("2026-10-14"))
        #expect(Route.parse(URL(string: "tj://open/trade/5")!) == .trade(5))
        #expect(Route.parse(URL(string: "tj://open/position/6/MESZ6")!) == .position(account: 6, symbol: "MESZ6"))
        #expect(Route.parse(URL(string: "tj://open/journal/328")!) == .journal(328))
        #expect(Route.parse(URL(string: "tj://open/nudges")!) == .nudges)
        #expect(Route.parse(URL(string: "tj://open/trade/x")!) == nil)
        #expect(Route.parse(URL(string: "https://example.net/open/today")!) == nil)
        #expect(Route.parse(URL(string: "tj://pair?server=https%3A%2F%2Fj.example.net&code=AB7Q-K2MN")!) == .pair(server: "https://j.example.net", code: "AB7Q-K2MN"))
        #expect(Route.parse(URL(string: "tj://pair")!) == .pair(server: nil, code: nil))
    }

    @Test func roundTripsThroughItsURL() {
        for r: Route in [.today, .day("2026-10-14"), .trade(5), .position(account: 6, symbol: "MESZ6"), .journal(3), .nudges, .plan, .accounts, .settings, .pair(server: "https://j.example.net", code: "AB")] {
            #expect(Route.parse(r.url) == r, "\(r)")
        }
    }

    @Test func readsTheServersRoutesAndHrefs() {
        #expect(Route.from(route: "position/6/MESZ6") == .position(account: 6, symbol: "MESZ6"))
        #expect(Route.from(route: "day/2026-10-14") == .day("2026-10-14"))
        #expect(Route.from(route: "") == nil)
        #expect(Route.from(href: "/journal/2026-10-14") == .day("2026-10-14"))
        #expect(Route.from(href: "/trades?sheet=pos%3A6%3AMESZ6&status=open") == .position(account: 6, symbol: "MESZ6"))
        #expect(Route.from(href: "/trades?status=open&sheet=posset:6:MESZ6") == .position(account: 6, symbol: "MESZ6"))
        #expect(Route.from(href: "/trades?sheet=123") == .trade(123))
        #expect(Route.from(href: "/journal/328") == .journal(328))
        #expect(Route.from(href: "/dashboard") == .today)
    }

    @Test func tabsFollowTheRoute() {
        #expect(Route.trade(1).tab == .trades && Route.day("x").tab == .calendar && Route.position(account: 1, symbol: "ES").tab == .today && Route.journal(1).tab == .journal)
    }
}

struct MarkdownTests {
    @Test func splitsANoteIntoBlocks() {
        let src = """
        ### AI review — 2026-10-14
        *claude via Anthropic*

        - **Setup & entry:** a range-top fade.
          Patient, not chased.
        - Risk: none recorded.

        ---

        _Note added 2026-10-14 21:16 CEST_

        ![](/api/v1/attachments/3)

        ```
        code here
        ```
        > a quote
        1. first
        - [ ] todo
        """
        let b = Markdown.blocks(src)
        #expect(b[0] == .heading(level: 3, text: "AI review — 2026-10-14"))
        #expect(b[1] == .paragraph("*claude via Anthropic*"))
        #expect(b[2] == .listItem(text: "**Setup & entry:** a range-top fade. Patient, not chased.", ordered: false, indent: 0), "a wrapped item is one item")
        #expect(b[3] == .listItem(text: "Risk: none recorded.", ordered: false, indent: 0))
        #expect(b[4] == .rule)
        #expect(b[5] == .noteMarker("Note added 2026-10-14 21:16 CEST"))
        #expect(b[6] == .image(alt: "", href: "/api/v1/attachments/3"))
        #expect(b[7] == .code("code here"))
        #expect(b[8] == .quote("a quote"))
        #expect(b[9] == .listItem(text: "first", ordered: true, indent: 0))
        #expect(b[10] == .listItem(text: "☐ todo", ordered: false, indent: 0))
        #expect(b.count == 11)
    }

    @Test func snippetsArePlain() {
        #expect(Markdown.snippet("### Title\n\n**bold** and [a link](http://x) and `code`") == "Title bold and a link and code")
        #expect(Markdown.snippet(String(repeating: "word ", count: 60)).hasSuffix("…"))
        #expect(Markdown.snippet("") == "")
        #expect(Markdown.snippet("| a | b |\n|---|---|\n| 1 | 2 |\n\nAfter the table") == "After the table", "table rows are not prose")
    }
}

struct URLTests {
    @Test func normalisesWhatTheUserTypes() {
        #expect(ServerURL.base("journal.example.net")?.absoluteString == "https://journal.example.net")
        #expect(ServerURL.base("https://journal.example.net/")?.absoluteString == "https://journal.example.net")
        #expect(ServerURL.base(" http://192.168.1.10:18777/settings ")?.absoluteString == "http://192.168.1.10:18777")
        #expect(ServerURL.base("") == nil && ServerURL.base("https://") == nil)
        let api = TJAPI(serverURL: "https://j.example.net", key: "tj_x")!
        #expect(api.url("/today").absoluteString == "https://j.example.net/api/v1/today")
        #expect(api.url("/api/v1/attachments/3").absoluteString == "https://j.example.net/api/v1/attachments/3", "a server href is used as is")
        #expect(api.url("/trades", query: [URLQueryItem(name: "page", value: "2")]).absoluteString == "https://j.example.net/api/v1/trades?page=2")
        #expect(TJAPI(serverURL: "https://j.example.net", key: "") == nil)
    }

    @Test func errorsSpeakForThemselves() {
        #expect(TJError.http(404, "").isMissingRoute && !TJError.http(404, "no such trade").isMissingRoute)
        #expect(TJError.http(409, "review pending").errorDescription == "review pending")
        #expect(TJError.unauthorized.errorDescription?.contains("pair again") == true)
    }
}

/// What the app remembers between starts: UserDefaults, read back at init.
struct SettingsTests {
    @MainActor @Test func theActivityIsOnByDefaultAndAChoiceIsKept() {
        let d = UserDefaults.standard
        let before = d.object(forKey: "showsActivity")
        defer { if let before { d.set(before, forKey: "showsActivity") } else { d.removeObject(forKey: "showsActivity") } }
        d.removeObject(forKey: "showsActivity")
        #expect(Settings().showsActivity, "a fresh install shows the Session activity")
        let s = Settings()
        s.showsActivity = false
        #expect(!Settings().showsActivity, "switched off, it stays off at the next start")
        s.showsActivity = true
    }

    @MainActor @Test func pairedMeansAServerAndAKey() {
        let s = Settings()
        let url = s.serverURL, key = s.apiKey
        defer { s.serverURL = url; s.apiKey = key }
        s.serverURL = "https://j.example.net"
        s.apiKey = ""
        #expect(!s.isPaired)
        s.apiKey = "tj_abc"
        #expect(s.isPaired && s.api?.base.host() == "j.example.net")
        s.signOut()
        #expect(!s.isPaired && s.serverURL == "https://j.example.net", "sign out keeps the address for pairing again")
    }
}
