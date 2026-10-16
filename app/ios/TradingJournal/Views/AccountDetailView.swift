// One account: its running balance, the anchors it rests on (add one, delete one), and its trades.
import SwiftUI

struct AccountDetailView: View {
    let accountId: Int
    @Environment(Client.self) private var client
    @State private var account: Account?
    @State private var error: String?
    @State private var adding = false
    @State private var deleting: BalanceAnchor?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let a = account {
                    Card {
                        HStack(spacing: 6) {
                            Text(a.name).font(Brand.text(17, weight: .semibold, relativeTo: .title3)).lineLimit(1).truncationMode(.middle)
                            Spacer()
                        }
                        HStack(spacing: 6) {
                            if let f = a.firm { Pill(f, .sunken) }
                            Pill.accountType(a)
                            if a.retired { Pill("retired", .retired) }
                            if a.excludedFromStats { Pill("excluded from stats", .sunken) }
                            Spacer()
                        }
                        HStack(alignment: .firstTextBaseline) {
                            Text(Formatting.balance(a.balance?.balance)).font(Brand.mono(28, bold: true)).foregroundStyle(a.balance == nil ? Brand.fgSubtle : Brand.fg)
                            Spacer()
                            if let d = a.dayNet { PnlChip(d, size: .md) }
                        }
                        if let b = a.balance {
                            Text("as of \(b.asOf), from the anchor of \(b.anchorDate)\(b.unchangedSinceAnchor ? " (no trades since)" : " plus the trades since")").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                        } else {
                            Text("No balance anchor yet: add the balance your broker shows on a day and the journal carries it forward with the trades.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.warnText)
                        }
                    }
                    Card {
                        CardTitle("Balance anchors", trailing: "\(a.balance?.anchors.count ?? 0)")
                        ForEach(a.balance?.anchors ?? []) { an in
                            HStack {
                                Text(an.date).font(Brand.mono(13))
                                if !an.note.isEmpty { Text(an.note).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted).lineLimit(1) }
                                Spacer()
                                Text(Formatting.balance(an.balance)).font(Brand.mono(13))
                                Button { deleting = an } label: { Image(systemName: "trash").foregroundStyle(Brand.fgSubtle) }
                                    .buttonStyle(.plain)
                                    .accessibilityLabel("Delete the anchor of \(an.date)")
                            }
                        }
                        Button { adding = true } label: { Label("Add a balance", systemImage: "plus.circle") }.font(Brand.text(14, relativeTo: .subheadline))
                    }
                    NavigationLink { TradesView(initial: .init(accountId: a.id)) } label: {
                        Card {
                            HStack {
                                Label("Trades on this account", systemImage: "list.bullet.rectangle").font(Brand.text(14, weight: .medium, relativeTo: .subheadline))
                                Spacer()
                                Image(systemName: "chevron.right").foregroundStyle(Brand.fgSubtle)
                            }
                        }
                    }
                    .buttonStyle(.plain)
                } else if let error {
                    ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
                } else {
                    ProgressView().frame(maxWidth: .infinity, minHeight: 200)
                }
            }
            .padding(16)
        }
        .background(Brand.bg)
        .navigationTitle(account?.name ?? "Account")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: accountId) { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
        .sheet(isPresented: $adding) {
            if let a = account {
                BalanceAddView(account: a) { saved in account = saved }
            }
        }
        .confirmationDialog("Delete this anchor?", isPresented: Binding(get: { deleting != nil }, set: { if !$0 { deleting = nil } }), titleVisibility: .visible) {
            Button("Delete", role: .destructive) {
                if let an = deleting { Task { await delete(an) } }
                deleting = nil
            }
        } message: {
            if let an = deleting { Text("The balance of \(an.date) comes off the record; the running balance follows the remaining anchors.") }
        }
    }

    private func load() async {
        do {
            let all = try await client.accounts(showRetired: true)
            account = all.first { $0.id == accountId }
            error = account == nil ? "Not your account" : nil
        } catch {
            if account == nil { self.error = error.localizedDescription }
        }
    }

    private func delete(_ an: BalanceAnchor) async {
        do {
            try await client.deleteBalance(account: accountId, anchor: an.id)
            await load()
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct BalanceAddView: View {
    let account: Account
    var onSaved: (Account) -> Void = { _ in }
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var date: Date = .now
    @State private var balance = ""
    @State private var note = ""
    @State private var busy = false
    @State private var error: String?

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    DatePicker("Day", selection: $date, in: ...Date.now, displayedComponents: .date)
                    LabeledContent("Balance") {
                        TextField("0.00", text: $balance).keyboardType(.decimalPad).multilineTextAlignment(.trailing).font(Brand.mono(15))
                    }
                    TextField("Note (optional)", text: $note)
                } footer: {
                    Text("The balance the broker showed at the end of that day, in \(account.currency). The journal adds the trades since.")
                }
                if let error { Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText) }
            }
            .navigationTitle("Add a balance")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button(busy ? "Saving…" : "Save") { Task { await save() } }.disabled(busy || PlanMath.parse(balance) == nil)
                }
            }
        }
    }

    private func save() async {
        guard let v = PlanMath.parse(balance) else { return }
        busy = true
        defer { busy = false }
        do {
            let saved = try await client.addBalance(account: account.id, BalanceAnchorAdd(date: Formatting.day(date, tz: client.tz), balance: v, note: note))
            onSaved(saved)
            dismiss()
        } catch {
            self.error = error.localizedDescription
        }
    }
}
