// Every write the phone can do, against a running journal: TEST_RUNNER_TJ_SERVER and
// TEST_RUNNER_TJ_KEY (a device key) name it, else these are skipped. They leave the journal as
// they found it: what they create they delete, what they change they change back.
import Foundation
import Testing
@testable import TradingJournal

@MainActor
@Suite(.serialized) // one journal, one writer at a time
struct LiveWritesTests {
    private static var client: Client? {
        let env = ProcessInfo.processInfo.environment
        guard let server = env["TJ_SERVER"], !server.isEmpty, let key = env["TJ_KEY"], !key.isEmpty else { return nil } // make passes empty ones when unset
        let s = Settings()
        s.serverURL = server
        s.apiKey = key
        return Client(settings: s)
    }

    @Test func journalEditAppendAndConflicts() async throws {
        guard let c = Self.client else { return }
        let rows = try await c.journal(.init(scopes: [.group], limit: 1))
        let id = try #require(rows.first?.id, "the journal needs a group entry")
        let before = try await c.entry(id)

        // a stale save answers with the current entry
        var stale = JournalUpdate(before)
        stale.updatedAt = before.updatedAt.addingTimeInterval(-60)
        await #expect(throws: Client.JournalSaveError.self) { try await c.saveJournal(id, stale) }
        do {
            _ = try await c.saveJournal(id, stale)
        } catch Client.JournalSaveError.stale(let current) {
            #expect(current.id == id && current.updatedAt == before.updatedAt)
        }

        // a real edit, then the entry put back
        var edit = JournalUpdate(before)
        edit.body = before.notes + "\n\nphone edit"
        edit.emotions = ["calm"]
        let after = try await c.saveJournal(id, edit)
        #expect(after.notes.hasSuffix("phone edit") && after.labels.emotions == ["calm"] && after.updatedAt >= before.updatedAt)

        try await c.appendNote(id, "appended from the phone")
        let appended = try await c.entry(id)
        #expect(appended.notes.contains("appended from the phone") && appended.notes.contains("Note added"))

        var restore = JournalUpdate(before)
        restore.updatedAt = appended.updatedAt
        let restored = try await c.saveJournal(id, restore)
        #expect(restored.labels == before.labels && restored.tag == before.tag)
        if restored.notes != before.notes {
            let a = Array(before.notes), b = Array(restored.notes)
            let i = zip(a, b).enumerated().first { $0.element.0 != $0.element.1 }?.offset ?? min(a.count, b.count)
            Issue.record("notes differ at \(i) of \(a.count)/\(b.count): before …\(String(a[max(0, i - 30)..<min(a.count, i + 40)]).debugDescription) vs after …\(String(b[max(0, i - 30)..<min(b.count, i + 40)]).debugDescription)")
        }
    }

    @Test func createDeleteAndExists() async throws {
        guard let c = Self.client else { return }
        let trades = try await c.trades(.init())
        let t = try #require(trades.items.first { !$0.hasJournal }, "needs a trade without a note")
        let made = try await c.createJournal(JournalCreate(scope: .trade, body: "a note from the phone", tag: .neutral, tradeId: t.id))
        #expect(made.scope == .trade && made.tag == .neutral && made.trades.first?.id == t.id)
        do {
            _ = try await c.createJournal(JournalCreate(scope: .trade, body: "again", tradeId: t.id))
            Issue.record("a second trade note should answer 409")
        } catch Client.JournalSaveError.exists(let id) {
            #expect(id == made.id)
        }
        try await c.deleteJournal(made.id)
        await #expect(throws: TJError.http(404, "entry not found")) { _ = try await c.entry(made.id) }
    }

    @Test func uploadAnImage() async throws {
        guard let c = Self.client else { return }
        // a 1x1 PNG
        let png = Data(base64Encoded: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")!
        let up = try await c.upload(image: png, name: "dot.png", mimeType: "image/png")
        #expect(up.markdown == "![dot.png](\(up.url))" && up.url.hasPrefix("/api/v1/attachments/"))
        let back = try await c.image(up.url)
        #expect(back.count > 0)
    }

    @Test func dayPlanRoundTrip() async throws {
        guard let c = Self.client else { return }
        await c.refreshToday()
        let today = try #require(c.today)
        let original = today.plan.plan
        var p = DayPlanSave(day: "today", plan: original)
        p.bias = .long
        p.maxTrades = 4
        p.stopTime = "15:30"
        p.maxLoss = 300
        p.notes = "phone plan"
        let saved = try #require(try await c.savePlan(p))
        #expect(saved.bias == .long && saved.maxTrades == 4 && saved.stopTime == "15:30" && saved.maxLoss == 300)
        #expect(c.today?.plan.plan?.maxTrades == 4, "Today follows the save")
        if let original {
            _ = try await c.savePlan(DayPlanSave(day: "today", plan: original))
        } else {
            let cleared = try await c.savePlan(DayPlanSave(day: "today", plan: nil))
            #expect(cleared == nil, "all fields empty: the plan is gone")
        }
    }

    @Test func positionPlanRoundTrip() async throws {
        guard let c = Self.client else { return }
        await c.refreshToday()
        let p = try #require(c.today?.positions.first, "needs an open position")
        let original = PositionPlanSave(stop: p.stop, risk: nil, target: p.target, thesis: p.thesisText, allLegs: false)
        let stop = p.direction == .long ? (p.avgEntry ?? 0) - 10 * p.tickSize : (p.avgEntry ?? 0) + 10 * p.tickSize
        let saved = try await c.savePositionPlan(account: p.accountId, symbol: p.symbol, PositionPlanSave(stop: stop, risk: nil, target: nil, thesis: "phone thesis", allLegs: false))
        #expect(saved.stop == stop && saved.thesis == "phone thesis" && saved.planSource == "trader", "a device key writes as the trader")
        let risk = try #require(saved.risk)
        #expect(abs(risk - 10 * p.tickSize * p.pointValue * p.qty) < 0.01)
        _ = try await c.savePositionPlan(account: p.accountId, symbol: p.symbol, original)
    }

    @Test func settingsAndBalances() async throws {
        guard let c = Self.client else { return }
        let me = try await c.me()
        let flipped = try await c.updateSettings(SettingsUpdate(guardrail: !me.settings.guardrail))
        #expect(flipped.guardrail == !me.settings.guardrail && flipped.autoPlan == me.settings.autoPlan)
        _ = try await c.updateSettings(SettingsUpdate(guardrail: me.settings.guardrail))

        let a = try #require(try await c.accounts().first)
        let added = try await c.addBalance(account: a.id, BalanceAnchorAdd(date: "today", balance: 1234.5, note: "phone"))
        let anchor = try #require(added.balance?.anchors.first { $0.note == "phone" })
        #expect(anchor.balance == 1234.5)
        try await c.deleteBalance(account: a.id, anchor: anchor.id)
        let after = try #require(try await c.accounts().first { $0.id == a.id })
        #expect(after.balance?.anchors.contains { $0.id == anchor.id } != true)
    }
}
