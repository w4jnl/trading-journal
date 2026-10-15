// The app's side of the Session activity: it hands the server the tokens (push-to-start, and
// each running activity's update token) so the server can start, update and end it while the
// app sleeps, starts it itself when the app is open and the day has begun, and refreshes it
// from Today while the app is in front. The server decides the content (push/live.go); the
// local state uses the same rules so the first frame is right.
import ActivityKit
import Foundation

@MainActor
final class LiveActivities {
    static let shared = LiveActivities()

    weak var client: Client?
    private var startToken: String?
    private var tokens: [String: String] = [:] // activity id → its update token
    private var watched: Set<String> = []
    private var reporting: Task<Void, Never>?

    var enabled: Bool { ActivityAuthorizationInfo().areActivitiesEnabled }

    func setup(client: Client) {
        self.client = client
        Task {
            for await data in Activity<SessionActivityAttributes>.pushToStartTokenUpdates {
                self.startToken = data.hex
                self.reportSoon()
            }
        }
        Task {
            for await a in Activity<SessionActivityAttributes>.activityUpdates { self.watch(a) } // started by a push too
        }
        for a in Activity<SessionActivityAttributes>.activities { watch(a) }
    }

    private func watch(_ a: Activity<SessionActivityAttributes>) {
        guard watched.insert(a.id).inserted else { return }
        let id = a.id
        Task {
            for await data in a.pushTokenUpdates {
                self.tokens[id] = data.hex
                self.reportSoon()
            }
        }
        Task {
            for await st in a.activityStateUpdates where st == .ended || st == .dismissed {
                self.tokens[id] = nil
                self.watched.remove(id)
                self.reportSoon()
            }
        }
    }

    /// Sends the tokens a moment later (a start and its token arrive close together).
    func reportSoon() {
        reporting?.cancel()
        reporting = Task {
            try? await Task.sleep(for: .milliseconds(300))
            guard !Task.isCancelled else { return }
            await report()
        }
    }

    func report() async {
        let list = Activity<SessionActivityAttributes>.activities.compactMap { a -> DeviceActivity? in
            guard a.activityState == .active || a.activityState == .stale, let token = tokens[a.id] else { return nil }
            return DeviceActivity(id: a.id, token: token, kind: "session")
        }
        await client?.reportActivities(start: startToken, activities: list)
    }

    private var running: [Activity<SessionActivityAttributes>] {
        Activity<SessionActivityAttributes>.activities.filter { $0.activityState == .active || $0.activityState == .stale }
    }

    var isRunning: Bool { !running.isEmpty }

    /// With the app open: refreshes the running activity from Today, starts one when the day has
    /// begun and none runs, ends one from another day, or ends all when the user switched it off.
    func ensureSession(_ t: Today, allowed: Bool) {
        if !allowed {
            for id in running.map(\.id) { Task { await Self.end(id) } }
            return
        }
        let state = SessionActivityAttributes.ContentState.from(t)
        let stale = Date.now.addingTimeInterval(10 * 60)
        var current = false
        for a in running {
            if a.attributes.day == t.day {
                current = true
                let id = a.id
                Task { await Self.update(id, state: state, stale: stale) }
            } else {
                let id = a.id
                Task { await Self.end(id) }
            }
        }
        guard enabled, !current, t.active, t.session.inGlobex else { return }
        let attributes = SessionActivityAttributes.from(t)
        do {
            watch(try Activity<SessionActivityAttributes>.request(attributes: attributes, content: .init(state: state, staleDate: stale), pushType: .token))
        } catch {
            // No APS environment (an unsigned simulator build): the activity still shows, updated
            // by the app while it is open, never by a push.
            if let a = try? Activity<SessionActivityAttributes>.request(attributes: attributes, content: .init(state: state, staleDate: stale), pushType: nil) {
                watch(a)
            }
        }
    }

    /// Updates an activity by id; looked up off the main actor, because an Activity is not Sendable.
    nonisolated private static func update(_ id: String, state: SessionActivityAttributes.ContentState, stale: Date) async {
        for a in Activity<SessionActivityAttributes>.activities where a.id == id {
            await a.update(.init(state: state, staleDate: stale))
        }
    }

    /// Ends an activity by id; looked up off the main actor, because an Activity is not Sendable.
    nonisolated private static func end(_ id: String) async {
        for a in Activity<SessionActivityAttributes>.activities where a.id == id {
            await a.end(nil, dismissalPolicy: .immediate)
        }
    }
}

private extension Data {
    var hex: String { map { String(format: "%02x", $0) }.joined() }
}
