// The app lock's pure decisions: when a return to the foreground asks, what Settings offers.
import Foundation
import LocalAuthentication
import Testing
@testable import TradingJournal

struct LockTests {
    private func lock(since: Duration?, sameDay: Bool = true, grace: LockGrace = .after(.seconds(60)), enabled: Bool = true, onLaunch: Bool = true, launching: Bool = false) -> Bool {
        LockPolicy.shouldLock(since: since, sameDay: sameDay, grace: grace, enabled: enabled, onLaunch: onLaunch, launching: launching)
    }

    @Test func offMeansNeverLocked() {
        #expect(!lock(since: .seconds(3600), grace: .after(.zero), enabled: false, launching: true))
        #expect(!lock(since: nil, grace: .after(.zero), enabled: false))
        #expect(!lock(since: .seconds(90000), sameDay: false, grace: .untilTomorrow, enabled: false))
    }

    @Test func aColdStartFollowsLockOnLaunch() {
        #expect(lock(since: nil, launching: true))
        #expect(lock(since: .seconds(1), launching: true), "Lock on launch never trusts a grace")
        #expect(!lock(since: nil, onLaunch: false, launching: true), "nothing remembered: nothing to measure")
        #expect(!lock(since: .seconds(30), onLaunch: false, launching: true), "a remembered background inside the grace: a cold start is a return")
        #expect(lock(since: .seconds(61), onLaunch: false, launching: true))
    }

    @Test func theGraceIsMeasuredFromTheBackground() {
        #expect(!lock(since: .seconds(59)))
        #expect(lock(since: .seconds(60)), "the boundary locks")
        #expect(lock(since: .seconds(3600)))
        #expect(lock(since: .zero, grace: .after(.zero), onLaunch: false), "Immediately is grace zero")
        #expect(lock(since: .seconds(-5)), "a clock that went backwards locks rather than trusts")
        #expect(!lock(since: .seconds(86399), grace: .after(.seconds(86400))) && lock(since: .seconds(86400), grace: .after(.seconds(86400))))
    }

    @Test func untilTomorrowAsksOnceADay() {
        #expect(!lock(since: .seconds(8 * 3600), sameDay: true, grace: .untilTomorrow), "away all afternoon, still today: no prompt")
        #expect(lock(since: .seconds(60), sameDay: false, grace: .untilTomorrow), "a minute across midnight: the day changed")
        #expect(lock(since: .seconds(3 * 86400), sameDay: false, grace: .untilTomorrow))
        #expect(!lock(since: nil, sameDay: false, grace: .untilTomorrow), "inactive never locks, whatever the day")
        #expect(LockGrace(seconds: LockPolicy.untilTomorrow) == .untilTomorrow && LockGrace(seconds: 300) == .after(.seconds(300)))
    }

    @Test func goingInactiveNeverLocks() {
        #expect(!lock(since: nil, grace: .after(.zero)), "Control Center, a call, the Face ID sheet: no background, no lock")
    }

    @Test func gracesAreOrderedAndHoldTheDefault() {
        let s = LockPolicy.graces.map(\.seconds)
        let timed = s.filter { $0 >= 0 }
        #expect(timed == timed.sorted() && timed.first == 0 && timed.last == 86400)
        #expect(s.last == LockPolicy.untilTomorrow, "the once-a-day choice closes the list")
        #expect(s.contains(LockPolicy.defaultGrace))
        #expect(Set(s).count == s.count)
    }

    @Test func biometryNames() {
        #expect(LockPolicy.label(.faceID) == "Face ID" && LockPolicy.symbol(.faceID) == "faceid")
        #expect(LockPolicy.label(.touchID) == "Touch ID" && LockPolicy.symbol(.touchID) == "touchid")
        #expect(LockPolicy.label(.none) == "passcode" && LockPolicy.symbol(.none) == "lock.fill")
    }

    @MainActor @Test func lockSettingsDefaultOffAndKeepAChoice() {
        let d = UserDefaults.standard
        let keys = ["lockEnabled", "lockGraceSeconds", "lockOnLaunch"]
        let before = keys.map { ($0, d.object(forKey: $0)) }
        defer { for (k, v) in before { if let v { d.set(v, forKey: k) } else { d.removeObject(forKey: k) } } }
        for k in keys { d.removeObject(forKey: k) }
        let fresh = Settings()
        #expect(!fresh.lockEnabled, "a fresh install asks for nothing")
        #expect(fresh.lockGraceSeconds == 60 && fresh.lockGrace == .after(.seconds(60)))
        #expect(fresh.lockOnLaunch)
        fresh.lockEnabled = true
        fresh.lockGraceSeconds = 300
        fresh.lockOnLaunch = false
        let next = Settings()
        #expect(next.lockEnabled && next.lockGraceSeconds == 300 && !next.lockOnLaunch)
    }

    @Test func hideAmountsReadsTheAppGroupFlag() {
        let d = UserDefaults(suiteName: "nl.w4j.tradingjournal.tests.privacy")!
        d.removePersistentDomain(forName: "nl.w4j.tradingjournal.tests.privacy")
        #expect(!Privacy.hideAmounts(defaults: d), "off by default: the Lock Screen shows the figures")
        d.set(true, forKey: Privacy.key)
        #expect(Privacy.hideAmounts(defaults: d))
    }
}
