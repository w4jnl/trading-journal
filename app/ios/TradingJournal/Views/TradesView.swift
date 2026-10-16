// The trade log: pages of 50 from /trades grouped by exit day, a filter bar for symbol,
// direction, outcome and a range preset, pull to refresh.
import SwiftUI

struct TradesView: View {
    /// On an iPad the list drives the detail column instead of pushing.
    var selection: Binding<Route?>?
    @Environment(Client.self) private var client
    @Environment(Settings.self) private var settings
    @State private var filter: Client.TradeFilter
    @FocusState private var symbolFocused: Bool

    init(initial: Client.TradeFilter = .init(), selection: Binding<Route?>? = nil) {
        _filter = State(initialValue: initial)
        self.selection = selection
    }
    @State private var rows: [TradeRow] = []
    @State private var page: PageInfo?
    @State private var loading = false
    @State private var error: String?
    @State private var range = Range.all

    enum Range: String, CaseIterable, Identifiable {
        case today, week, month, all
        var id: String { rawValue }
        var title: String {
            switch self {
            case .today: "Today"
            case .week: "7 days"
            case .month: "30 days"
            case .all: "All"
            }
        }
    }

    private var sections: [(day: String, rows: [TradeRow])] {
        var out: [(String, [TradeRow])] = []
        for r in (settings.tradesBySet ? Self.bySet(rows) : rows) {
            let d = Formatting.day(r.when, tz: client.tz)
            if out.last?.0 == d { out[out.count - 1].1.append(r) } else { out.append((d, [r])) }
        }
        return out
    }

    /// The Sets view: a copied decision's legs fold into one row with the money summed, the
    /// size summed and the lead leg's prices; a trade on one account stays as it is.
    nonisolated static func bySet(_ rows: [TradeRow]) -> [TradeRow] {
        var out: [TradeRow] = []
        var index: [Int: Int] = [:] // set id → position in out
        for r in rows {
            guard let set = r.setId, r.setSize > 1 else {
                out.append(r)
                continue
            }
            if let i = index[set] {
                out[i].qty += r.qty
                out[i].netPnl = (out[i].netPnl ?? 0) + (r.netPnl ?? 0)
                out[i].grossPnl = (out[i].grossPnl ?? 0) + (r.grossPnl ?? 0)
                out[i].commission = (out[i].commission ?? 0) + (r.commission ?? 0)
                out[i].groupCount += 1
                out[i].hasJournal = out[i].hasJournal || r.hasJournal
            } else {
                var lead = r
                lead.groupCount = 1
                index[set] = out.count
                out.append(lead)
            }
        }
        return out
    }

    var body: some View {
        Group {
            // A phone's list pushes through its links; a selection binding would swallow the taps.
            if let selection { List(selection: selection) { content } } else { List { content } }
        }
        .listStyle(.plain)
        .background(Brand.bg)
        .scrollContentBackground(.hidden)
        .navigationTitle("Trades")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Menu {
                    Toggle("Group by set", isOn: Binding(get: { settings.tradesBySet }, set: { settings.tradesBySet = $0 }))
                    Button("Find a symbol") { symbolFocused = true }.keyboardShortcut("f", modifiers: .command)
                } label: { Label("View", systemImage: "square.grid.2x2") }
            }
        }
        .refreshable { await load() }
        .task { if rows.isEmpty { await load() } }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    @ViewBuilder private var content: some View {
        Section {
            filters
        }
        .listRowBackground(Brand.bg)
        .listRowInsets(EdgeInsets(top: 0, leading: 16, bottom: 4, trailing: 16))
        if let error {
            ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
        } else if rows.isEmpty && !loading {
            ContentUnavailableView("No trades", systemImage: "list.bullet.rectangle", description: Text("Nothing matches these filters."))
        }
        ForEach(sections, id: \.day) { s in
            Section {
                ForEach(s.rows) { r in
                    if selection != nil {
                        TradeRowView(trade: r, tz: client.tz, set: settings.tradesBySet).tag(Route.trade(r.id))
                    } else {
                        NavigationLink(value: Route.trade(r.id)) { TradeRowView(trade: r, tz: client.tz, set: settings.tradesBySet) }
                    }
                }
            } header: {
                HStack {
                    Text(Formatting.dayLabel(s.day, tz: client.tz)).font(Brand.text(12, weight: .medium, relativeTo: .caption))
                    Spacer()
                    PnlChip(s.rows.compactMap(\.netPnl).reduce(0, +), size: .sm)
                }
            }
        }
        if let page, page.page < page.pages {
            Button {
                filter.page += 1
                Task { await load(append: true) }
            } label: {
                HStack { Spacer(); Text(loading ? "Loading…" : "More (\(page.total - rows.count) left)").font(Brand.text(13, relativeTo: .footnote)); Spacer() }
            }
            .disabled(loading)
        }
    }

    private var filters: some View {
        VStack(spacing: 8) {
            Picker("Range", selection: $range) {
                ForEach(Range.allCases) { Text($0.title).tag($0) }
            }
            .pickerStyle(.segmented)
            .onChange(of: range) { _, r in
                switch r {
                case .today: filter.from = "today"; filter.to = "today"
                case .week: filter.from = Formatting.day(Date().addingTimeInterval(-6 * 86400), tz: client.tz); filter.to = "today"
                case .month: filter.from = Formatting.day(Date().addingTimeInterval(-29 * 86400), tz: client.tz); filter.to = "today"
                case .all: filter.from = ""; filter.to = ""
                }
                Task { await load() }
            }
            HStack(spacing: 8) {
                TextField("symbol", text: $filter.symbolRoot)
                    .focused($symbolFocused)
                    .textInputAutocapitalization(.characters).autocorrectionDisabled()
                    .font(Brand.mono(13))
                    .padding(.horizontal, 8).padding(.vertical, 5)
                    .background(Brand.bgSunken).clipShape(RoundedRectangle(cornerRadius: 4))
                    .frame(maxWidth: 110)
                    .onSubmit { Task { await load() } }
                Menu {
                    Picker("Direction", selection: $filter.direction) {
                        Text("any direction").tag(Direction?.none)
                        Text("long").tag(Direction?.some(.long))
                        Text("short").tag(Direction?.some(.short))
                    }
                    Picker("Outcome", selection: $filter.outcome) {
                        Text("any outcome").tag("")
                        Text("wins").tag("win")
                        Text("losses").tag("loss")
                        Text("flat").tag("flat")
                        Text("open").tag("open")
                    }
                    Picker("Sort", selection: $filter.sort) {
                        Text("newest first").tag("")
                        Text("by net").tag("net")
                        Text("by R").tag("r")
                    }
                } label: {
                    Label(filterLabel, systemImage: "line.3.horizontal.decrease.circle").font(Brand.text(13, relativeTo: .footnote))
                }
                .onChange(of: filter.direction) { _, _ in Task { await load() } }
                .onChange(of: filter.outcome) { _, _ in Task { await load() } }
                .onChange(of: filter.sort) { _, _ in filter.dir = filter.sort.isEmpty ? "" : "desc"; Task { await load() } }
                Spacer()
                if let page { Text("\(page.total)").font(Brand.mono(12)).foregroundStyle(Brand.fgSubtle) }
            }
        }
        .padding(.vertical, 6)
    }

    private var filterLabel: String {
        var parts: [String] = []
        if let d = filter.direction { parts.append(d.label) }
        if !filter.outcome.isEmpty { parts.append(filter.outcome) }
        if !filter.sort.isEmpty { parts.append("by \(filter.sort)") }
        return parts.isEmpty ? "Filter" : parts.joined(separator: " · ")
    }

    private func load(append: Bool = false) async {
        if !append { filter.page = 1 }
        loading = true
        defer { loading = false }
        do {
            let list = try await client.trades(filter)
            rows = append ? rows + list.items : list.items
            page = list.page
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct TradeRowView: View {
    let trade: TradeRow
    let tz: TimeZone
    var set = false

    var body: some View {
        let t = trade
        HStack(spacing: 8) {
            Text(Formatting.clock(t.when, tz: tz)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted).frame(width: 42, alignment: .leading)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(t.symbol).font(Brand.text(14, weight: .semibold, relativeTo: .subheadline))
                    Pill.direction(t.direction)
                    Text(Formatting.qty(t.qty)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                    if t.setSize > 1 { Pill(set && t.groupCount > 0 ? "\(t.groupCount) of \(t.setSize)" : "×\(t.setSize)", .group) }
                    if t.isOpen { Pill("open", .accent) }
                }
                HStack(spacing: 6) {
                    Text(set && t.setSize > 1 ? "\(t.setSize) accounts, lead \(t.account)" : t.account).font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle).lineLimit(1)
                    if t.hasJournal { Image(systemName: "book.fill").font(.system(size: 9)).foregroundStyle(tagColor) }
                }
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 2) {
                PnlChip(t.netPnl, size: .md)
                if let r = t.r { Text(Formatting.r(r)).font(Brand.mono(11)).foregroundStyle(Brand.fgMuted) }
            }
        }
        .padding(.vertical, 2)
        .contentShape(Rectangle()) // the whole row takes the tap, the gap in the middle too
    }

    private var tagColor: Color {
        switch trade.journalTag ?? "" {
        case "good": Brand.pos
        case "bad": Brand.neg
        default: Brand.fgMuted
        }
    }
}
