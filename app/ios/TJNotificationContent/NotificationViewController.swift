// The notification content extension: the expanded view of a journal push (long press) is the
// NotificationCard, with the live numbers fetched from the journal when it opens. The server's
// address and the key come from the keychain group the app shares. Seen answers in place; Open
// and the system's actions are forwarded to the app.
import SwiftUI
import UIKit
import UserNotifications
import UserNotificationsUI

@MainActor
@Observable
final class CardModel {
    var push: PushPayload?
    var title = ""
    var subtitle = ""
    var body = ""
    var data: CardData = .loading
    var status: String?
    var tz: TimeZone = .current
    private var api: TJAPI? { TJAPI.fromKeychain() }

    func load() async {
        guard let push else { return }
        guard let api else {
            data = .unavailable("open the app once to show the numbers here")
            return
        }
        do {
            switch push.kind {
            case .trade where push.tradeId > 0:
                data = .trade(try await api.get(TradeDetail.self, "/trades/\(push.tradeId)"))
            case .nudge:
                let today = try await api.get(Today.self, "/today")
                tz = today.tz
                if let (a, s) = push.position, let p = today.positions.first(where: { $0.accountId == a && $0.symbol == s }) {
                    data = .position(p)
                } else {
                    data = .today(net: today.stats.net, open: today.positions.isEmpty ? nil : today.unrealized.total, stale: today.unrealized.stale)
                }
            default:
                data = .unavailable("")
            }
        } catch {
            data = .unavailable(error.localizedDescription)
        }
    }

    func seen() async {
        guard let push, push.id > 0 else { return }
        guard let api else {
            status = "open the app once to answer from here"
            return
        }
        do {
            try await api.send("POST", "/nudges/\(push.id)/seen")
            status = "seen"
        } catch {
            status = error.localizedDescription
        }
    }
}

final class NotificationViewController: UIViewController, @preconcurrency UNNotificationContentExtension {
    private let model = CardModel()
    private var host: UIHostingController<CardHost>?
    private var openHandler: (() -> Void)?

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = UIColor(hex: 0x0A0A0B)
        let host = UIHostingController(rootView: CardHost(model: model, onOpen: { [weak self] in self?.openHandler?() }))
        host.view.backgroundColor = .clear
        host.sizingOptions = .preferredContentSize
        addChild(host)
        host.view.translatesAutoresizingMaskIntoConstraints = false
        view.addSubview(host.view)
        NSLayoutConstraint.activate([
            host.view.leadingAnchor.constraint(equalTo: view.leadingAnchor),
            host.view.trailingAnchor.constraint(equalTo: view.trailingAnchor),
            host.view.topAnchor.constraint(equalTo: view.topAnchor),
            host.view.bottomAnchor.constraint(equalTo: view.bottomAnchor),
        ])
        host.didMove(toParent: self)
        self.host = host
    }

    override func preferredContentSizeDidChange(forChildContentContainer container: any UIContentContainer) {
        super.preferredContentSizeDidChange(forChildContentContainer: container)
        preferredContentSize = CGSize(width: view.bounds.width, height: container.preferredContentSize.height)
    }

    func didReceive(_ notification: UNNotification) {
        let c = notification.request.content
        model.push = PushPayload(c.userInfo)
        model.title = c.title
        model.subtitle = c.subtitle
        model.body = c.body
        model.data = .loading
        model.status = nil
        // Open from the card: the extension cannot launch the app itself, but the system's
        // default action does; the card asks to be dismissed and forwarded.
        openHandler = { [weak self] in self?.extensionContext?.performNotificationDefaultAction() }
        Task { await model.load() }
    }

    func didReceive(_ response: UNNotificationResponse, completionHandler completion: @escaping (UNNotificationContentExtensionResponseOption) -> Void) {
        completion(.dismissAndForwardAction) // the app records Seen, appends the note, opens the screen
    }
}

struct CardHost: View {
    let model: CardModel
    var onOpen: () -> Void

    var body: some View {
        if let push = model.push {
            NotificationCard(push: push, title: model.title, subtitle: model.subtitle, body_: model.body, data: model.data, status: model.status, tz: model.tz,
                             onSeen: { Task { await model.seen() } }, onOpen: onOpen)
        }
    }
}
