// Decoding the server's real answers: the fixtures under Fixtures/ are captured from a running
// journal (make ios-fixtures), so a field the server renames fails here before it blanks a screen.
import Foundation
import Testing
@testable import TradingJournal

enum Fixtures {
    static func data(_ name: String) throws -> Data {
        let url = try #require(Bundle(for: Marker.self).url(forResource: name, withExtension: "json"), "no fixture \(name).json")
        return try Data(contentsOf: url)
    }

    static func load<T: Decodable>(_ type: T.Type, _ name: String) throws -> T {
        try Wire.decoder.decode(T.self, from: data(name))
    }

    private final class Marker {}
}

struct WireTests {
    @Test func decodesMe() throws {
        let me = try Fixtures.load(Me.self, "me")
        #expect(me.user.username == "trader" && me.key.kind == "device")
        #expect(me.server.timezone == "Europe/Amsterdam" && me.server.tz.identifier == "Europe/Amsterdam")
        #expect(me.server.features.guardrail && !me.server.features.push)
        #expect(me.settings.autoPlan == 0)
    }

    @Test func decodesToday() throws {
        let t = try Fixtures.load(Today.self, "today")
        #expect(t.day == "2026-10-15" && t.positions.count == 4)
        #expect(t.session.inGlobex && t.session.inRth)
        #expect(t.unrealized.stale && t.unrealized.priced == 4)
        #expect(t.nudges.unseen == 2 && t.nudges.items.first?.key == "past-stop-live")
        #expect(t.nudges.items.first?.route == "position/6/MESZ6")
        #expect(t.plan.plan == nil && t.plan.lossFraction == nil)
        #expect(t.lastTrade == nil && t.lastFillAt != nil)
        #expect(t.equity.known && t.equity.byType.count == 2)
        #expect(t.active, "open positions count as an active day")
        let p = try #require(t.positions.first)
        #expect(p.id == "25:MESZ6" && p.direction == .long && p.hasPlan && p.isSuggestion)
        #expect(t.positions.contains { $0.thesis == nil }, "thesis is omitted when empty")
        #expect(p.cycleFills.first?.side == .buy && p.addedFills == 1)
        #expect(p.headline == "long 3 @ 7766.33")
        #expect(p.rewardRisk.map { abs($0 - 2.44) < 0.01 } == true)
    }

    @Test func decodesTradesAndDetail() throws {
        let list = try Fixtures.load(TradeList.self, "trades")
        #expect(list.page.pageSize == 50 && list.page.total > 0)
        let t = try #require(list.items.first)
        #expect(t.id == 368287 && t.direction == .long && t.netPnl == 11.28 && t.r == nil && t.setSize == 5)
        #expect(t.journalTag == nil, "omitted when empty")
        #expect(!t.isOpen && t.when == t.exitTime)
        let d = try Fixtures.load(TradeDetail.self, "trade_detail")
        #expect(d.id == t.id && d.executions.count >= 2)
        #expect(d.executions.first?.role == .entry && d.executions.first?.side == .buy)
        #expect(d.chartPng == "/api/v1/trades/368287/chart.png")
        #expect(d.journalEntryId == nil)
    }

    @Test func decodesJournal() throws {
        let rows = try Fixtures.load(JournalList.self, "journal").items
        let r = try #require(rows.first)
        #expect(r.scope == .group && r.tag == .good && r.mood == nil && r.strategy == nil)
        #expect(rows.contains { $0.strategy == "Post-Close Globex Fade" }, "the list carries the strategy name")
        #expect(r.summary?.trades == 5)
        #expect(!r.plainSnippet.contains("###") && !r.plainSnippet.contains("**"), "the snippet is plain text")
        let e = try Fixtures.load(JournalDetail.self, "journal_detail")
        #expect(e.id == 328 && e.scope == .group && e.trades.count == 5)
        #expect(e.labels.mistakes.isEmpty && e.checklist == nil)
        #expect(Markdown.blocks(e.notes).contains { if case .noteMarker = $0 { return true } else { return false } })
    }

    @Test func decodesStatsNudgesAccountsEquity() throws {
        let s = try Fixtures.load(DailyStats.self, "stats_daily")
        #expect(s.totals.trades == 1742 && s.items.first?.date == "2026-09-01")
        #expect(s.totals.bestDay?.date == "2026-10-14")
        let n = try Fixtures.load(NudgeList.self, "nudges")
        #expect(n.unseen == 2 && n.items.first?.unseen == true && n.items.first?.isWarning == true)
        let a = try Fixtures.load(AccountList.self, "accounts").items
        #expect(a.count > 2)
        let first = try #require(a.first)
        #expect(first.type == .live && first.balance?.anchors.count == 1 && first.dayNet == nil)
        let eq = try Fixtures.load(Equity.self, "equity")
        #expect(eq.included == 15 && eq.drawdown.maxDrawdownDays == 111 && !eq.drawdown.underwater)
        #expect(eq.series.first?.date == "2026-02-27")
        let labels = try Fixtures.load(LabelVocabulary.self, "journal_labels")
        #expect(labels.mistakes.contains { $0.key == "chased" })
        let strategies = try Fixtures.load(StrategyList.self, "strategies").items
        #expect(strategies.first?.name == "Europe Open Liquidity Scalp")
        #expect(try Fixtures.load(DataVersion.self, "version").v == 1)
    }

    @Test func unknownEnumValuesDoNotBreakDecoding() throws {
        let json = #"{"id":1,"scope":"quarter","date":"2026-10-14","tag":"meh","mood":3,"snippet":"x"}"#
        let r = try Wire.decoder.decode(JournalRow.self, from: Data(json.utf8))
        #expect(r.scope == .unknown && r.tag == .unknown)
        let plan = try Wire.decoder.decode(DayPlan.self, from: Data(#"{"day":"2026-10-15","bias":"","levels":"","maxTrades":null,"stopTime":"","maxLoss":null,"notes":"","createdAt":"2026-10-15T07:00:00Z","updatedAt":"2026-10-15T07:00:00Z"}"#.utf8))
        #expect(plan.bias == .none && plan.bias.label == "no bias")
    }

    @Test func datesCarryFractionsAndOffsets() throws {
        let d = try #require(Wire.parseDate("2026-10-14T19:43:10.910169Z"))
        let plain = try #require(Wire.parseDate("2026-10-14T21:43:10+02:00"))
        #expect(abs(d.timeIntervalSince(plain) - 0.910169) < 0.001, "the offset is applied and the fraction kept")
        #expect(Wire.parseDate("yesterday") == nil)
        let s = Wire.rfc3339(Date(timeIntervalSince1970: 1_760_000_000))
        #expect(s.hasPrefix("2025-10-09T") && s.hasSuffix("Z"))
    }

    @Test func encodesTheBodiesTheServerReads() throws {
        let data = try Wire.encoder.encode(PairRequest(code: "AB7Q-K2MN", deviceName: "the trader's iPhone"))
        let s = String(decoding: data, as: UTF8.self)
        #expect(s.contains(#""code":"AB7Q-K2MN""#) && s.contains(#""deviceName""#), "camelCase, as API.md says")
    }

    @Test func sseEventsAreVersions() {
        #expect(SSE.parse(event: "version", data: #"{"v":42}"#) == .version(42))
        #expect(SSE.parse(event: "", data: #"{"v":1}"#) == .version(1), "an event line is optional")
        #expect(SSE.parse(event: "hello", data: #"{"v":1}"#) == nil)
        #expect(SSE.parse(event: "version", data: "ping") == nil)
    }
}
