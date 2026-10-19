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
        #expect(p.set == nil && p.legs == 1 && st.decisions == nil && st.openSets == nil, "a payload from before the sets decodes")
        #expect(st.grouped == st, "nothing to fold, nothing per decision: the same state")
        let legs = try JSONDecoder().decode(SessionPosition.self, from: Data(#"{"a": 9, "s": "MESZ6", "d": "short", "q": 2, "avg": 7760, "last": 7768.25, "u": -16.5, "tts": 0, "beyond": false, "set": [7, 9]}"#.utf8))
        #expect(legs.set == [7, 9] && legs.setKey == "7,9:MESZ6" && legs.legs == 1)
        #expect(!String(decoding: try JSONEncoder().encode(legs), as: UTF8.self).contains("legs"), "legs is not on the wire")
        let attrs = try JSONDecoder().decode(SessionActivityAttributes.self, from: Data(#"{"day":"2026-10-15","tz":"America/Chicago","rthStart":1760535000,"rthEnd":1760558400}"#.utf8))
        #expect(attrs.timeZone.identifier == "America/Chicago" && attrs.rthRange.upperBound.timeIntervalSince1970 == 1_760_558_400)
    }

    /// The hours in exchange time and the phone's; one line when the phone keeps exchange time.
    @Test func theClockShowsTheHoursInBothZones() {
        // 2026-10-15 08:30-15:00 Chicago (CDT) = 13:30-20:00 UTC = 15:30-22:00 Amsterdam
        let a = SessionActivityAttributes(day: "2026-10-15", tz: "Europe/Amsterdam", rthStart: 1_760_535_000, rthEnd: 1_760_558_400, exchangeTz: "America/Chicago")
        #expect(SessionClock.hours(a, local: TimeZone(identifier: "Europe/Amsterdam")!) == ["08:30–15:00 Chicago", "15:30–22:00 Amsterdam"])
        #expect(SessionClock.hours(a, local: TimeZone(identifier: "America/Chicago")!) == ["08:30–15:00 Chicago"])
        #expect(SessionClock.hours(a, local: TimeZone(identifier: "America/New_York")!) == ["08:30–15:00 Chicago", "09:30–16:00 New York"])
        var old = a
        old.exchangeTz = nil
        #expect(old.exchangeZone.identifier == "America/Chicago", "a payload from before the field")
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

    /// One row per decision: size and unrealised summed, the lead's prices, the nearest stop,
    /// through the stop if any leg is; decisions, their wins and losses and open decisions in
    /// place of the per-leg counts.
    @Test func theGroupedStateFoldsTheLegs() {
        var st = SessionActivityAttributes.ContentState.quiet
        st.trades = 4; st.wins = 3; st.losses = 1; st.openCount = 3
        st.decisions = 2; st.setWins = 2; st.setLosses = 0; st.openSets = 2
        st.positions = [
            SessionPosition(a: 7, s: "MESZ6", d: "short", q: 2, avg: 7760, last: 7768.25, u: -16.5, tts: 0, beyond: false, set: [7, 9]),
            SessionPosition(a: 9, s: "MESZ6", d: "short", q: 2, avg: 7760.25, last: 7768.25, u: -16, tts: -12, beyond: false, set: [7, 9]),
            SessionPosition(a: 7, s: "MNQZ6", d: "long", q: 1, avg: 24_500, last: 24_480, u: -40, tts: 0, beyond: true),
        ]
        let g = st.grouped
        #expect(g.positions.count == 2 && g.trades == 2 && g.wins == 2 && g.losses == 0 && g.openCount == 2)
        let set = g.positions[0]
        #expect(set.a == 7 && set.s == "MESZ6" && set.q == 4 && set.u == -32.5 && set.legs == 2 && set.avg == 7760, "the lead leg's prices, the rest summed")
        #expect(set.tts == -12 && set.hasStop, "the stop distance of the leg that has one")
        #expect(g.positions[1].legs == 1 && g.positions[1].beyond)
        #expect(st.display(bySet: false) == st && st.display(bySet: true) == g)
        var mixed = st
        mixed.positions[0].tts = -20
        #expect(mixed.grouped.positions[0].tts == -12, "the nearest stop among the legs")
        mixed.positions[1].beyond = true
        #expect(mixed.grouped.positions[0].beyond, "through the stop when any leg is")
    }

    /// The server's order and cut, mirrored for the local start: the decision with the most at
    /// stake first, legs together, three decisions and twelve legs at most, whole sets only.
    @Test func theLocalStateArrangesLikeTheServer() {
        func leg(_ a: Int64, _ s: String, _ u: Double, _ set: [Int64]? = nil) -> SessionPosition {
            SessionPosition(a: a, s: s, d: "long", q: 1, avg: 1, last: 1, u: u, tts: 0, beyond: false, set: set)
        }
        let out = SessionActivityAttributes.ContentState.arranged([leg(1, "MESZ6", 10, [1, 2, 3]), leg(5, "MNQZ6", -25), leg(2, "MESZ6", 14, [1, 2, 3]), leg(6, "MCLZ6", 4), leg(3, "MESZ6", 9, [1, 2, 3]), leg(7, "ESZ6", 30)])
        #expect(out.map(\.setKey) == ["1,2,3:MESZ6", "1,2,3:MESZ6", "1,2,3:MESZ6", "7:ESZ6", "5:MNQZ6"] && out[0].a == 2)
        let wide = (1...14).map { leg(Int64($0), "MESZ6", Double($0), Array(1...14)) } + [leg(20, "ESZ6", 1)]
        #expect(SessionActivityAttributes.ContentState.arranged(wide).count == 12)
    }

    /// Today's positions fold the same way for the card and the cache.
    @Test func thePositionsFoldBySet() throws {
        let t = try Fixtures.load(Today.self, "today")
        let folded = t.positions.foldedBySet()
        #expect(folded.count == t.positions.openSets && folded.count <= t.positions.count)
        for p in folded where p.setLegs.count > 0 {
            let legs = t.positions.filter { $0.setKey == p.setKey }
            #expect(p.qty == legs.reduce(0) { $0 + $1.qty }, "the legs' size summed")
            #expect(legs.count == 1 || p.account.contains(", "), "every account named")
        }
        var a = try #require(t.positions.first)
        a.setLegs = [a.accountId + 1]; a.ticksToStop = -30; a.beyondStop = false
        var b = a
        b.accountId += 1; b.setLegs = [a.accountId]; b.ticksToStop = -5; b.unrealized = 2; b.account = "other"
        let one = [a, b].foldedBySet()
        #expect(one.count == 1 && one[0].ticksToStop == -5 && one[0].account == a.account + ", other" && one[0].unrealized == (a.unrealized ?? 0) + 2)
        #expect(a.setKey == b.setKey && [a, b].openSets == 1)
    }

    @Test func theCacheRoundTrips() throws {
        let t = try Fixtures.load(Today.self, "today")
        let c = TodayCache.from(t, days: TodayCache.preview.days, asOf: Date(timeIntervalSince1970: 1_760_600_000))
        #expect(c.openCount == 4 && c.unseen == 2 && !c.hasPlan && c.inGlobex && c.unrealized == t.unrealized.total)
        #expect(c.openSets == t.positions.openSets && c.decisions == (t.decisions?.count ?? t.plan.tradesUsed))
        var per = c
        per.decisions = 2; per.setWins = 2; per.setLosses = 0; per.openSets = 1
        let g = per.display(bySet: true)
        #expect(g.trades == 2 && g.wins == 2 && g.losses == 0 && g.openCount == 1 && per.display(bySet: false) == per)
        let old = try JSONDecoder().decode(TodayCache.self, from: #"{"day":"2026-10-15","tz":"Europe/Amsterdam","asOf":0,"net":1,"unrealized":0,"stale":false,"trades":3,"wins":2,"losses":1,"openCount":0,"hasPlan":false,"maxTrades":0,"tradesUsed":0,"lossUsed":0,"maxLoss":0,"unseen":0,"inGlobex":true,"days":[]}"#.data(using: .utf8)!)
        #expect(old.display(bySet: true).trades == 3, "a cache from before the counts shows what it has")
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
        var sets = open
        sets.positions = [SessionPosition(a: 7, s: "MESZ6", d: "short", q: 2, avg: 7760, last: 7768.25, u: -16.5, tts: -16, beyond: false, set: [7, 9]),
                          SessionPosition(a: 9, s: "MESZ6", d: "short", q: 2, avg: 7760.25, last: 7768.25, u: -16, tts: -15, beyond: false, set: [7, 9])]
        sets.openCount = 2; sets.openSets = 1; sets.decisions = 2; sets.setWins = 2; sets.setLosses = 0
        try save(SessionLockScreen(attributes: attrs, state: sets), "lock-legs.png", width: 370)
        try save(SessionLockScreen(attributes: attrs, state: sets.grouped), "lock-sets.png", width: 370)
        try save(SessionLockScreen(attributes: attrs, state: warn), "lock-warn.png", width: 370)
        try save(SessionLockScreen(attributes: attrs, state: open, isStale: true), "lock-stale.png", width: 370)
        let early = SessionActivityAttributes(day: "2026-10-15", tz: "Europe/Amsterdam", rthStart: now + 3600, rthEnd: now + 3600 + 23_400, exchangeTz: "America/Chicago")
        try save(SessionLockScreen(attributes: early, state: .quiet), "lock-preopen.png", width: 370)
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
