// The app lock's decisions, pure: when a return to the foreground asks for Face ID, which grace
// periods Settings offers, and what the enrolled biometry is called. LockGate applies them.
import Foundation
import LocalAuthentication

/// How long the app may be away before it asks again.
enum LockGrace: Equatable, Sendable {
    case after(Duration)
    /// Once a day: the app asks again when the calendar day has changed, however long it was away.
    case untilTomorrow

    /// The stored form: seconds, with `LockPolicy.untilTomorrow` for the once-a-day choice.
    nonisolated init(seconds: Double) {
        self = seconds < 0 ? .untilTomorrow : .after(.seconds(seconds))
    }
}

enum LockPolicy {
    static let untilTomorrow: Double = -1
    /// What Settings > Privacy > Lock after offers, in seconds; 0 is at once.
    static let graces: [(label: String, seconds: Double)] = [
        ("Immediately", 0), ("30 seconds", 30), ("1 minute", 60), ("5 minutes", 300), ("15 minutes", 900), ("1 hour", 3600),
        ("24 hours", 86400), ("Until tomorrow", untilTomorrow),
    ]
    static let defaultGrace: Double = 60

    /// Whether the app locks as it becomes active. `since` is the time since the app went to the
    /// background and `sameDay` whether that was today; nil when it only went inactive (Control
    /// Center, a call, the Face ID sheet itself), which never locks. `launching` is the first
    /// activation of this process: with `onLaunch` it always asks; without, the gate passes the
    /// time since the last background it remembered, so a cold start counts as a return.
    nonisolated static func shouldLock(since: Duration?, sameDay: Bool, grace: LockGrace, enabled: Bool, onLaunch: Bool, launching: Bool) -> Bool {
        guard enabled else { return false }
        if launching, onLaunch { return true }
        guard let since else { return false }
        switch grace {
        case .after(let d): return since < .zero || since >= d // a clock that went backwards locks too
        case .untilTomorrow: return !sameDay
        }
    }

    nonisolated static func label(_ biometry: LABiometryType) -> String {
        switch biometry {
        case .faceID: "Face ID"
        case .touchID: "Touch ID"
        case .opticID: "Optic ID"
        case .none: "passcode"
        @unknown default: "passcode"
        }
    }

    nonisolated static func symbol(_ biometry: LABiometryType) -> String {
        switch biometry {
        case .faceID: "faceid"
        case .touchID: "touchid"
        case .opticID: "opticid"
        case .none: "lock.fill"
        @unknown default: "lock.fill"
        }
    }
}
