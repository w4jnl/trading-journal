// Trading Journal for iPhone and iPad: today's P&L and open positions live, the trade log, the
// journal, the calendar, the guardrail's nudges. Talks to the journal's HTTP API only (API.md).
import SwiftUI
import UIKit

@main
struct TradingJournalApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @Environment(\.scenePhase) private var phase
    @State private var lock = LockGate()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(Client.shared)
                .environment(Settings.shared)
                .environment(lock)
                .onAppear { lock.scene(.active, settings: Settings.shared) } // a cold start locks before the first frame
        }
        .onChange(of: phase) { _, now in
            lock.scene(now, settings: Settings.shared)
            switch now {
            case .active:
                Client.shared.start()
                Task { await Notifications.shared.refreshAuthorization() }
                LiveActivities.shared.reportSoon()
            case .background:
                Client.shared.stop()
            default:
                break
            }
        }
    }
}

final class AppDelegate: NSObject, UIApplicationDelegate {
    func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        // The delegate is set before launching finishes, or a cold-start tap on a notification is lost.
        Notifications.shared.setup(client: Client.shared)
        LiveActivities.shared.setup(client: Client.shared)
        return true
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        Notifications.shared.didRegister(deviceToken: deviceToken)
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        Notifications.shared.didFail(error)
    }

    /// The server's silent sync push after every change: the alerts of nudges seen elsewhere
    /// come off the Lock Screen, the badge follows, Today is fetched, without a sound.
    func application(_ application: UIApplication, didReceiveRemoteNotification userInfo: [AnyHashable: Any],
                     fetchCompletionHandler completionHandler: @escaping (UIBackgroundFetchResult) -> Void) {
        guard let push = PushPayload(userInfo), push.kind == .sync else {
            completionHandler(.noData)
            return
        }
        let done = MainThreadCallback { completionHandler(.newData) }
        Task { @MainActor in
            await Notifications.shared.sync(push)
            done.call()
        }
    }
}
