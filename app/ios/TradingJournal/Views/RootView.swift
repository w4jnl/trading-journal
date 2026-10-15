// The shell: five tabs on an iPhone, a sidebar on an iPad or in a wide window (by the size
// class, not the device), each section its own NavigationStack with a typed path. Every way
// in (a tap on a notification, a tj:// link, the -open launch argument) sets client.pendingRoute
// and lands here.
import SwiftUI

struct RootView: View {
    @Environment(Client.self) private var client
    @Environment(Settings.self) private var settings
    @Environment(\.horizontalSizeClass) private var sizeClass
    @State private var tab: Route.Tab = .today
    @State private var paths: [Route.Tab: [Route]] = [:]
    @State private var pairing: Route?

    var body: some View {
        Group {
            if sizeClass == .regular {
                NavigationSplitView {
                    List(Route.Tab.allCases, id: \.self, selection: Binding(get: { Optional(tab) }, set: { if let t = $0 { tab = t } })) { t in
                        Label(t.title, systemImage: t.symbol)
                    }
                    .navigationTitle("Journal")
                } detail: {
                    stack(tab)
                }
            } else {
                TabView(selection: $tab) {
                    ForEach(Route.Tab.allCases, id: \.self) { t in
                        stack(t)
                            .tabItem { Label(t.title, systemImage: t.symbol) }
                            .tag(t)
                    }
                }
            }
        }
        .tint(Brand.accent)
        .fullScreenCover(item: $pairing) { route in
            PairView(prefill: route)
        }
        .onAppear {
            client.start()
            Task { await Notifications.shared.refreshAuthorization() }
            if !settings.isPaired { pairing = .pair(server: settings.serverURL.isEmpty ? nil : settings.serverURL, code: nil) }
            // development: `simctl launch … -open trade/5` opens a screen straight away
            if let o = UserDefaults.standard.string(forKey: "open"), let r = Route.from(route: o) {
                client.pendingRoute = r
            }
        }
        .onOpenURL { url in
            if let r = Route.parse(url) { client.pendingRoute = r }
        }
        .onChange(of: client.pendingRoute) { _, r in
            guard let r else { return }
            client.pendingRoute = nil
            open(r)
        }
        .onChange(of: client.connection) { _, c in
            if c == .unauthorized, pairing == nil { pairing = .pair(server: settings.serverURL, code: nil) }
        }
        .onChange(of: settings.apiKey) { _, _ in
            Task { await Notifications.shared.registerIfNeeded() } // paired again: the token goes to this server
        }
    }

    private func stack(_ t: Route.Tab) -> some View {
        NavigationStack(path: binding(t)) {
            root(t).navigationDestination(for: Route.self) { destination($0) }
        }
    }

    private func binding(_ t: Route.Tab) -> Binding<[Route]> {
        Binding(get: { paths[t] ?? [] }, set: { paths[t] = $0 })
    }

    private func open(_ r: Route) {
        if case .pair = r {
            pairing = r
            return
        }
        pairing = nil
        tab = r.tab
        switch r {
        case .today, .settings: paths[r.tab] = []
        default: paths[r.tab] = [r]
        }
    }

    @ViewBuilder
    private func root(_ t: Route.Tab) -> some View {
        switch t {
        case .today: TodayView()
        case .trades: TradesView()
        case .journal: JournalView()
        case .calendar: CalendarView()
        case .settings: SettingsView(pair: { pairing = .pair(server: settings.serverURL, code: nil) })
        }
    }

    @ViewBuilder
    private func destination(_ r: Route) -> some View {
        switch r {
        case .today: TodayView()
        case .day(let d): DayView(day: d)
        case .trade(let id): TradeDetailView(id: id)
        case .position(let acc, let sym): PositionDetailView(accountId: acc, symbol: sym)
        case .journal(let id): JournalEntryView(id: id)
        case .nudges: NudgesView()
        case .plan: PlanView()
        case .accounts: AccountsView()
        case .settings: SettingsView(pair: { pairing = .pair(server: settings.serverURL, code: nil) })
        case .pair: EmptyView()
        }
    }
}

extension Route: Identifiable {
    var id: String { url.absoluteString }
}
