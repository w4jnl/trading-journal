// The pure helpers: formatting, routes, markdown, URLs and the settings' defaults.
import Foundation
import SwiftUI
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
        #expect(Route.parse(URL(string: "tj://open/library")!) == .library)
        #expect(Route.parse(URL(string: "tj://open/strategy/9")!) == .strategy(9))
        #expect(Route.strategy(9).url.absoluteString == "tj://open/strategy/9" && Route.library.tab == .journal)
        #expect(Route.parse(URL(string: "tj://open/nudges")!) == .nudges)
        #expect(Route.parse(URL(string: "tj://open/trade/x")!) == nil)
        #expect(Route.parse(URL(string: "https://example.net/open/today")!) == nil)
        #expect(Route.parse(URL(string: "tj://pair?server=https%3A%2F%2Fj.example.net&code=AB7Q-K2MN")!) == .pair(server: "https://j.example.net", code: "AB7Q-K2MN"))
        #expect(Route.parse(URL(string: "tj://pair")!) == .pair(server: nil, code: nil))
    }

    @Test func roundTripsThroughItsURL() {
        for r: Route in [.today, .day("2026-10-14"), .trade(5), .position(account: 6, symbol: "MESZ6"), .journal(3), .nudges, .plan, .accounts, .settings, .digest(nil), .digest("2026-W41"), .pair(server: "https://j.example.net", code: "AB")] {
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

    /// The GEX annotation's shape: an italic title, a pipe table and a machine-readable HTML comment.
    static let gex = """
    _Gamma context (GEX) — futures prices_

    |  | entry 16:03 ET | exit 16:05 ET |
    |---|---|---|
    | regime | negative (P0) | negative (P0) |
    | flip 7796.02 | below (-399 t) | below (-381 t) @ 7792.82 |
    | put wall 7695.24 | +4 t | +11 t @ 7694.70 |

    <!-- tj-gex:v1 market=ES regime=negative
    em=64.35 stale=false -->
    | not a table
    after
    """

    @Test func tablesAndComments() {
        let b = Markdown.blocks(Self.gex)
        #expect(b[0] == .paragraph("_Gamma context (GEX) — futures prices_"))
        #expect(b[1] == .table(header: ["", "entry 16:03 ET", "exit 16:05 ET"],
                               rows: [["regime", "negative (P0)", "negative (P0)"], ["flip 7796.02", "below (-399 t)", "below (-381 t) @ 7792.82"], ["put wall 7695.24", "+4 t", "+11 t @ 7694.70"]]))
        #expect(b[2] == .paragraph("| not a table after"), "a pipe line without a delimiter row stays prose; the comment is gone")
        #expect(b.count == 3)
        #expect(Markdown.snippet(Self.gex) == "Gamma context (GEX) — futures prices")
        #expect(Markdown.tableCells("| a \\| b | c |") == ["a | b", "c"], "an escaped pipe stays in its cell")
        #expect(Markdown.tableCells("no pipes") == nil)
        #expect(Markdown.isTableDelimiter(":---:") && Markdown.isTableDelimiter("-") && !Markdown.isTableDelimiter("") && !Markdown.isTableDelimiter("a-"))
        #expect(Markdown.blocks("<!-- only a comment -->").isEmpty)
    }

    /// Renders the GEX note for a look by eye: TEST_RUNNER_TJ_MD_PNG=/dir xcodebuild test …
    @MainActor @Test func rendersMarkdown() throws {
        guard let dir = ProcessInfo.processInfo.environment["TJ_MD_PNG"] else { return }
        let r = ImageRenderer(content: MarkdownView(source: Self.gex).padding(16).frame(width: 390).background(Brand.bg))
        r.scale = 3
        try #require(r.uiImage?.pngData()).write(to: URL(fileURLWithPath: dir).appending(path: "markdown-gex.png"))
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
    @MainActor @Test func setsAreOffByDefaultAndAChoiceIsKept() {
        let d = UserDefaults.standard
        let before = d.object(forKey: "tradesBySet")
        defer { if let before { d.set(before, forKey: "tradesBySet") } else { d.removeObject(forKey: "tradesBySet") } }
        d.removeObject(forKey: "tradesBySet")
        #expect(!Settings().tradesBySet, "a fresh install lists trades one by one")
        let s = Settings()
        s.tradesBySet = true
        #expect(Settings().tradesBySet, "switched on, it stays on at the next start")
        s.tradesBySet = false
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

struct NavigationPathTests {
    /// A link to a screen already on the stack returns to it; anything else pushes as usual.
    @Test func aRouteOnTheStackReturnsToIt() {
        let day = Route.day("2026-10-14"), trade = Route.trade(5), note = Route.journal(9)
        #expect(RootView.deduplicated([day, day], was: [day]) == [day], "a day's nudge naming the day")
        #expect(RootView.deduplicated([trade, note, trade], was: [trade, note]) == [trade], "the note's trade that opened it")
        #expect(RootView.deduplicated([trade, note], was: [trade]) == [trade, note], "a new screen pushes")
        #expect(RootView.deduplicated([], was: [trade]) == [], "a pop is a pop")
        #expect(RootView.deduplicated([note, trade], was: [trade, note, trade]) == [note, trade], "a programmatic path is taken as given")
    }
}

// An SVG attachment is told apart from a raster image by its bytes, and its frame is sized
// from the root element before WebKit lays it out.
@Suite struct SVGDrawingTests {
    @Test func detectsSVG() {
        #expect(SVGDrawing.isSVG(Data("<?xml version=\"1.0\"?>\n<svg xmlns=\"http://www.w3.org/2000/svg\"/>".utf8)))
        #expect(SVGDrawing.isSVG(Data("\u{FEFF}  <svg viewBox=\"0 0 1 1\"></svg>".utf8)))
        #expect(!SVGDrawing.isSVG(Data([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A])))
        #expect(!SVGDrawing.isSVG(Data("plain words about <svg> later".utf8)))
    }

    @Test func aspectRatioFromRoot() {
        #expect(SVGDrawing.aspectRatio(of: "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 200 100\"><rect/></svg>") == 2)
        #expect(SVGDrawing.aspectRatio(of: "<svg viewBox='0,0,300,100'/>") == 3)
        #expect(SVGDrawing.aspectRatio(of: "<svg width=\"400px\" height=\"200\"></svg>") == 2)
        #expect(SVGDrawing.aspectRatio(of: "<svg width=\"100%\" height=\"50\"></svg>") == 16.0 / 9.0)
        #expect(SVGDrawing.aspectRatio(of: "<svg><rect viewBox=\"0 0 5 1\"/></svg>") == 16.0 / 9.0)
    }
}
