// Whether money is hidden on the Lock Screen until the phone is unlocked: one flag in the App
// Group the app writes (Settings > Privacy) and the widgets, the Live Activity and the
// notification card read when they render. It rides on `.privacySensitive`, so the phone's own
// Face ID resolves it; the app lock (LockGate) is a separate thing for the app itself.
import SwiftUI

enum Privacy {
    static let key = "hideAmountsLocked"

    nonisolated static func hideAmounts(defaults: UserDefaults? = UserDefaults(suiteName: TodayCache.suite)) -> Bool {
        defaults?.object(forKey: key) as? Bool ?? false
    }
}

extension View {
    /// An amount of money: redacted while the phone is locked when the user asked for that.
    func amount() -> some View { privacySensitive(Privacy.hideAmounts()) }
}
