// The write bodies as the server reads them, the 409 shapes the editor acts on, and the plan maths.
import Foundation
import Testing
@testable import TradingJournal

struct WriteBodyTests {
    @Test func aJournalUpdateCarriesTheWholeEntry() throws {
        let e = try Fixtures.load(JournalDetail.self, "journal_detail")
        var u = JournalUpdate(e)
        #expect(u.body == e.notes && u.tag == .good && u.updatedAt == e.updatedAt && u.checks.isEmpty)
        u.tag = nil
        u.mistakes = ["chased"]
        let json = try JSONSerialization.jsonObject(with: Wire.encoder.encode(u)) as! [String: Any]
        #expect(json["tag"] == nil, "no tag on the wire removes the tag")
        #expect(json["mood"] == nil && json["strategyId"] == nil)
        #expect(json["mistakes"] as? [String] == ["chased"] && json["emotions"] as? [String] == [])
        #expect(json["checks"] as? [Int] == [])
        #expect((json["updatedAt"] as? String)?.hasSuffix("Z") == true, "RFC 3339 as loaded")
    }

    @Test func aJournalCreateSendsOnlyItsScopesFields() throws {
        let day = try JSONSerialization.jsonObject(with: Wire.encoder.encode(JournalCreate(scope: .day, body: "x", tag: .neutral, date: "2026-10-14"))) as! [String: Any]
        #expect(day["scope"] as? String == "day" && day["date"] as? String == "2026-10-14" && day["tag"] as? String == "neutral")
        #expect(day["tradeId"] == nil && day["mood"] == nil)
        let trade = try JSONSerialization.jsonObject(with: Wire.encoder.encode(JournalCreate(scope: .trade, body: "x", tradeId: 5))) as! [String: Any]
        #expect(trade["tradeId"] as? Int == 5 && trade["tag"] == nil)
    }

    @Test func aDayPlanSaveClearsWithZeroAndEmpty() throws {
        var p = DayPlanSave(day: "today", plan: nil)
        #expect(p.bias == .none && p.maxTrades == 0 && p.stopTime.isEmpty)
        p.bias = .long
        p.maxTrades = 4
        p.maxLoss = 300
        let json = try JSONSerialization.jsonObject(with: Wire.encoder.encode(p)) as! [String: Any]
        #expect(json["bias"] as? String == "long" && json["maxTrades"] as? Int == 4 && json["maxLoss"] as? Double == 300)
        #expect(json["stopTime"] as? String == "" && json["levels"] as? String == "", "present and empty: cleared")
        let existing = try Wire.decoder.decode(DayPlan.self, from: Data(#"{"day":"2026-10-15","bias":"short","levels":"7750","maxTrades":3,"stopTime":"15:30","maxLoss":250,"notes":"n","createdAt":"2026-10-15T07:00:00Z","updatedAt":"2026-10-15T07:00:00Z"}"#.utf8))
        let fromPlan = DayPlanSave(day: "2026-10-15", plan: existing)
        #expect(fromPlan.bias == .short && fromPlan.maxTrades == 3 && fromPlan.stopTime == "15:30" && fromPlan.maxLoss == 250)
    }

    @Test func aPositionPlanSendsNullToClear() throws {
        let s = String(decoding: try Wire.encoder.encode(PositionPlanSave(stop: nil, risk: nil, target: 7788.5, thesis: "t", allLegs: false)), as: UTF8.self)
        #expect(s.contains(#""stop":null"#) && s.contains(#""risk":null"#) && s.contains(#""target":7788.5"#) && s.contains(#""allLegs":false"#))
    }

    @Test func settingsAndBalancesAndIds() throws {
        let s = String(decoding: try Wire.encoder.encode(SettingsUpdate(guardrail: nil, autoPlan: 2)), as: UTF8.self)
        #expect(s == #"{"autoPlan":2}"#, "an absent switch is left alone")
        let b = String(decoding: try Wire.encoder.encode(BalanceAnchorAdd(date: "today", balance: 2891.25, note: "")), as: UTF8.self)
        #expect(b.contains(#""balance":2891.25"#) && b.contains(#""date":"today""#))
        #expect(String(decoding: try Wire.encoder.encode(NudgeIDs(ids: [1, 2])), as: UTF8.self) == #"{"ids":[1,2]}"#)
    }

    @Test func multipartIsWhatGoReads() {
        let body = Multipart.body(boundary: "B", field: "file", filename: "a\"b.jpg", mimeType: "image/jpeg", data: Data([0xFF, 0xD8]))
        let s = String(decoding: body, as: UTF8.self)
        #expect(s.hasPrefix("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"ab.jpg\"\r\nContent-Type: image/jpeg\r\n\r\n"))
        #expect(s.hasSuffix("\r\n--B--\r\n"))
    }
}

struct ConflictTests {
    @Test func aStaleSaveCarriesTheCurrentEntry() throws {
        let entry = try Fixtures.data("journal_detail")
        let body = Data(#"{"error":"the entry changed since you loaded it","reason":"stale","entry":"#.utf8) + entry + Data("}".utf8)
        let resp = HTTPURLResponse(url: URL(string: "https://j.example.net/api/v1/journal/328")!, statusCode: 409, httpVersion: nil, headerFields: nil)!
        #expect(throws: TJError.conflict(reason: "stale", message: "the entry changed since you loaded it", body: body)) {
            try TJAPI.check(body, resp)
        }
        let c = try Wire.decoder.decode(JournalConflict.self, from: body)
        #expect(c.reason == "stale" && c.entry?.id == 328)
        let pending = Data(#"{"error":"an AI review is writing this note","reason":"review-pending"}"#.utf8)
        let p = try Wire.decoder.decode(JournalConflict.self, from: pending)
        #expect(p.reason == "review-pending" && p.entry == nil)
        let exists = try Wire.decoder.decode(JournalConflict.self, from: Data(#"{"error":"a day entry already exists for 2026-06-02","entryId":8}"#.utf8))
        #expect(exists.entryId == 8 && exists.reason == nil)
    }

    @Test func otherCodesStayPlainErrors() throws {
        let resp = HTTPURLResponse(url: URL(string: "https://j.example.net/api/v1/journal/1")!, statusCode: 403, httpVersion: nil, headerFields: nil)!
        #expect(throws: TJError.http(403, "this call needs a device key")) {
            try TJAPI.check(Data(#"{"error":"this call needs a device key"}"#.utf8), resp)
        }
    }
}

struct PlanMathTests {
    @Test func riskAndRewardFollowThePosition() throws {
        let p = try #require(try Fixtures.load(Today.self, "today").positions.first)
        // MES long 3 @ 7766.33: 5 a point
        let risk = try #require(PlanMath.risk(stop: 7757.25, position: p))
        #expect(abs(risk - 136.25) < 0.01, "what the server shows for the same stop")
        #expect(PlanMath.risk(stop: nil, position: p) == nil)
        let rr = try #require(PlanMath.rewardRisk(stop: 7757.25, target: 7788.5, avgEntry: p.avgEntry))
        #expect(abs(rr - 2.44) < 0.01)
        #expect(PlanMath.rewardRisk(stop: 7766.333333333333, target: 7788.5, avgEntry: p.avgEntry) == nil, "a stop at the entry has no ratio")
        #expect(PlanMath.stopIsWrongSide(stop: 7770, position: p), "a stop above a long entry")
        #expect(!PlanMath.stopIsWrongSide(stop: 7757.25, position: p))
        #expect(PlanMath.parse(" 7 757,25 ".replacingOccurrences(of: " ", with: "")) == 7757.25 && PlanMath.parse("") == nil && PlanMath.parse("x") == nil)
    }
}
