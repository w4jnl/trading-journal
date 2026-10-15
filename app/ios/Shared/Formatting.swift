// Pure helpers that turn the journal's numbers and times into text, shared by the app, the
// notification card and the widgets; no SwiftUI here, so they are tested in milliseconds.
import Foundation

enum Formatting {
    /// Money as the web app shows it: a sign always, two decimals, no currency; nil is "—".
    nonisolated static func money(_ v: Double?) -> String {
        guard let v else { return "—" }
        return String(format: "%+.2f", v == 0 ? 0 : v)
    }

    /// Money with no decimals and a sign, for the Island and small widgets.
    nonisolated static func moneyShort(_ v: Double?) -> String {
        guard let v else { return "—" }
        return String(format: "%+.0f", v == 0 ? 0 : v)
    }

    /// Money with thousands separators and no sign, for balances and equity.
    nonisolated static func balance(_ v: Double?) -> String {
        guard let v else { return "—" }
        let f = NumberFormatter()
        f.numberStyle = .decimal
        f.minimumFractionDigits = 2
        f.maximumFractionDigits = 2
        return f.string(from: NSNumber(value: v)) ?? String(format: "%.2f", v)
    }

    /// A price at the instrument's tick: 7766.25, 7766.5 → "7766.25", "7766.50".
    nonisolated static func price(_ v: Double?, tickSize: Double = 0.25) -> String {
        guard let v else { return "—" }
        let decimals = Self.decimals(tickSize)
        return String(format: "%.\(decimals)f", v)
    }

    nonisolated static func decimals(_ tickSize: Double) -> Int {
        guard tickSize > 0 else { return 0 }
        var d = 0
        var t = tickSize
        while d < 6, abs(t - t.rounded()) > 1e-9 {
            t *= 10
            d += 1
        }
        return d
    }

    /// "1.8R", "-0.5R"; nil is "—".
    nonisolated static func r(_ v: Double?) -> String {
        guard let v else { return "—" }
        return String(format: "%.1fR", v)
    }

    /// Ticks to a level: "76 ticks", "1 tick"; negative means through it.
    nonisolated static func ticks(_ v: Double?) -> String {
        guard let v else { return "—" }
        let n = Int(v.rounded())
        return "\(n) \(abs(n) == 1 ? "tick" : "ticks")"
    }

    /// A quantity: whole contracts without decimals.
    nonisolated static func qty(_ v: Double) -> String {
        v == v.rounded() ? String(Int(v)) : String(format: "%.2f", v)
    }

    /// A percentage from a ratio: 0.9173 → "92%".
    nonisolated static func percent(_ v: Double?) -> String {
        guard let v else { return "—" }
        return String(format: "%.0f%%", v * 100)
    }

    /// The money at risk from a stop, as the server computes it: |avgEntry − stop| × pointValue × qty.
    nonisolated static func risk(avgEntry: Double, stop: Double, qty: Double, pointValue: Double) -> Double {
        abs(avgEntry - stop) * pointValue * qty
    }

    // MARK: time

    /// A wall clock in the trading timezone: "14:48".
    nonisolated static func clock(_ d: Date?, tz: TimeZone) -> String {
        guard let d else { return "—" }
        let f = DateFormatter()
        f.timeZone = tz
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "HH:mm"
        return f.string(from: d)
    }

    /// The trading day of a moment: "2026-10-15".
    nonisolated static func day(_ d: Date, tz: TimeZone) -> String {
        let f = DateFormatter()
        f.timeZone = tz
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd"
        return f.string(from: d)
    }

    /// "2026-10-15" → a Date at the trading day's midnight, nil for anything else.
    nonisolated static func parseDay(_ s: String, tz: TimeZone) -> Date? {
        let f = DateFormatter()
        f.timeZone = tz
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd"
        return f.date(from: s)
    }

    /// "Wed 14 Oct" for a trading day string; the string itself when it is not a day.
    nonisolated static func dayLabel(_ s: String, tz: TimeZone, style: DayLabelStyle = .short) -> String {
        guard let d = parseDay(s, tz: tz) else { return s }
        let f = DateFormatter()
        f.timeZone = tz
        f.locale = Locale.current
        switch style {
        case .short: f.setLocalizedDateFormatFromTemplate("EEE d MMM")
        case .long: f.setLocalizedDateFormatFromTemplate("EEEE d MMMM yyyy")
        case .month: f.setLocalizedDateFormatFromTemplate("MMMM yyyy")
        }
        return f.string(from: d)
    }

    enum DayLabelStyle { case short, long, month }

    /// "no news since 09:12" style ages: seconds → "12 s", "4 min", "2 h".
    nonisolated static func age(_ since: Date, now: Date = .now) -> String {
        let s = max(0, Int(now.timeIntervalSince(since)))
        if s < 60 { return "\(s) s" }
        if s < 3600 { return "\(s / 60) min" }
        if s < 86400 { return "\(s / 3600) h" }
        return "\(s / 86400) d"
    }
}
