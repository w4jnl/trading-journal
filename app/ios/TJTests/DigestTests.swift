// The weekly digest as the server sends it, the suggestion rows, and the Sets view's folding.
import Foundation
import Testing
@testable import TradingJournal

struct DigestTests {
    @Test func decodesTheDigest() throws {
        let d = try Fixtures.load(Digest.self, "digest")
        #expect(d.week.hasPrefix("2026-W") && d.tradingDays > 0 && !d.kpis.isEmpty)
        let net = try #require(d.kpis.first { $0.key == "net" })
        #expect(net.text.hasPrefix("+") || net.text.hasPrefix("-"))
        #expect(DigestKPI.format("winRate", 0.8173) == "82%" && DigestKPI.format("decisions", 61) == "61" && DigestKPI.format("avgR", nil) == "—")
        #expect(d.plan.tradingDays == d.tradingDays)
        #expect(d.lessons.contains { $0.mistake == nil }, "a lesson about no mistake stands alone")
    }

    @Test func suggestionsReadAsSentences() {
        #expect(Suggestion(kind: "mistake", value: "chased", name: "Chased the entry").title == "mistake: Chased the entry")
        #expect(Suggestion(kind: "strategy", value: "12", name: "VWAP Bounce").title == "VWAP Bounce")
        #expect(Suggestion(kind: "tag", value: "good", name: "good").title == "tag good")
        #expect(Suggestion(kind: "emotion", value: "calm", name: "Calm").id == "emotion:calm")
    }

    @Test func theSetsViewFoldsACopiedDecision() throws {
        let rows = try Fixtures.load(TradeList.self, "trades").items
        let folded = TradesView.bySet(rows)
        let sets = Set(rows.compactMap { $0.setSize > 1 ? $0.setId : nil })
        let solos = rows.filter { $0.setSize <= 1 }.count
        #expect(folded.count == sets.count + solos)
        if let set = sets.first, let lead = folded.first(where: { $0.setId == set }) {
            let legs = rows.filter { $0.setId == set }
            #expect(lead.qty == legs.reduce(0) { $0 + $1.qty })
            #expect(abs((lead.netPnl ?? 0) - legs.reduce(0) { $0 + ($1.netPnl ?? 0) }) < 0.001)
            #expect(lead.groupCount == legs.count)
        }
    }
}
