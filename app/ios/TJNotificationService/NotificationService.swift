// The notification service extension: before a closed-trade push shows, the trade's chart PNG
// from the journal rides along as its attachment, so the collapsed notification already shows
// the move. The key comes from the keychain group the app shares; nothing waits more than a few
// seconds, and anything that goes wrong shows the push as sent.
import UniformTypeIdentifiers
import UserNotifications

final class NotificationService: UNNotificationServiceExtension {
    private var delivery: Delivery?
    private var work: Task<Void, Never>?

    override func didReceive(_ request: UNNotificationRequest, withContentHandler contentHandler: @escaping (UNNotificationContent) -> Void) {
        guard let content = request.content.mutableCopy() as? UNMutableNotificationContent,
              let push = PushPayload(content.userInfo), push.kind == .trade, push.tradeId > 0,
              let api = TJAPI.fromKeychain() else {
            contentHandler(request.content)
            return
        }
        let delivery = Delivery(content: content, handler: contentHandler)
        self.delivery = delivery
        let tradeId = push.tradeId
        work = Task {
            let config = URLSessionConfiguration.ephemeral
            config.timeoutIntervalForRequest = 6
            let session = URLSession(configuration: config)
            if let data = try? await api.image("/api/v1/trades/\(tradeId)/chart.png", session: session), data.count > 0, data.count < 2_000_000,
               let attachment = NotificationService.attachment(data) {
                delivery.attach(attachment)
            }
            delivery.finish()
        }
    }

    override func serviceExtensionTimeWillExpire() {
        work?.cancel()
        delivery?.finish()
    }

    /// The PNG as an attachment: the system moves the file it is given.
    nonisolated static func attachment(_ data: Data) -> UNNotificationAttachment? {
        let url = FileManager.default.temporaryDirectory.appending(path: "\(UUID().uuidString).png")
        do {
            try data.write(to: url)
            return try UNNotificationAttachment(identifier: "chart", url: url, options: [UNNotificationAttachmentOptionsTypeHintKey: UTType.png.identifier])
        } catch {
            return nil
        }
    }
}

/// The content on its way out and the handler that takes it, called exactly once, from the
/// fetch when it is done or from the system when the time is up.
final class Delivery: @unchecked Sendable {
    private let lock = NSLock()
    private let content: UNMutableNotificationContent
    private var handler: ((UNNotificationContent) -> Void)?

    init(content: UNMutableNotificationContent, handler: @escaping (UNNotificationContent) -> Void) {
        self.content = content
        self.handler = handler
    }

    func attach(_ a: UNNotificationAttachment) {
        lock.lock()
        defer { lock.unlock() }
        guard handler != nil else { return } // already delivered: too late
        content.attachments = [a]
    }

    func finish() {
        lock.lock()
        let h = handler
        handler = nil
        let c = content
        lock.unlock()
        h?(c)
    }
}
