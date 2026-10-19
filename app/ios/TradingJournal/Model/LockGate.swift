// The app lock: Face ID (the passcode as fallback) before the app shows anything, after it has
// been away longer than the grace the user chose. A UI gate only; the device key and the server
// are untouched, and the client keeps streaming behind the cover so unlocking shows the present.
// The cover is a second window above the alert level, so sheets and editors hide under it too.
import LocalAuthentication
import SwiftUI
import UIKit

@MainActor
@Observable
final class LockGate {
    /// Nothing shows until the owner authenticates.
    private(set) var locked = false
    /// The content is hidden (app switcher, a call) without asking for anything.
    private(set) var covered = false
    private(set) var authenticating = false
    private var backgroundedAt: ContinuousClock.Instant?
    private var backgroundedOn: Date?
    private var activatedOnce = false
    /// The wall clock of the last background, kept for the next process: a cold start without
    /// "Lock on launch" measures the grace from it.
    private static let rememberedKey = "lockBackgroundedAt"
    private var window: UIWindow?

    /// The enrolled biometry, for labels; Face ID on most phones.
    var biometry: LABiometryType {
        let c = LAContext()
        _ = c.canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: nil)
        return c.biometryType
    }

    /// False on a phone without a passcode: there is nothing to ask, so the lock cannot be turned on.
    var canAuthenticate: Bool { LAContext().canEvaluatePolicy(.deviceOwnerAuthentication, error: nil) }

    /// The scene's phase changed; the lock follows the user's settings.
    func scene(_ phase: ScenePhase, settings: Settings) {
        switch phase {
        case .background:
            backgroundedAt = .now
            backgroundedOn = .now
            UserDefaults.standard.set(backgroundedOn, forKey: Self.rememberedKey)
            covered = settings.lockEnabled
        case .inactive:
            covered = settings.lockEnabled // the switcher's thumbnail; never a prompt from here
        case .active:
            var since: Duration?
            var sameDay = true
            if let backgroundedAt, let backgroundedOn {
                since = ContinuousClock.now - backgroundedAt
                sameDay = Calendar.current.isDateInToday(backgroundedOn)
            } else if !activatedOnce, let remembered = UserDefaults.standard.object(forKey: Self.rememberedKey) as? Date {
                since = .seconds(Date.now.timeIntervalSince(remembered))
                sameDay = Calendar.current.isDateInToday(remembered)
            }
            if LockPolicy.shouldLock(since: since, sameDay: sameDay, grace: settings.lockGrace, enabled: settings.lockEnabled,
                                     onLaunch: settings.lockOnLaunch, launching: !activatedOnce) {
                locked = true
            }
            activatedOnce = true
            backgroundedAt = nil
            backgroundedOn = nil
            covered = false
            if locked, !authenticating { Task { await unlock() } }
        @unknown default:
            break
        }
        show(locked || covered)
    }

    /// Asks the owner; on success the cover comes down.
    func unlock() async {
        guard !authenticating else { return }
        authenticating = true
        defer { authenticating = false }
        if await authenticate(reason: "Unlock the journal.") {
            locked = false
            show(covered)
        }
    }

    /// Face ID, Touch ID or the passcode, whichever the phone has; true when it has none.
    func authenticate(reason: String) async -> Bool {
        let ctx = LAContext()
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: nil) else { return true }
        return await withCheckedContinuation { k in
            ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason) { ok, _ in k.resume(returning: ok) }
        }
    }

    private func show(_ on: Bool) {
        if on {
            guard window == nil else { return }
            let scenes = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
            guard let scene = scenes.first(where: { $0.activationState == .foregroundActive || $0.activationState == .foregroundInactive }) ?? scenes.first else { return }
            let w = UIWindow(windowScene: scene)
            w.windowLevel = .alert + 1
            w.rootViewController = UIHostingController(rootView: LockScreen(gate: self))
            w.isHidden = false
            window = w
        } else {
            window?.isHidden = true
            window = nil
        }
    }
}

/// What the cover shows: the brand, the biometry's glyph, Unlock when locked. No figures.
struct LockScreen: View {
    let gate: LockGate

    var body: some View {
        ZStack(alignment: .top) {
            Brand.bg.ignoresSafeArea()
            Rectangle().fill(Brand.accent).frame(height: 4).ignoresSafeArea(edges: .top)
            VStack(spacing: 20) {
                Spacer()
                Image(systemName: gate.locked ? LockPolicy.symbol(gate.biometry) : "lock.fill")
                    .font(.system(size: 44, weight: .light))
                    .foregroundStyle(Brand.accent)
                    .accessibilityHidden(true)
                Text("Trade Log").font(Brand.mono(22, bold: true)).foregroundStyle(Brand.fg)
                if gate.locked {
                    Button {
                        Task { await gate.unlock() }
                    } label: {
                        Text(gate.authenticating ? "Unlocking…" : "Unlock").font(Brand.text(16, weight: .semibold)).frame(minWidth: 160)
                    }
                    .buttonStyle(.borderedProminent)
                    .tint(Brand.accent)
                    .disabled(gate.authenticating)
                    .padding(.top, 8)
                }
                Spacer()
                Spacer()
            }
        }
    }
}
