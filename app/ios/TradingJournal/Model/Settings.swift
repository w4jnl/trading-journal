// What the app remembers: the journal's address and this phone's key (keychain, shared with the
// extensions), what the server told us about itself, the push registration and a few choices.
// Every property persists in didSet. The simulator can be pointed at a journal without touching
// the UI: `simctl launch … nl.w4j.tradingjournal -serverURL http://… -apiKey tj_…` (launch
// arguments land in UserDefaults and win over the stored values for that run).
import Foundation
import Observation

@MainActor
@Observable
final class Settings {
    static let shared = Settings()

    var serverURL: String { didSet { defaults.set(serverURL, forKey: "serverURL"); Keychain.set(serverURL, for: "serverURL") } }
    var apiKey: String { didSet { Keychain.set(apiKey, for: "apiKey") } }
    /// The server's facts from the last /me or pairing, so screens render before the network answers.
    var server: ServerInfo? { didSet { defaults.set(try? JSONEncoder().encode(server), forKey: "server") } }
    var user: UserInfo? { didSet { defaults.set(try? JSONEncoder().encode(user), forKey: "user") } }
    /// "<server>|<apns token>" of the last device registration, so it is not repeated at every start.
    var registered: String? { didSet { defaults.set(registered, forKey: "registered") } }
    /// Which trades view the user left: by trade or by set.
    var tradesBySet: Bool { didSet { defaults.set(tradesBySet, forKey: "tradesBySet") } }
    /// What this phone wants pushed; Notifications.updatePrefs changes it and tells the server.
    var notificationPrefs: DevicePrefs { didSet { defaults.set(try? JSONEncoder().encode(notificationPrefs), forKey: "notificationPrefs") } }

    private let defaults = UserDefaults.standard

    init() {
        let args = UserDefaults.standard.volatileDomain(forName: UserDefaults.argumentDomain)
        serverURL = (args["serverURL"] as? String) ?? defaults.string(forKey: "serverURL") ?? Keychain.get("serverURL") ?? ""
        apiKey = (args["apiKey"] as? String) ?? Keychain.get("apiKey") ?? ""
        server = defaults.data(forKey: "server").flatMap { try? JSONDecoder().decode(ServerInfo.self, from: $0) }
        user = defaults.data(forKey: "user").flatMap { try? JSONDecoder().decode(UserInfo.self, from: $0) }
        registered = defaults.string(forKey: "registered")
        tradesBySet = defaults.object(forKey: "tradesBySet") as? Bool ?? false
        notificationPrefs = defaults.data(forKey: "notificationPrefs").flatMap { try? JSONDecoder().decode(DevicePrefs.self, from: $0) } ?? DevicePrefs()
        // Saved again at every start: an item from a build before the shared keychain group moves
        // into it, and an extension finds the server even when only the key was in the keychain.
        Keychain.set(apiKey, for: "apiKey")
        Keychain.set(serverURL, for: "serverURL")
    }

    var isPaired: Bool { api != nil }
    var api: TJAPI? { TJAPI(serverURL: serverURL, key: apiKey) }
    /// The trading timezone, the server's; the phone's until the server has said.
    var tz: TimeZone { server?.tz ?? .current }

    /// The push token this phone registered with the current server, from the `registered` record.
    var registeredToken: String? { Self.registeredToken(registered, serverURL: serverURL) }

    nonisolated static func registeredToken(_ registered: String?, serverURL: String) -> String? {
        guard let registered, let bar = registered.lastIndex(of: "|") else { return nil }
        let url = String(registered[..<bar]), token = String(registered[registered.index(after: bar)...])
        guard url == serverURL, !token.isEmpty else { return nil }
        return token
    }

    /// Forgets the pairing on this phone (the key stays valid on the server until unpaired there).
    func signOut() {
        apiKey = ""
        registered = nil
        user = nil
    }
}
