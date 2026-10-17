// Settings: the journal this phone is paired with, what the server says about itself, the
// connection, and sign out. Notifications and the Live Activity get their sections in the
// push phase. PairView: scan the QR shown in the web app's Settings, or type the code.
import SwiftUI
import VisionKit

struct SettingsView: View {
    let pair: () -> Void
    @Environment(Client.self) private var client
    @Environment(Settings.self) private var settings
    @State private var me: Me?
    @State private var stored: Int?
    @State private var error: String?
    @State private var confirmSignOut = false
    @State private var notifications = Notifications.shared

    var body: some View {
        @Bindable var settings = settings
        Form {
            Section {
                LabeledContent("Server", value: settings.serverURL.isEmpty ? "—" : settings.serverURL)
                LabeledContent("User", value: settings.user?.name ?? "—")
                LabeledContent("Timezone", value: settings.server?.timezone ?? "—")
                LabeledContent("Server version", value: settings.server?.version ?? "—")
                LabeledContent("Connection") { ConnectionLine() }
                LabeledContent("AI", value: settings.aiReady ? "ready" : "off")
                if let me {
                    LabeledContent("This phone's key", value: "\(me.key.name) · \(me.key.kind)")
                }
                if let error { Text(error).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.negText) }
            } header: {
                Text("Journal")
            } footer: {
                Text("Paired with a device key made in the web app's Settings → Phone app. Unpair there to revoke it; Sign out here forgets it on this phone.")
            }
            Section {
                LabeledContent("Permission", value: permissionLabel)
                if notifications.authorization == .notDetermined {
                    Button("Enable notifications") { Task { await notifications.enable() } }
                } else if notifications.authorization == .denied {
                    Text("Allow notifications for Journal in the Settings app.").font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted)
                } else {
                    LabeledContent("Registered with the journal", value: notifications.isRegistered ? "yes" : (notifications.apnsToken == nil ? "no APNs token yet" : "no"))
                    if let can = notifications.serverCanPush ?? me?.server.features.push {
                        LabeledContent("Server can push", value: can ? "yes" : "no key configured")
                    }
                    NavigationLink("Which pushes, sounds, Focus") { NotificationSettingsView() }
                    Button("Register again") {
                        settings.registered = nil
                        Task { await notifications.registerIfNeeded() }
                    }
                }
                if let e = notifications.lastError {
                    Text(e).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.warnText)
                }
            } header: {
                Text("Notifications")
            } footer: {
                Text("The journal pushes the guardrail's nudges the moment they happen, closed trades quietly, and keeps the badge at the unseen count. A build from Xcode registers a sandbox token; a TestFlight build a production one.")
            }
            Section {
                if let me {
                    Toggle("Guardrail", isOn: Binding(get: { me.settings.guardrail }, set: { on in Task { await update(SettingsUpdate(guardrail: on)) } }))
                        .tint(Brand.accent)
                    Picker("Stop and target on a new position", selection: Binding(get: { me.settings.autoPlan }, set: { v in Task { await update(SettingsUpdate(autoPlan: v)) } })) {
                        Text("Nothing automatic").tag(0)
                        Text("Suggest only").tag(1)
                        Text("Set the plan").tag(2)
                    }
                    .disabled(!me.server.features.ai)
                    LabeledContent("Features", value: features(me.server.features))
                }
            } header: {
                Text("Trader")
            } footer: {
                Text("The guardrail watches the day against the plan and nudges you. The automatic plan needs AI on the server.")
            }
            Section {
                Toggle("Trades grouped by set", isOn: $settings.tradesBySet)
                LabeledContent("Kept for offline", value: stored.map { ByteCountFormatter.string(fromByteCount: Int64($0), countStyle: .file) } ?? "…")
                Button("Clear what is kept") { Task { await client.clearStore(); stored = await client.storedBytes() } }
                    .disabled((stored ?? 0) == 0)
            } header: {
                Text("This phone")
            } footer: {
                Text("The journal's answers, kept so the screens show the last known state when it is out of reach, and the images. At most 150 MB; answers older than a month leave.")
            }
            Section {
                Button("Pair again") { pair() }
                Button("Sign out", role: .destructive) { confirmSignOut = true }
                    .confirmationDialog("Forget this journal on this phone?", isPresented: $confirmSignOut, titleVisibility: .visible) {
                        Button("Sign out", role: .destructive) { client.signOut() }
                    }
            }
            Section("About") {
                LabeledContent("App version", value: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "")
                LabeledContent("Data version", value: "\(client.version)")
                NavigationLink("Licences") { LicencesView() }
            }
        }
        .navigationTitle("Settings")
        .navigationBarTitleDisplayMode(.inline)
        .task { stored = await client.storedBytes() }
        .task {
            guard settings.isPaired else { return }
            do {
                me = try await client.me()
                error = nil
            } catch {
                self.error = error.localizedDescription
            }
        }
    }

    private var permissionLabel: String {
        switch notifications.authorization {
        case .authorized: "allowed"
        case .provisional: "provisional"
        case .ephemeral: "ephemeral"
        case .denied: "denied"
        case .notDetermined: "not asked yet"
        @unknown default: "?"
        }
    }

    private func update(_ u: SettingsUpdate) async {
        do {
            let s = try await client.updateSettings(u)
            me?.settings = s
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }

    private func features(_ f: ServerFeatures) -> String {
        var on: [String] = []
        if f.ai { on.append("AI") }
        if f.charts { on.append("charts") }
        if f.guardrail { on.append("guardrail") }
        if f.push { on.append("push") }
        return on.isEmpty ? "none" : on.joined(separator: ", ")
    }
}

struct LicencesView: View {
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                Text("Inter by The Inter Project Authors and JetBrains Mono by JetBrains, both under the SIL Open Font License 1.1 (Shared/Fonts).").font(Brand.text(14, relativeTo: .subheadline))
                Text("Everything else is Apple's frameworks.").font(Brand.text(14, relativeTo: .subheadline)).foregroundStyle(Brand.fgMuted)
            }
            .padding(16)
        }
        .navigationTitle("Licences")
        .navigationBarTitleDisplayMode(.inline)
    }
}

/// Pairing: the web app's Settings shows a QR of tj://pair?server=…&code=… for five minutes.
/// Scan it, or type the server and the code.
struct PairView: View {
    let prefill: Route
    @Environment(Client.self) private var client
    @Environment(Settings.self) private var settings
    @Environment(\.dismiss) private var dismiss
    @State private var server = ""
    @State private var code = ""
    @State private var busy = false
    @State private var error: String?
    @State private var scanning = false

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("https://journal.example.net", text: $server)
                        .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                    TextField("ABCD-EFGH", text: $code)
                        .textInputAutocapitalization(.characters).autocorrectionDisabled()
                        .font(Brand.mono(17))
                } header: {
                    Text("Pair with the journal")
                } footer: {
                    Text("In the web app: Settings → Phone app → Pair a phone. The code lasts five minutes and works once.")
                }
                Section {
                    if DataScannerViewController.isSupported && DataScannerViewController.isAvailable {
                        Button { scanning = true } label: { Label("Scan the QR code", systemImage: "qrcode.viewfinder") }
                    }
                    Button {
                        Task { await pair() }
                    } label: {
                        HStack {
                            Text(busy ? "Pairing…" : "Pair")
                            if busy { Spacer(); ProgressView() }
                        }
                    }
                    .disabled(busy || ServerURL.base(server) == nil || code.trimmingCharacters(in: .whitespaces).count < 8)
                    if let error { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) }
                }
                if client.connection == .unauthorized {
                    Section {
                        Text("The journal no longer accepts this phone's key. Pair again to continue.").font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.warnText)
                    }
                }
            }
            .navigationTitle("Pair")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                if settings.isPaired {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                }
            }
            .sheet(isPresented: $scanning) {
                QRScanner { url in
                    scanning = false
                    if let r = Route.parse(url), case .pair(let s, let c) = r {
                        if let s { server = s }
                        if let c { code = c }
                        Task { await pair() }
                    } else {
                        error = "That QR is not a pairing code."
                    }
                }
            }
            .onAppear {
                if case .pair(let s, let c) = prefill {
                    server = s ?? settings.serverURL
                    code = c ?? ""
                    if s != nil, c != nil { Task { await pair() } }
                }
            }
        }
    }

    private func pair() async {
        busy = true
        defer { busy = false }
        do {
            try await client.pair(serverURL: server, code: code.trimmingCharacters(in: .whitespacesAndNewlines))
            error = nil
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// The camera, looking for one QR with a tj:// URL.
struct QRScanner: UIViewControllerRepresentable {
    let found: (URL) -> Void

    func makeUIViewController(context: Context) -> DataScannerViewController {
        let vc = DataScannerViewController(recognizedDataTypes: [.barcode(symbologies: [.qr])], qualityLevel: .balanced, isHighlightingEnabled: true)
        vc.delegate = context.coordinator
        try? vc.startScanning()
        return vc
    }

    func updateUIViewController(_ uiViewController: DataScannerViewController, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator(found: found) }

    final class Coordinator: NSObject, DataScannerViewControllerDelegate {
        let found: (URL) -> Void
        private var done = false
        init(found: @escaping (URL) -> Void) { self.found = found }

        func dataScanner(_ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem], allItems: [RecognizedItem]) {
            guard !done else { return }
            for item in addedItems {
                if case .barcode(let b) = item, let s = b.payloadStringValue, let url = URL(string: s), url.scheme?.lowercased() == "tj" {
                    done = true
                    dataScanner.stopScanning()
                    found(url)
                    return
                }
            }
        }
    }
}
