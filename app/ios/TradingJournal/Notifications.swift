// Pushes: the categories with their Lock Screen actions, the APNs token handed to the server,
// what a tapped action does, and the server's silent sync. Seen marks a nudge seen through the
// HTTP API without opening the app; Note… appends to the trade's journal from the Lock Screen;
// a plain tap opens the screen the push is about. The expanded card is the content extension's.
import Foundation
import Observation
import UIKit
import UserNotifications

enum NotificationAction: String, CaseIterable {
    case seen = "SEEN"
    case open = "OPEN"
    case note = "NOTE"
}

extension PushCategory {
    var actions: [UNNotificationAction] {
        let open = UNNotificationAction(identifier: NotificationAction.open.rawValue, title: "Open", options: [.foreground], icon: UNNotificationActionIcon(systemImageName: "arrow.up.right.square"))
        let seen = UNNotificationAction(identifier: NotificationAction.seen.rawValue, title: "Seen", options: [], icon: UNNotificationActionIcon(systemImageName: "eye"))
        // Note… opens the system's text field on the Lock Screen (with dictation) and appends
        // the words to the trade's journal note, creating it when there is none yet.
        let note = UNTextInputNotificationAction(identifier: NotificationAction.note.rawValue, title: "Note…", options: [.authenticationRequired],
                                                 icon: UNNotificationActionIcon(systemImageName: "square.and.pencil"),
                                                 textInputButtonTitle: "Add", textInputPlaceholder: "a line for the journal")
        switch self {
        case .nudge: return [seen, open]
        case .position: return [seen, open]
        case .trade: return [note, open]
        }
    }

    var category: UNNotificationCategory {
        switch self {
        case .nudge, .position:
            UNNotificationCategory(identifier: rawValue, actions: actions, intentIdentifiers: [], hiddenPreviewsBodyPlaceholder: "%u nudges", options: [.customDismissAction])
        case .trade:
            UNNotificationCategory(identifier: rawValue, actions: actions, intentIdentifiers: [], hiddenPreviewsBodyPlaceholder: "%u trades closed", options: [])
        }
    }

    static var all: Set<UNNotificationCategory> { Set(allCases.map(\.category)) }
}

@MainActor
@Observable
final class Notifications: NSObject, UNUserNotificationCenterDelegate {
    static let shared = Notifications()

    var client: Client?
    private(set) var authorization: UNAuthorizationStatus = .notDetermined
    private(set) var apnsToken: String?
    /// What the server said at the last registration: whether it can push at all.
    private(set) var serverCanPush: Bool?
    private(set) var lastError: String?
    /// The last failure to hand the settings to the server; nil once they got there.
    private(set) var prefsError: String?
    private var prefsSync: Task<Void, Never>?

    func setup(client: Client) {
        self.client = client
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.setNotificationCategories(PushCategory.all)
        Task { await refreshAuthorization() }
    }

    func refreshAuthorization() async {
        authorization = await UNUserNotificationCenter.current().notificationSettings().authorizationStatus
        if authorization == .authorized || authorization == .provisional {
            UIApplication.shared.registerForRemoteNotifications()
        }
    }

    /// Asks once; then registers with APNs, whose token goes to the server.
    func enable() async {
        _ = try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])
        await refreshAuthorization()
    }

    func didRegister(deviceToken: Data) {
        apnsToken = deviceToken.map { String(format: "%02x", $0) }.joined()
        lastError = nil
        Task { await registerIfNeeded() }
    }

    func didFail(_ error: Error) {
        lastError = error.localizedDescription
    }

    /// Hands the APNs token to the server once per server and token.
    func registerIfNeeded() async {
        guard let client, let apnsToken, client.settings.isPaired else { return }
        if client.settings.registered == "\(client.settings.serverURL)|\(apnsToken)" { return }
        do {
            serverCanPush = try await client.registerDevice(apnsToken: apnsToken)
            lastError = nil
            prefsError = nil
        } catch {
            lastError = "registering with the journal: \(error.localizedDescription)"
        }
    }

    var isRegistered: Bool {
        guard let client, let apnsToken else { return false }
        return client.settings.registered == "\(client.settings.serverURL)|\(apnsToken)"
    }

    // MARK: settings

    /// Changes the settings and hands them to the server a moment later (a run of toggles is
    /// one request).
    func updatePrefs(_ change: (inout DevicePrefs) -> Void) {
        guard let client else { return }
        change(&client.settings.notificationPrefs)
        prefsSync?.cancel()
        prefsSync = Task {
            try? await Task.sleep(for: .milliseconds(600))
            guard !Task.isCancelled else { return }
            await syncPrefs()
        }
    }

    func syncPrefs() async {
        guard let client else { return }
        do {
            try await client.putPrefs()
            prefsError = nil
        } catch {
            prefsError = error.localizedDescription
            if client.settings.registered == nil { await registerIfNeeded() }
        }
    }

    // MARK: delegate

    // The completion-handler forms on purpose: UIKit asserts that the handler runs on the main
    // thread, and the async forms of these methods call it from the concurrency pool.

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                            withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        let push = PushPayload(notification.request.content.userInfo)
        let done = MainThreadCallback { completionHandler(Notifications.presentation(for: push)) }
        Task { @MainActor in done.call() }
    }

    /// A push while the app is open: a nudge shows in full; a closed trade only lands in the
    /// list, Today already shows it.
    nonisolated static func presentation(for push: PushPayload?) -> UNNotificationPresentationOptions {
        switch push?.kind {
        case .trade: [.list, .badge]
        case .sync: []
        default: [.banner, .sound, .badge, .list]
        }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping () -> Void) {
        let push = PushPayload(response.notification.request.content.userInfo)
        let action = response.actionIdentifier
        let text = (response as? UNTextInputNotificationResponse)?.userText
        let done = MainThreadCallback(completionHandler)
        Task { @MainActor in
            // An action from the Lock Screen runs in the background: iOS keeps the app alive
            // until the handler is called, so it is called once the call went out.
            await Notifications.shared.perform(action: action, push: push, text: text)
            done.call()
        }
    }

    func perform(action: String, push: PushPayload?, text: String? = nil) async {
        guard let push, let client else { return }
        switch action {
        case NotificationAction.seen.rawValue, UNNotificationDismissActionIdentifier:
            if push.kind == .nudge, push.id > 0 { try? await client.markSeen(push.id) }
        case NotificationAction.note.rawValue:
            let words = text?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
            guard push.kind == .trade, push.tradeId > 0, !words.isEmpty else { return }
            do {
                _ = try await client.createJournal(JournalCreate(scope: .trade, body: words, tradeId: push.tradeId))
            } catch Client.JournalSaveError.exists(let id) {
                try? await client.appendNote(id, words)
            } catch {
                lastError = error.localizedDescription
            }
        default: // the notification itself, or Open
            if push.kind == .nudge, push.id > 0 { try? await client.markSeen(push.id) }
            client.pendingRoute = push.destination
        }
    }

    // MARK: sync

    /// Removes the delivered nudge notifications whose id is not in `unseen`: from Today while
    /// the app runs, from the server's silent sync push while it does not.
    nonisolated static func clearDelivered(unseen: Set<Int>) async {
        let center = UNUserNotificationCenter.current()
        let gone: [String] = await withCheckedContinuation { done in
            center.getDeliveredNotifications { list in
                done.resume(returning: list.compactMap { n in
                    guard let p = PushPayload(n.request.content.userInfo), p.kind == .nudge, p.id > 0 else { return nil }
                    return unseen.contains(p.id) ? nil : n.request.identifier
                })
            }
        }
        if !gone.isEmpty { center.removeDeliveredNotifications(withIdentifiers: gone) }
    }

    /// The server's silent sync push: what still waits, the badge, and a fresh Today.
    func sync(_ push: PushPayload) async {
        await Notifications.clearDelivered(unseen: Set(push.unseen))
        if let badge = push.badge { try? await UNUserNotificationCenter.current().setBadgeCount(badge) }
        await client?.refreshToday()
    }
}

/// Carries a UIKit completion handler into a main-actor task; it is only ever called there.
struct MainThreadCallback: @unchecked Sendable {
    let body: () -> Void
    init(_ body: @escaping () -> Void) { self.body = body }
    @MainActor func call() { body() }
}
