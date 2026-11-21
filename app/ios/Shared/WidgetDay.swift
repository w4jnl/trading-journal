// When the Today widgets start a new day. The widgets never call the server and the silent sync
// only arrives on a change, so on a quiet night the cache would show yesterday's result as
// today's until the app next ran. The server counts days by the journal's trading timezone and
// flips at its midnight; the widgets flip later, at an hour the trader picks (05:00 unless
// changed), keep the last day's figure until then marked with its weekday, and carry the open
// positions over. The timeline entry for the flip is dated in advance, so WidgetKit shows the
// blank day at that minute with no app, push or network involved; a fill after midnight brings
// the real new day through the sync as before.
import Foundation

enum WidgetDay {
    static let key = "widgetRolloverHour"
    static let defaultHour = 5
    /// The hours Settings offers, in the journal's trading timezone.
    static let hours = [0, 3, 5, 7]

    static func label(_ hour: Int) -> String { hour == 0 ? "Midnight" : String(format: "%02d:00", hour) }

    /// The rollover hour as the app last saved it into the App Group; the default before a save.
    nonisolated static func rolloverHour(defaults: UserDefaults? = UserDefaults(suiteName: TodayCache.suite)) -> Int {
        defaults?.object(forKey: key) as? Int ?? defaultHour
    }

    nonisolated static func save(_ hour: Int, defaults: UserDefaults? = UserDefaults(suiteName: TodayCache.suite)) {
        defaults?.set(hour, forKey: key)
    }
}

extension TodayCache {
    /// The widgets' timeline from this cache: what to show now, and the blank day with the minute
    /// it begins when that minute is still ahead (nil once the cache itself is the current day's
    /// or later, as it is after a fetch past midnight).
    func timeline(at now: Date, rolloverHour: Int) -> (current: TodayCache, next: (at: Date, day: TodayCache)?) {
        let target = widgetDay(at: now, rolloverHour: rolloverHour)
        if target > day { return (rolled(to: target), nil) }
        let at = nextRollover(after: now, rolloverHour: rolloverHour)
        let rolled = rolled(to: Formatting.day(at, tz: timeZone))
        return (self, rolled.day > day ? (at, rolled) : nil)
    }

    /// The day the widget should show at `now`: the calendar day in the trading timezone that
    /// began at the rollover hour, so 02:00 with a 05:00 rollover still belongs to yesterday.
    func widgetDay(at now: Date, rolloverHour: Int) -> String {
        Formatting.day(calendar.date(byAdding: .hour, value: -rolloverHour, to: now) ?? now, tz: timeZone)
    }

    /// The first rollover moment after `now`.
    func nextRollover(after now: Date, rolloverHour: Int) -> Date {
        let shown = Formatting.parseDay(widgetDay(at: now, rolloverHour: rolloverHour), tz: timeZone) ?? now
        let tomorrow = calendar.date(byAdding: .day, value: 1, to: calendar.startOfDay(for: shown)) ?? now
        return calendar.date(bySettingHour: rolloverHour, minute: 0, second: 0, of: tomorrow) ?? tomorrow
    }

    /// Whether this cache is from before the calendar day at `now`: the figure then carries its
    /// weekday so it is not read as today's.
    func isBehind(at now: Date) -> Bool { day < Formatting.day(now, tz: timeZone) }

    /// "Fri" for the cached day.
    var weekday: String {
        guard let d = Formatting.parseDay(day, tz: timeZone) else { return day }
        let f = DateFormatter()
        f.timeZone = timeZone
        f.locale = Locale.current
        f.setLocalizedDateFormatFromTemplate("EEE")
        return f.string(from: d)
    }

    /// The cache as the blank day `newDay`: no trades, no result, no plan, no nudges; the day it
    /// replaces moves into the strip of last days; the open positions stay, their unrealised
    /// marked stale. Itself when `newDay` is not after the cached day.
    func rolled(to newDay: String) -> TodayCache {
        guard newDay > day else { return self }
        var r = self
        r.day = newDay
        if trades > 0 || net != 0 {
            r.days = Array((days.filter { $0.date != day } + [DayCell(date: day, net: net, trades: trades)]).suffix(10))
        }
        r.net = 0
        r.trades = 0; r.wins = 0; r.losses = 0
        r.decisions = 0; r.setWins = 0; r.setLosses = 0
        r.hasPlan = false; r.maxTrades = 0; r.tradesUsed = 0; r.lossUsed = 0; r.maxLoss = 0
        r.unseen = 0
        r.stale = stale || openCount > 0
        return r
    }

    private var calendar: Calendar {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = timeZone
        return c
    }
}
