// What this phone wants pushed, as the server reads it before every push (API.md, "Settings"):
// per kind on or off, its sound and whether it breaks through Focus, plus the activity choices.
// The app owns them: kept in the preferences, sent whole with the registration and on every
// change. A kind back at its defaults is dropped so the server's record stays short.
import Foundation

enum PushKind: String, CaseIterable, Identifiable, Sendable {
    case nudge, tradeClosed, planMissing
    var id: String { rawValue }

    var title: String {
        switch self {
        case .nudge: "Guardrail nudges"
        case .tradeClosed: "Closed trades"
        case .planMissing: "No plan before the open"
        }
    }

    var detail: String {
        switch self {
        case .nudge: "The moment the guardrail records one, with your lesson."
        case .tradeClosed: "A quiet note when a trade closes, with its net."
        case .planMissing: "Once a day between 08:15 and 08:30 Chicago, when no plan is written."
        }
    }

    /// Breaks through Focus unless switched off: the kind that needs you now.
    var timeSensitiveByDefault: Bool { self == .nudge }
    /// Off until the phone opts in.
    var onByDefault: Bool { self != .planMissing }
    /// The app's sound for the kind, bundled under Sounds/.
    var appSound: String { self == .tradeClosed ? "tj-trade" : "tj-nudge" }
}

enum PushSound: String, CaseIterable, Identifiable, Sendable {
    case tj, system = "default", none
    var id: String { rawValue }
    var title: String {
        switch self {
        case .tj: "journal"
        case .system: "iOS default"
        case .none: "silent"
        }
    }
}

struct DevicePrefs: Codable, Equatable, Hashable, Sendable {
    struct KindPrefs: Codable, Equatable, Hashable, Sendable {
        var off: Bool?
        var sound: String? // nil or "tj": the app's; "default"; "none"
        var timeSensitive: Bool?
    }

    var kinds: [String: KindPrefs] = [:]
    var noActivity: Bool?
    var finishedAsNotification: Bool?
    var quietOutsideRth: Bool?

    enum CodingKeys: String, CodingKey { case kinds, noActivity, finishedAsNotification, quietOutsideRth }

    var showsActivity: Bool { noActivity != true }

    func isOn(_ k: PushKind) -> Bool {
        if let off = kinds[k.rawValue]?.off { return !off }
        return k.onByDefault
    }

    mutating func setOn(_ k: PushKind, _ on: Bool) {
        kinds[k.rawValue, default: KindPrefs()].off = on == k.onByDefault ? nil : !on
        tidy(k)
    }

    func sound(_ k: PushKind) -> PushSound { PushSound(rawValue: kinds[k.rawValue]?.sound ?? "tj") ?? .tj }

    mutating func setSound(_ k: PushKind, _ s: PushSound) {
        kinds[k.rawValue, default: KindPrefs()].sound = s == .tj ? nil : s.rawValue
        tidy(k)
    }

    func timeSensitive(_ k: PushKind) -> Bool { kinds[k.rawValue]?.timeSensitive ?? k.timeSensitiveByDefault }

    mutating func setTimeSensitive(_ k: PushKind, _ on: Bool) {
        kinds[k.rawValue, default: KindPrefs()].timeSensitive = on == k.timeSensitiveByDefault ? nil : on
        tidy(k)
    }

    private mutating func tidy(_ k: PushKind) {
        if kinds[k.rawValue] == KindPrefs() { kinds[k.rawValue] = nil }
    }

    init() {}

    /// The server omits what is at its default, kinds included.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        kinds = try c.decodeIfPresent([String: KindPrefs].self, forKey: .kinds) ?? [:]
        noActivity = try c.decodeIfPresent(Bool.self, forKey: .noActivity)
        finishedAsNotification = try c.decodeIfPresent(Bool.self, forKey: .finishedAsNotification)
        quietOutsideRth = try c.decodeIfPresent(Bool.self, forKey: .quietOutsideRth)
    }

    /// The server omits what is at its default; "sent whole" still means these four keys.
    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(kinds, forKey: .kinds)
        if noActivity == true { try c.encode(true, forKey: .noActivity) }
        if finishedAsNotification == true { try c.encode(true, forKey: .finishedAsNotification) }
        if quietOutsideRth == true { try c.encode(true, forKey: .quietOutsideRth) }
    }
}

/// POST /devices: the phone, which APNs service its token belongs to, and its settings.
struct DeviceRegistration: Encodable, Sendable {
    var token: String
    var name: String
    var sandbox: Bool
    var bundleId: String
    var prefs: DevicePrefs
}

struct DeviceActivity: Codable, Sendable, Hashable {
    var id: String
    var token: String
    var kind: String
}

struct ActivityReport: Encodable, Sendable {
    var startToken: String?
    var activities: [DeviceActivity]
}

struct Device: Codable, Sendable, Equatable {
    var token: String
    var name: String
    var sandbox: Bool
    var bundleId: String
    var prefs: DevicePrefs?
    var activityStart: Bool?
    var activities: [DeviceActivity]?
    var createdAt: Date
    var lastSeenAt: Date?
}

struct DeviceRegistered: Decodable, Sendable {
    var device: Device
    var push: Bool
}

struct DeviceList: Decodable, Sendable {
    var items: [Device]
    var push: Bool
}

struct DeviceSaved: Decodable, Sendable { var device: Device }
