// Pushes: the category table, what a payload says, the settings as the server reads them, the
// registration record, and the card rendered for a look by eye.
import Foundation
import SwiftUI
import Testing
import UserNotifications
@testable import TradingJournal

struct PushPayloadTests {
    @Test func readsWhatTheServerSends() throws {
        let nudge = try #require(PushPayload(["kind": "nudge", "id": 9, "key": "past-stop-live", "route": "position/25/MESZ6", "day": "2026-10-15", "lesson": "wait"]))
        #expect(nudge.kind == .nudge && nudge.id == 9 && nudge.isPositionNudge && nudge.lesson == "wait")
        #expect(nudge.destination == .position(account: 25, symbol: "MESZ6") && nudge.position?.symbol == "MESZ6")
        let trade = try #require(PushPayload(["kind": "trade", "tradeId": 368287, "route": "trade/368287", "symbolRoot": "MES", "direction": "long", "netPnl": 11.28, "accountId": 25]))
        #expect(trade.kind == .trade && trade.tradeId == 368287 && trade.netPnl == 11.28 && trade.destination == .trade(368287))
        let sync = try #require(PushPayload(["kind": "sync", "v": 140, "badge": 1, "unseen": [9, 12]]))
        #expect(sync.kind == .sync && sync.v == 140 && sync.badge == 1 && sync.unseen == [9, 12])
        let plan = try #require(PushPayload(["kind": "nudge", "key": "plan-missing", "route": "plan", "day": "2026-10-15"]))
        #expect(plan.id == 0 && plan.destination == .plan)
        #expect(PushPayload(["aps": ["alert": "x"]]) == nil, "no kind: not a journal push")
    }

    @Test func numbersArriveAsTheyMay() throws {
        // simctl and APNs hand numbers as NSNumber; a JSON file decoded by hand may give doubles
        let p = try #require(PushPayload(["kind": "sync", "v": 3.0, "badge": 2.0, "unseen": [4.0]]))
        #expect(p.v == 3 && p.badge == 2 && p.unseen == [4])
    }

    @Test func thePayloadFilesParse() throws {
        for name in ["nudge", "day-nudge", "trade-closed", "plan-missing", "sync"] {
            let url = try #require(Bundle(for: Marker.self).url(forResource: name, withExtension: "apns"), "\(name).apns")
            let obj = try #require(try JSONSerialization.jsonObject(with: Data(contentsOf: url)) as? [String: Any])
            #expect(obj["Simulator Target Bundle"] as? String == "nl.w4j.tradingjournal")
            #expect(PushPayload(obj) != nil, "\(name)")
        }
    }

    private final class Marker {}
}

struct CategoryTests {
    @Test func theTableMatchesTheServer() {
        #expect(Set(PushCategory.all.map(\.identifier)) == ["TJ_NUDGE", "TJ_TRADE", "TJ_POSITION"])
        #expect(PushCategory.nudge.actions.map(\.identifier) == ["SEEN", "OPEN"])
        #expect(PushCategory.position.actions.map(\.identifier) == ["SEEN", "OPEN"])
        #expect(PushCategory.trade.actions.map(\.identifier) == ["NOTE", "OPEN"])
        #expect(PushCategory.trade.actions.first is UNTextInputNotificationAction, "Note… is the text field")
        #expect(PushCategory.nudge.category.options.contains(.customDismissAction), "a swipe away marks the nudge seen")
        #expect(!PushCategory.trade.category.options.contains(.customDismissAction))
    }

    @Test func aPushWhileOpenShowsByKind() {
        #expect(Notifications.presentation(for: PushPayload(kind: .nudge)) == [.banner, .sound, .badge, .list])
        #expect(Notifications.presentation(for: PushPayload(kind: .trade)) == [.list, .badge])
        #expect(Notifications.presentation(for: PushPayload(kind: .sync)) == [])
        #expect(Notifications.presentation(for: nil) == [.banner, .sound, .badge, .list])
    }
}

struct DevicePrefsTests {
    @Test func defaultsAreTheServers() {
        let p = DevicePrefs()
        #expect(p.isOn(.nudge) && p.isOn(.tradeClosed) && !p.isOn(.planMissing), "the reminder is opt-in")
        #expect(p.timeSensitive(.nudge) && !p.timeSensitive(.tradeClosed))
        #expect(p.sound(.nudge) == .tj && p.showsActivity)
    }

    @Test func kindsBackAtTheirDefaultsAreDropped() {
        var p = DevicePrefs()
        p.setOn(.tradeClosed, false)
        p.setSound(.nudge, .none)
        p.setTimeSensitive(.nudge, false)
        p.setOn(.planMissing, true)
        #expect(!p.isOn(.tradeClosed) && p.sound(.nudge) == .none && !p.timeSensitive(.nudge) && p.isOn(.planMissing))
        p.setOn(.tradeClosed, true)
        p.setSound(.nudge, .tj)
        p.setTimeSensitive(.nudge, true)
        p.setOn(.planMissing, false)
        #expect(p.kinds.isEmpty, "all back to the defaults: nothing left to store")
    }

    @Test func encodesTheWayTheServerReadsThem() throws {
        var p = DevicePrefs()
        p.setTimeSensitive(.nudge, false)
        p.setSound(.tradeClosed, .system)
        p.setOn(.planMissing, true)
        p.quietOutsideRth = true
        let json = try JSONSerialization.jsonObject(with: Wire.encoder.encode(p)) as! [String: Any]
        let kinds = json["kinds"] as! [String: [String: Any]]
        #expect(kinds["nudge"]?["timeSensitive"] as? Bool == false)
        #expect(kinds["tradeClosed"]?["sound"] as? String == "default")
        #expect(kinds["planMissing"]?["off"] as? Bool == false, "opting in is off:false, as the server reads it")
        #expect(json["quietOutsideRth"] as? Bool == true && json["noActivity"] == nil && json["finishedAsNotification"] == nil)
        let back = try JSONDecoder().decode(DevicePrefs.self, from: Wire.encoder.encode(p))
        #expect(back == p)
    }

    @Test func registrationCarriesTheWhole() throws {
        let r = DeviceRegistration(token: "6f1a", name: "the trader's iPhone", sandbox: true, bundleId: "nl.w4j.tradingjournal", prefs: DevicePrefs())
        let s = String(decoding: try Wire.encoder.encode(r), as: UTF8.self)
        #expect(s.contains(#""token":"6f1a""#) && s.contains(#""sandbox":true"#) && s.contains(#""prefs":{"kinds":{}}"#))
        let d = try Wire.decoder.decode(DeviceRegistered.self, from: Data(#"{"device":{"token":"6f1a","name":"x","sandbox":true,"bundleId":"nl.w4j.tradingjournal","prefs":{},"createdAt":"2026-10-15T20:00:00Z"},"push":true}"#.utf8))
        #expect(d.push && d.device.token == "6f1a")
    }
}

struct RegistrationTests {
    @Test func theRegisteredRecordNamesTheServer() {
        let server = "https://j.example.net"
        #expect(Settings.registeredToken("\(server)|aaaa1111", serverURL: server) == "aaaa1111")
        #expect(Settings.registeredToken("https://other.example|aaaa1111", serverURL: server) == nil, "another server's token is not ours")
        #expect(Settings.registeredToken(nil, serverURL: server) == nil)
        #expect(Settings.registeredToken("garbage", serverURL: server) == nil)
    }
}

struct CardTests {
    /// Renders the card for a look by eye: TEST_RUNNER_TJ_CARD_PNG=/dir xcodebuild test …
    @MainActor @Test func rendersTheCards() throws {
        guard let dir = ProcessInfo.processInfo.environment["TJ_CARD_PNG"] else { return }
        let today = try Fixtures.load(Today.self, "today")
        let p = try #require(today.positions.first)
        let nudge = PushPayload(kind: .nudge, id: 9, key: "past-stop-live", route: "position/25/MESZ6", day: "2026-10-15", lesson: "only close early if a defined invalidation prints")
        let cards: [(String, NotificationCard)] = [
            ("card-position", NotificationCard(push: nudge, title: "Price is through your stop on MESZ6", subtitle: "MESZ6 · open",
                                               body_: "Last 7752.50 against the recorded stop 7757.25 on long 3. — Your lesson: only close early if a defined invalidation prints", data: .position(p), status: "seen")),
            ("card-day", NotificationCard(push: PushPayload(kind: .nudge, id: 7, key: "revenge", route: "day/2026-10-14", day: "2026-10-14"), title: "Re-entry right after a loss", subtitle: "Wed 14 Oct",
                                          body_: "MES short opened 1s after a -203.47 loss on the same market.", data: .today(net: 1751.76, open: -33.5, stale: true))),
            ("card-trade", NotificationCard(push: PushPayload(kind: .trade, route: "trade/368287", tradeId: 368287, netPnl: 11.28), title: "MES long", subtitle: "EXPRESS-V2-100000-57419676",
                                            body_: "+11.28 · 21:57", data: .trade(try Fixtures.load(TradeDetail.self, "trade_detail")), tz: today.tz)),
            ("card-loading", NotificationCard(push: nudge, title: "Price is through your stop on MESZ6", subtitle: "MESZ6 · open", body_: "Last 7752.50 against the recorded stop.", data: .loading)),
        ]
        for (name, card) in cards {
            let r = ImageRenderer(content: card.frame(width: 390))
            r.scale = 3
            try #require(r.uiImage?.pngData()).write(to: URL(fileURLWithPath: dir).appending(path: name + ".png"))
        }
    }
}
