// What the phone keeps of the journal's answers, so the screens have something to show when the
// server is out of reach: every GET's body as the server sent it, by its path and query, with
// the time it arrived. The server stays the only source of truth; a stored answer is shown as
// what it is ("last known 09:12") and replaced by the live one as soon as there is one. Files
// under Application Support, one per answer, with a small index; nothing from another server or
// another key is ever read, since the store lives under a scope hashed from both.
import CryptoKit
import Foundation

/// An answer and, when it is the stored one, the time the server gave it. A live answer has no
/// asOf. The value's members read through (`loaded.items`), so a screen uses it as the value.
@dynamicMemberLookup
struct Loaded<T: Sendable>: Sendable {
    var value: T
    var asOf: Date? = nil

    var isStale: Bool { asOf != nil }

    subscript<V>(dynamicMember keyPath: KeyPath<T, V>) -> V { value[keyPath: keyPath] }

    func map<U: Sendable>(_ f: (T) -> U) -> Loaded<U> { Loaded<U>(value: f(value), asOf: asOf) }
}

/// One store: a directory of answers under a byte cap, the least recently used leaving first,
/// and (for answers that go out of date) a maximum age.
actor Store {
    struct Entry: Codable, Sendable {
        var key: String
        /// When the answer arrived.
        var at: Date
        /// When it was last read, for the eviction order.
        var used: Date
        var size: Int
    }

    let dir: URL
    let cap: Int
    let maxAge: TimeInterval?
    private var index: [String: Entry] = [:]
    private var loaded = false

    /// The answers' home: Application Support/Store, created on first use. Only this phone's
    /// app reads it; the files are protected until the first unlock, so the sync push and the
    /// warm-up can run before the phone is unlocked once after a restart.
    nonisolated static let defaultRoot: URL = {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first ?? FileManager.default.temporaryDirectory
        return base.appending(path: "Store", directoryHint: .isDirectory)
    }()

    static let jsonCap = 50 << 20
    static let jsonMaxAge: TimeInterval = 30 * 86400
    static let imageCap = 100 << 20

    init(dir: URL, cap: Int, maxAge: TimeInterval? = nil) {
        self.dir = dir
        self.cap = cap
        self.maxAge = maxAge
    }

    /// The two stores of one pairing: the JSON answers and the images.
    static func open(root: URL = defaultRoot, scope: String) -> (json: Store, images: Store) {
        let base = root.appending(path: scope, directoryHint: .isDirectory)
        return (Store(dir: base.appending(path: "json"), cap: jsonCap, maxAge: jsonMaxAge),
                Store(dir: base.appending(path: "images"), cap: imageCap))
    }

    /// The scope of a pairing: a hash of the server's address and the key, so a different
    /// server or a new key never sees another's answers.
    nonisolated static func scope(serverURL: String, key: String) -> String {
        Self.hash(serverURL + "\n" + key)
    }

    /// Removes every scope but the one named; after pairing anew.
    nonisolated static func wipeOthers(root: URL = defaultRoot, than scope: String) {
        guard let names = try? FileManager.default.contentsOfDirectory(atPath: root.path()) else { return }
        for n in names where n != scope {
            try? FileManager.default.removeItem(at: root.appending(path: n))
        }
    }

    /// The store's key of a GET: the path and its query in a fixed order. A `refresh` item asks
    /// the server for fresh numbers and changes nothing in what the answer is about, so it is
    /// left out, and the answer replaces the plain one.
    nonisolated static func key(_ path: String, query: [URLQueryItem] = []) -> String {
        let q = query.filter { $0.name != "refresh" }.sorted { ($0.name, $0.value ?? "") < ($1.name, $1.value ?? "") }
        guard !q.isEmpty else { return path }
        return path + "?" + q.map { "\($0.name)=\($0.value ?? "")" }.joined(separator: "&")
    }

    nonisolated static func hash(_ s: String) -> String {
        SHA256.hash(data: Data(s.utf8)).prefix(16).map { String(format: "%02x", $0) }.joined()
    }

    // MARK: reading and writing

    /// The stored answer and when it arrived; nil when there is none (or the file is gone).
    func get(_ key: String) -> (Data, Date)? {
        load()
        let name = Self.hash(key)
        guard var e = index[name] else { return nil }
        guard let data = try? Data(contentsOf: file(name)) else {
            index[name] = nil
            save()
            return nil
        }
        e.used = .now
        index[name] = e
        return (data, e.at)
    }

    /// Keeps an answer that arrived now, replacing the one before it.
    func put(_ key: String, _ data: Data, at: Date = .now) {
        load()
        let name = Self.hash(key)
        do {
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: Self.attributes)
            try data.write(to: file(name), options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        } catch {
            return
        }
        index[name] = Entry(key: key, at: at, used: at, size: data.count)
        if bytes > cap { evict() }
        save()
    }

    func remove(_ key: String) {
        load()
        let name = Self.hash(key)
        guard index.removeValue(forKey: name) != nil else { return }
        try? FileManager.default.removeItem(at: file(name))
        save()
    }

    /// Every answer this store holds.
    var bytes: Int {
        load()
        return index.values.reduce(0) { $0 + $1.size }
    }

    var count: Int {
        load()
        return index.count
    }

    /// Drops what is too old and what is over the cap; at every start.
    func sweep() {
        load()
        if let maxAge {
            let limit = Date.now.addingTimeInterval(-maxAge)
            for (name, e) in index where e.at < limit {
                index[name] = nil
                try? FileManager.default.removeItem(at: file(name))
            }
        }
        evict()
        save()
    }

    /// Forgets everything: at sign out, and when the trader asks.
    func wipe() {
        index = [:]
        loaded = true
        try? FileManager.default.removeItem(at: dir)
    }

    // MARK: inside

    private static var attributes: [FileAttributeKey: Any] { [.protectionKey: FileProtectionType.completeUntilFirstUserAuthentication] }

    private func file(_ name: String) -> URL { dir.appending(path: name) }
    private var indexFile: URL { dir.appending(path: "index.json") }

    private func load() {
        guard !loaded else { return }
        loaded = true
        if let data = try? Data(contentsOf: indexFile), let i = try? JSONDecoder().decode([String: Entry].self, from: data) {
            index = i
        }
    }

    private func save() {
        guard let data = try? JSONEncoder().encode(index) else { return }
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: Self.attributes)
        try? data.write(to: indexFile, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
    }

    /// The least recently used leave until the store is under its cap.
    private func evict() {
        var total = index.values.reduce(0) { $0 + $1.size }
        guard total > cap else { return }
        for (name, e) in index.sorted(by: { $0.value.used < $1.value.used }) {
            index[name] = nil
            try? FileManager.default.removeItem(at: file(name))
            total -= e.size
            if total <= cap { break }
        }
    }
}

/// The words a trader typed that the journal has not taken yet: a save that met no answer keeps
/// its draft here, by entry (or by what the new note is for), by plan day, by position, and
/// the editor opens on it next time. UserDefaults: a draft is small and there are few.
enum Drafts {
    struct Draft<T: Codable & Sendable>: Codable, Sendable {
        var value: T
        var at: Date
    }

    nonisolated static func key(entry id: Int) -> String { "draft:entry:\(id)" }
    nonisolated static func key(new scope: JournalScope, date: String?, tradeId: Int?) -> String { "draft:new:\(scope.rawValue):\(date ?? ""):\(tradeId.map(String.init) ?? "")" }
    nonisolated static func key(plan day: String) -> String { "draft:plan:\(day)" }
    nonisolated static func key(positionPlan account: Int, symbol: String) -> String { "draft:position:\(account):\(symbol)" }

    nonisolated static func load<T: Codable & Sendable>(_ type: T.Type, _ key: String, defaults: UserDefaults = .standard) -> Draft<T>? {
        guard let data = defaults.data(forKey: key) else { return nil }
        return try? JSONDecoder().decode(Draft<T>.self, from: data)
    }

    nonisolated static func save<T: Codable & Sendable>(_ value: T, _ key: String, defaults: UserDefaults = .standard) {
        defaults.set(try? JSONEncoder().encode(Draft(value: value, at: .now)), forKey: key)
    }

    nonisolated static func clear(_ key: String, defaults: UserDefaults = .standard) {
        defaults.removeObject(forKey: key)
    }
}
