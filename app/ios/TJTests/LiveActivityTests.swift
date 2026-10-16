// The Session activity: the server's content state as ActivityKit decodes it, the local state
// built from Today (same rules, under the 4 KB limit), the widgets' cache, and the faces
// rendered for a look by eye.
import Foundation
import SwiftUI
import Testing
@testable import TradingJournal

struct LiveActivityTests {
    /// What push/live.go sends as content-state: the names and shapes ActivityKit decodes.
    @Test func decodesTheServersContentState() throws {
        let json = #"{"realized": 120.5, "unrealized": 13.75, "trades": 4, "wins": 3, "losses": 1, "maxTrades": 6, "lossUsed": 84.2, "maxLoss": 300, "positions": [{"a": 7, "s": "MESZ6", "d": "long", "q": 3, "avg": 7766.25, "last": 7767.25, "u": 13.75, "tts": 49, "beyond": false}], "openCount": 1, "nudges": 1, "lastFill": 1760530000, "updated": 1760530060, "stale": false, "alert": ""}"#
        let st = try JSONDecoder().decode(SessionActivityAttributes.ContentState.self, from: Data(json.utf8))
        #expect(st.total == 134.25 && st.kind == .warn, "a nudge waits: warn")
        #expect(st.lossFraction.map { abs($0 - 0.2807) < 0.001 } == true)
        let p = try #require(st.positions.first)
        #expect(p.id == "7:MESZ6" && p.hasStop && p.link == URL(string: "tj://open/position/7/MESZ6"))
        let attrs = try JSONDecoder().decode(SessionActivityAttributes.self, from: Data(#"{"day":"2026-10-15","tz":"America/Chicago","rthStart":1760535000,"rthEnd":1760558400}"#.utf8))
        #expect(attrs.timeZone.identifier == "America/Chicago" && attrs.rthRange.upperBound.timeIntervalSince1970 == 1_760_558_400)
    }

    @Test func theKindIsTheGlance() {
        var st = SessionActivityAttributes.ContentState.quiet
        #expect(st.kind == .quiet)
        st.trades = 2
        st.realized = -40
        #expect(st.kind == .neg)
        st.unrealized = 60
        #expect(st.kind == .pos, "the total decides")
        st.positions = [SessionPosition(a: 1, s: "ESZ6", d: "long", q: 1, avg: 1, last: 1, u: 0, tts: 0, beyond: true)]
        #expect(st.kind == .warn, "a breached stop")
        #expect(!SessionPosition(a: 1, s: "ESZ6", d: "long", q: 1, avg: 1, last: 1, u: 0, tts: 0, beyond: false).hasStop)
    }

    @Test func theLocalStateFollowsTodayAndFitsTheLimit() throws {
        let t = try Fixtures.load(Today.self, "today")
        let st = SessionActivityAttributes.ContentState.from(t, now: Date(timeIntervalSince1970: 1_760_600_000))
        #expect(st.positions.count == 3, "at most three of the four, by |unrealised|")
        #expect(abs(st.positions[0].u) >= abs(st.positions[1].u) && abs(st.positions[1].u) >= abs(st.positions[2].u))
        #expect(st.openCount == 4 && st.nudges == 2 && st.realized == t.stats.net && st.stale == t.unrealized.stale)
        #expect(st.maxTrades == 0 && st.maxLoss == 0, "no plan: no cap, no budget")
        #expect(st.updated == 1_760_600_000 && st.lastFillDate != nil)
        let attrs = SessionActivityAttributes.from(t)
        #expect(attrs.day == "2026-10-15" && attrs.tz == "Europe/Amsterdam" && attrs.rthRange.contains(t.session.rthStart.addingTimeInterval(60)))
        // the limit, with three long rows
        var fat = st
        fat.positions = (0..<3).map { SessionPosition(a: 123_456_789, s: "MNQZ6-LONG-CONTRACT-NAME-\($0)", d: "short", q: 123.5, avg: 24_567.75, last: 24_568.25, u: -12_345.25, tts: -1234, beyond: true) }
        fat.alert = String(repeating: "x", count: 200)
        #expect(try JSONEncoder().encode(fat).count < 4096)
    }

    @Test func theCacheRoundTrips() throws {
        let t = try Fixtures.load(Today.self, "today")
        let c = TodayCache.from(t, days: TodayCache.preview.days, asOf: Date(timeIntervalSince1970: 1_760_600_000))
        #expect(c.openCount == 4 && c.unseen == 2 && !c.hasPlan && c.inGlobex && c.unrealized == t.unrealized.total)
        #expect(abs(c.weekNet - (639 + 135 - 313 + 1751.76 + 451)) < 0.01, "the last five days")
        let d = UserDefaults(suiteName: "tj.tests.cache")!
        d.removePersistentDomain(forName: "tj.tests.cache")
        #expect(TodayCache.read(defaults: d) == nil)
        c.write(defaults: d)
        #expect(TodayCache.read(defaults: d) == c)
        let empty = TodayCache.from(try Fixtures.load(Today.self, "today").with(positions: []), days: [])
        #expect(empty.unrealized == 0, "no open positions: no unrealised, whatever the stale total says")
    }

    /// Renders the Lock Screen card and the island's parts for a look by eye: TEST_RUNNER_TJ_LIVE_PNG=/dir
    @MainActor @Test func rendersTheActivity() throws {
        guard let dir = ProcessInfo.processInfo.environment["TJ_LIVE_PNG"] else { return }
        let now = Int64(Date().timeIntervalSince1970)
        let attrs = SessionActivityAttributes(day: "2026-10-15", tz: "Europe/Amsterdam", rthStart: now - 3600, rthEnd: now + 5400)
        let open = SessionActivityAttributes.ContentState(realized: 120.5, unrealized: 13.75, trades: 4, wins: 3, losses: 1, maxTrades: 6, lossUsed: 84.2, maxLoss: 300,
                                                          positions: [SessionPosition(a: 7, s: "MESZ6", d: "long", q: 3, avg: 7766.25, last: 7767.25, u: 13.75, tts: 49, beyond: false),
                                                                      SessionPosition(a: 9, s: "MNQZ6", d: "short", q: 1, avg: 24_500, last: 24_480, u: 40, tts: 0, beyond: false)],
                                                          openCount: 2, nudges: 0, lastFill: now - 600, updated: now, stale: false, alert: "")
        var warn = open
        warn.realized = -210
        warn.unrealized = -48.5
        warn.lossUsed = 258.5
        warn.nudges = 1
        warn.positions = [SessionPosition(a: 7, s: "MESZ6", d: "short", q: 2, avg: 7760, last: 7768.25, u: -48.5, tts: -3, beyond: true)]
        func save<V: View>(_ v: V, _ name: String, width: CGFloat) throws {
            let r = ImageRenderer(content: v.frame(width: width).background(Brand.dark.bg))
            r.scale = 3
            try #require(r.uiImage?.pngData()).write(to: URL(fileURLWithPath: dir).appending(path: name))
        }
        try save(SessionLockScreen(attributes: attrs, state: open), "lock-open.png", width: 370)
        try save(SessionLockScreen(attributes: attrs, state: warn), "lock-warn.png", width: 370)
        try save(SessionLockScreen(attributes: attrs, state: open, isStale: true), "lock-stale.png", width: 370)
        try save(SessionExpandedBottom(attributes: attrs, state: warn).padding(12), "expanded.png", width: 370)
        try save(HStack { SessionCompactLeading(state: open); Spacer(); SessionCompactTrailing(state: open) }.padding(8), "compact.png", width: 200)
        try save(TodayWidgetPreview(cache: .preview, family: .systemMedium).frame(height: 158), "widget-medium.png", width: 338)
        try save(TodayWidgetPreview(cache: .preview, family: .systemSmall).frame(height: 158), "widget-small.png", width: 158)
    }
}

/// The widget views live in the extension; the app's tests render a stand-in of the same cache.
struct TodayWidgetPreview: View {
    let cache: TodayCache
    let family: String_Family
    enum String_Family { case systemSmall, systemMedium }
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("TODAY").font(Brand.text(10, weight: .medium)).foregroundStyle(Brand.dark.fgMuted)
            Text(Formatting.money(cache.net)).font(Brand.mono(family == .systemSmall ? 26 : 24, bold: true)).foregroundStyle(cache.net < 0 ? Brand.dark.neg : Brand.dark.pos)
            Text("\(cache.trades) trades · \(cache.openCount) open").font(Brand.text(11)).foregroundStyle(Brand.dark.fgMuted)
        }
        .padding(14)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .environment(\.colorScheme, .dark)
    }
}

private extension Today {
    func with(positions: [Position]) -> Today {
        var t = self
        t.positions = positions
        return t
    }
}
