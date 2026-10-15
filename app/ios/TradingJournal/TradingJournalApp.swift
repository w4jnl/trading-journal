// Trading Journal for iPhone and iPad: today's P&L and open positions live, the trade log, the
// journal, the calendar, the guardrail's nudges. Talks to the journal's HTTP API only (API.md).
import SwiftUI
import UIKit

@main
struct TradingJournalApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @Environment(\.scenePhase) private var phase

    var body: some Scene {
        WindowGroup {
            RootView()
                .environment(Client.shared)
                .environment(Settings.shared)
        }
        .onChange(of: phase) { _, now in
            switch now {
            case .active:
                Client.shared.start()
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
        true
    }
}
