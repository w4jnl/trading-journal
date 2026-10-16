// Decoding the server's real answers: the fixtures under Fixtures/ are captured from a running
// journal (make ios-fixtures) and scrubbed of what identifies the trader (scripts/scrub-fixtures.py),
// so a field the server renames fails here before it blanks a screen.
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

/// The AI's shapes: what the server adds for a user with a provider, all optional for a server without.
struct AIWireTests {
    @Test func decodesTheAIStatesAndEncodesATarget() throws {
        let pos = try Wire.decoder.decode(PositionAI.self, from: Data(#"{"pending":false,"levels":{"stop":7802.5,"target":7781,"target2":null,"scaleOut":null,"trail":"to break-even after 1R","reasons":{"stop":"above the 20:50 high"},"summary":"Fade the top.","risk":87.5,"r1":1.5,"r2":null,"entry":7794.25,"qty":1},"model":"claude","at":"2026-10-15T20:03:00Z","outdated":false,"auto":"off"}"#.utf8))
        #expect(pos.levels?.stop == 7802.5 && pos.levels?.r1 == 1.5 && pos.model == "claude" && pos.outdated == false)
        let draft = try Wire.decoder.decode(PlanDraft.self, from: Data(#"{"pending":false,"maxTrades":4,"stopTime":"11:00","maxLoss":300,"levels":"7800 / 7750","notes":"Sit out the open.","reasons":["two losers at the open last week"],"model":"claude","createdAt":"2026-10-16T07:00:00Z"}"#.utf8))
        #expect(draft.has && draft.maxTrades == 4 && draft.reasons?.count == 1)
        #expect(!(try Wire.decoder.decode(PlanDraft.self, from: Data(#"{"pending":true}"#.utf8))).has, "being written: no draft yet")
        let run = try Wire.decoder.decode(DayRunResponse.self, from: Data(#"{"status":{"state":"running","date":"2026-10-14","done":2,"total":7,"current":"MESZ6 short 21:16","model":"claude","startedAt":"2026-10-16T07:00:00Z"}}"#.utf8)).status
        #expect(run.running && run.done == 2 && run.total == 7)
        #expect(!(try Wire.decoder.decode(DayRunResponse.self, from: Data(#"{"status":{"state":"idle","date":"","done":0,"total":0,"current":"","startedAt":"0001-01-01T00:00:00Z"}}"#.utf8)).status.running))
        let me = try Fixtures.load(Me.self, "me")
        #expect(me.ai == nil || me.ai?.ready != nil, "a fixture from before the field decodes")
        let review = try Wire.decoder.decode(ReviewState.self, from: Data(#"{"pending":true}"#.utf8))
        #expect(review.pending && review.error == nil)
        // the target names one thing and nothing else
        let t = try JSONSerialization.jsonObject(with: Wire.encoder.encode(ReviewTarget.trade(5))) as! [String: Any]
        #expect(t["scope"] as? String == "trade" && t["tradeId"] as? Int == 5 && t["entryId"] == nil && t["date"] == nil)
        let e = try JSONSerialization.jsonObject(with: Wire.encoder.encode(ReviewTarget.entry(9))) as! [String: Any]
        #expect(e["entryId"] as? Int == 9 && e["scope"] == nil)
    }
}
