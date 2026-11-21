// The widgets' day: held with its weekday until the rollover hour in the journal's timezone,
// then a blank day dated in advance on the timeline; a cache already on the new day stays.
import Foundation
import Testing
@testable import TradingJournal

struct WidgetDayTests {
    /// Friday 2026-11-20 with a result, in Europe/Amsterdam.
    private var friday: TodayCache {
        var c = TodayCache.preview
        c.day = "2026-11-20"; c.tz = "Europe/Amsterdam"
        c.net = 640; c.trades = 4; c.wins = 3; c.losses = 1; c.unseen = 2
        c.hasPlan = true; c.maxTrades = 6; c.tradesUsed = 4; c.lossUsed = 120; c.maxLoss = 300
        c.decisions = 3; c.setWins = 2; c.setLosses = 1
        c.days = [.init(date: "2026-11-18", net: 100, trades: 2), .init(date: "2026-11-19", net: -50, trades: 1)]
        return c
    }

    private func amsterdam(_ s: String) -> Date {
        let f = DateFormatter()
        f.timeZone = TimeZone(identifier: "Europe/Amsterdam")
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd HH:mm"
        return f.date(from: s)!
    }

    @Test func theDayHoldsUntilTheRolloverHour() {
        let c = friday
        // Friday evening: today, next flip Saturday 05:00.
        let eve = c.timeline(at: amsterdam("2026-11-20 23:30"), rolloverHour: 5)
        #expect(eve.current == c && !c.isBehind(at: amsterdam("2026-11-20 23:30")))
        #expect(eve.next?.at == amsterdam("2026-11-21 05:00") && eve.next?.day.day == "2026-11-21")
        // Saturday 02:00: still Friday's figure, now marked as Friday's, same flip ahead.
        let night = c.timeline(at: amsterdam("2026-11-21 02:00"), rolloverHour: 5)
        #expect(night.current == c && c.isBehind(at: amsterdam("2026-11-21 02:00")) && c.weekday.hasPrefix("Fri"))
        #expect(night.next?.at == amsterdam("2026-11-21 05:00"))
        // Saturday 05:00 and later: the blank Saturday, nothing ahead.
        let morning = c.timeline(at: amsterdam("2026-11-21 05:00"), rolloverHour: 5)
        #expect(morning.current.day == "2026-11-21" && morning.current.net == 0 && morning.next == nil)
        // Two days later with no fetch in between: the blank day is that day's.
        #expect(c.timeline(at: amsterdam("2026-11-23 09:00"), rolloverHour: 5).current.day == "2026-11-23")
        // Midnight rollover: the flip is at 00:00 and 00:30 is already the new day.
        let mid = c.timeline(at: amsterdam("2026-11-20 23:30"), rolloverHour: 0)
        #expect(mid.next?.at == amsterdam("2026-11-21 00:00"))
        #expect(c.timeline(at: amsterdam("2026-11-21 00:30"), rolloverHour: 0).current.day == "2026-11-21")
    }

    @Test func theBlankDayKeepsThePositionsAndMovesTheResultIntoTheStrip() {
        let c = friday
        let r = c.rolled(to: "2026-11-21")
        #expect(r.day == "2026-11-21" && r.net == 0 && r.trades == 0 && r.wins == 0 && r.losses == 0 && r.unseen == 0)
        #expect(r.decisions == 0 && r.setWins == 0 && r.setLosses == 0)
        #expect(!r.hasPlan && r.maxTrades == 0 && r.tradesUsed == 0 && r.lossUsed == 0 && r.maxLoss == 0)
        #expect(r.openCount == c.openCount && r.unrealized == c.unrealized && r.stale, "positions carry over, their unrealised is old")
        #expect(r.days.last?.date == "2026-11-20" && r.days.last?.net == 640 && r.days.count == 3)
        #expect(abs(r.weekNet - (100 - 50 + 640)) < 0.01, "the week now includes Friday")
        #expect(c.rolled(to: "2026-11-20") == c && c.rolled(to: "2026-11-19") == c, "never backwards")
        var quiet = c
        quiet.net = 0; quiet.trades = 0
        #expect(quiet.rolled(to: "2026-11-21").days == c.days, "a day without trades adds no cell")
    }

    @Test func aCacheFetchedAfterMidnightStays() {
        var sat = friday
        sat.day = "2026-11-21"; sat.net = 0; sat.trades = 0
        // 02:00 Saturday with a 05:00 rollover: the widget's day is still Friday, but the cache
        // is Saturday's already (the app ran); show it, and there is nothing to flip to.
        let t = sat.timeline(at: amsterdam("2026-11-21 02:00"), rolloverHour: 5)
        #expect(t.current == sat && t.next == nil && !sat.isBehind(at: amsterdam("2026-11-21 02:00")))
    }

    @Test func theHourRoundTripsThroughTheAppGroup() {
        let d = UserDefaults(suiteName: "tj.tests.widgetday")!
        d.removePersistentDomain(forName: "tj.tests.widgetday")
        #expect(WidgetDay.rolloverHour(defaults: d) == 5)
        WidgetDay.save(0, defaults: d)
        #expect(WidgetDay.rolloverHour(defaults: d) == 0)
        #expect(WidgetDay.label(0) == "Midnight" && WidgetDay.label(7) == "07:00")
    }
}
