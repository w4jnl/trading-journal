// The app lock's pure decisions: when a return to the foreground asks, what Settings offers.
import Foundation
import LocalAuthentication
import Testing
@testable import TradingJournal

struct LockTests {
    @Test func offMeansNeverLocked() {
        #expect(!LockPolicy.shouldLock(since: .seconds(3600), grace: .zero, enabled: false, onLaunch: true, launching: true))
        #expect(!LockPolicy.shouldLock(since: nil, grace: .zero, enabled: false, onLaunch: true, launching: false))
    }

    @Test func aColdStartFollowsLockOnLaunch() {
        #expect(LockPolicy.shouldLock(since: nil, grace: .seconds(60), enabled: true, onLaunch: true, launching: true))
        #expect(!LockPolicy.shouldLock(since: nil, grace: .seconds(60), enabled: true, onLaunch: false, launching: true))
        #expect(LockPolicy.shouldLock(since: .seconds(1), grace: .seconds(60), enabled: true, onLaunch: true, launching: true), "the first activation never trusts a grace")
    }

    @Test func theGraceIsMeasuredFromTheBackground() {
        let grace = Duration.seconds(60)
        #expect(!LockPolicy.shouldLock(since: .seconds(59), grace: grace, enabled: true, onLaunch: true, launching: false))
        #expect(LockPolicy.shouldLock(since: .seconds(60), grace: grace, enabled: true, onLaunch: true, launching: false), "the boundary locks")
        #expect(LockPolicy.shouldLock(since: .seconds(3600), grace: grace, enabled: true, onLaunch: true, launching: false))
        #expect(LockPolicy.shouldLock(since: .zero, grace: .zero, enabled: true, onLaunch: false, launching: false), "Immediately is grace zero")
    }

    @Test func goingInactiveNeverLocks() {
        #expect(!LockPolicy.shouldLock(since: nil, grace: .zero, enabled: true, onLaunch: true, launching: false), "Control Center, a call, the Face ID sheet: no background, no lock")
    }

    @Test func gracesAreOrderedAndHoldTheDefault() {
        let s = LockPolicy.graces.map(\.seconds)
        #expect(s == s.sorted() && s.first == 0)
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
        #expect(fresh.lockGraceSeconds == 60 && fresh.lockGrace == .seconds(60))
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
