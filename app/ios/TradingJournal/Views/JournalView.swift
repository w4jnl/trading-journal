// The journal: entries by date with their tag, mood, net and a snippet; a scope filter. And one
// entry: the note as markdown with its images, the labels, the checklist, the linked trades.
import SwiftUI

struct JournalView: View {
    @Environment(Client.self) private var client
    @State private var rows: [JournalRow] = []
    @State private var loading = false
    @State private var error: String?
    @State private var scope: ScopeFilter = .all

    enum ScopeFilter: String, CaseIterable, Identifiable {
        case all, day, week, trade, group
        var id: String { rawValue }
        var scopes: [JournalScope] {
            switch self {
            case .all: []
            case .day: [.day]
            case .week: [.week]
            case .trade: [.trade]
            case .group: [.group]
            }
        }
        var title: String { self == .all ? "All" : self == .group ? "Sets" : rawValue.capitalized }
    }

    var body: some View {
        List {
            Section {
                Picker("Scope", selection: $scope) {
                    ForEach(ScopeFilter.allCases) { Text($0.title).tag($0) }
                }
                .pickerStyle(.segmented)
                .onChange(of: scope) { _, _ in Task { await load() } }
            }
            .listRowBackground(Brand.bg)
            .listRowInsets(EdgeInsets(top: 4, leading: 16, bottom: 4, trailing: 16))
            if let error {
                ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
            } else if rows.isEmpty && !loading {
                ContentUnavailableView("No notes", systemImage: "book")
            }
            ForEach(rows) { r in
                NavigationLink(value: Route.journal(r.id)) { JournalRowView(row: r, tz: client.tz) }
            }
            if rows.count >= 50 {
                Button("More") { Task { await load(offset: rows.count) } }.font(Brand.text(13, relativeTo: .footnote))
            }
        }
        .listStyle(.plain)
        .background(Brand.bg)
        .scrollContentBackground(.hidden)
        .navigationTitle("Journal")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load() }
        .task { if rows.isEmpty { await load() } }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    private func load(offset: Int = 0) async {
        loading = true
        defer { loading = false }
        do {
            let items = try await client.journal(.init(scopes: scope.scopes, offset: offset))
            rows = offset == 0 ? items : rows + items
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct JournalRowView: View {
    let row: JournalRow
    let tz: TimeZone

    var body: some View {
        let r = row
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Text(Formatting.dayLabel(r.date, tz: tz)).font(Brand.text(14, weight: .semibold, relativeTo: .subheadline))
                Pill(r.scope == .group ? "set" : r.scope.rawValue, r.scope == .group ? .group : .sunken)
                if let p = Pill.tag(r.tag) { p }
                if let m = r.mood { MoodDots(mood: m) }
                Spacer()
                if let s = r.summary, s.trades > 0 { PnlChip(s.net, size: .sm) }
            }
            if let st = r.strategy { Text(st).font(Brand.text(11, weight: .medium, relativeTo: .caption2)).foregroundStyle(Brand.accentText) }
            Text(r.plainSnippet).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted).lineLimit(2)
            if let l = r.labels, !(l.mistakes.isEmpty && l.emotions.isEmpty) {
                HStack(spacing: 4) {
                    ForEach(l.mistakes, id: \.self) { Pill($0, .error) }
                    ForEach(l.emotions, id: \.self) { Pill($0, .info) }
                }
            }
        }
        .padding(.vertical, 2)
    }
}

/// Mood 1–5 as five dots.
struct MoodDots: View {
    let mood: Int
    var body: some View {
        HStack(spacing: 2) {
            ForEach(1...5, id: \.self) { i in
                Circle().fill(i <= mood ? Brand.accent : Brand.border).frame(width: 5, height: 5)
            }
        }
        .accessibilityLabel("mood \(mood) of 5")
    }
}

struct JournalEntryView: View {
    let id: Int
    @Environment(Client.self) private var client
    @Environment(\.dismiss) private var dismiss
    @State private var entry: JournalDetail?
    @State private var error: String?
    @State private var editing = false
    @State private var appending = false

    var body: some View {
        ScrollView {
            if let e = entry {
                VStack(alignment: .leading, spacing: 12) {
                    Card {
                        HStack(spacing: 6) {
                            Text(Formatting.dayLabel(e.date, tz: client.tz, style: .long)).font(Brand.text(17, weight: .semibold, relativeTo: .title3))
                            Spacer()
                        }
                        HStack(spacing: 6) {
                            Pill(e.scope == .group ? "set" : e.scope.rawValue, e.scope == .group ? .group : .sunken)
                            if let p = Pill.tag(e.tag) { p }
                            if let m = e.mood { MoodDots(mood: m) }
                            if let st = e.strategy { Pill(st.name, .accent) }
                            Spacer()
                            if let s = e.summary, s.trades > 0 { PnlChip(s.net, size: .md) }
                        }
                        if !(e.labels.mistakes.isEmpty && e.labels.emotions.isEmpty) {
                            HStack(spacing: 4) {
                                ForEach(e.labels.mistakes, id: \.self) { Pill($0, .error) }
                                ForEach(e.labels.emotions, id: \.self) { Pill($0, .info) }
                            }
                        }
                        if let s = e.summary, s.trades > 0 {
                            StatStrip(stats: [
                                .init(label: "trades", value: "\(s.trades)"),
                                .init(label: "wins", value: "\(s.wins)", color: Brand.posText),
                                .init(label: "losses", value: "\(s.losses)", color: Brand.negText),
                                .init(label: "fees", value: String(format: "%.2f", s.commission)),
                            ])
                        }
                    }
                    Card {
                        if e.notes.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                            Text("No note written yet.").font(Brand.text(14, relativeTo: .subheadline)).foregroundStyle(Brand.fgMuted)
                        } else {
                            MarkdownView(source: e.notes)
                        }
                    }
                    if let c = e.checklist, !c.items.isEmpty {
                        Card {
                            CardTitle("Checklist", trailing: "\(c.done) of \(c.total)")
                            ForEach(c.items) { i in
                                HStack(alignment: .firstTextBaseline, spacing: 8) {
                                    Image(systemName: i.checked ? "checkmark.square.fill" : "square").foregroundStyle(i.checked ? Brand.accent : Brand.fgSubtle)
                                    Text(i.text).font(Brand.text(14, relativeTo: .subheadline)).foregroundStyle(i.checked ? Brand.fg : Brand.fgMuted)
                                }
                            }
                        }
                    }
                    if !e.trades.isEmpty {
                        Card {
                            CardTitle("Trades", trailing: "\(e.trades.count)")
                            ForEach(e.trades) { t in
                                NavigationLink(value: Route.trade(t.id)) {
                                    HStack(spacing: 6) {
                                        Text(Formatting.clock(t.exitTime ?? t.entryTime, tz: client.tz)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                                        Text(t.symbol).font(Brand.text(13, weight: .medium, relativeTo: .footnote))
                                        Pill.direction(t.direction)
                                        Text(Formatting.qty(t.qty)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                                        Spacer()
                                        PnlChip(t.netPnl, size: .sm)
                                    }
                                }
                                .buttonStyle(.plain)
                            }
                        }
                    }
                    if let png = e.chartPng, !png.isEmpty, e.scope != .week {
                        Card {
                            CardTitle("Chart")
                            AuthImage(href: png, maxHeight: 320)
                        }
                    }
                    Text("Updated \(Formatting.dayLabel(Formatting.day(e.updatedAt, tz: client.tz), tz: client.tz)) \(Formatting.clock(e.updatedAt, tz: client.tz))").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                }
                .padding(16)
            } else if let error {
                ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 200)
            }
        }
        .background(Brand.bg)
        .navigationTitle(entry.map { Formatting.dayLabel($0.date, tz: client.tz) } ?? "Note")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if entry != nil {
                ToolbarItemGroup(placement: .topBarTrailing) {
                    Button { appending = true } label: { Label("Append", systemImage: "text.append") }
                    Button("Edit") { editing = true }
                }
            }
        }
        .sheet(isPresented: $editing) {
            if let e = entry {
                JournalEditView(entry: e, onSaved: { entry = $0 }, onDeleted: { dismiss() })
            }
        }
        .sheet(isPresented: $appending) {
            AppendNoteView(entryId: id) { Task { await load() } }
        }
        .task(id: id) { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    private func load() async {
        do {
            entry = try await client.entry(id)
            error = nil
        } catch TJError.http(404, _) {
            if entry != nil { dismiss() } else { self.error = "This note is gone." }
        } catch {
            if entry == nil { self.error = error.localizedDescription }
        }
    }
}
