// The calendar: a month of heat cells from /stats/daily with week rows and the month's totals;
// a day opens its stats, trades, note and nudges.
import SwiftUI

struct CalendarView: View {
    @Environment(Client.self) private var client
    @State private var month: Date = .now
    @State private var stats: DailyStats?
    @State private var error: String?

    private var cal: Calendar {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = client.tz
        c.firstWeekday = 2
        return c
    }

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                HStack {
                    Button { shift(-1) } label: { Image(systemName: "chevron.left") }.accessibilityLabel("Previous month")
                    Spacer()
                    Text(Formatting.dayLabel(Formatting.day(month, tz: client.tz), tz: client.tz, style: .month)).font(Brand.text(17, weight: .semibold, relativeTo: .title3))
                    Spacer()
                    Button { shift(1) } label: { Image(systemName: "chevron.right") }.accessibilityLabel("Next month").disabled(isCurrentMonth)
                }
                .padding(.horizontal, 4)
                if let s = stats {
                    grid(s)
                    Card {
                        CardTitle("Month")
                        HStack(alignment: .firstTextBaseline) {
                            PnlChip(s.totals.net, size: .lg)
                            Spacer()
                            Text("\(s.totals.winDays ?? 0) up · \(s.totals.lossDays ?? 0) down").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                        }
                        StatStrip(stats: [
                            .init(label: "trades", value: "\(s.totals.trades)"),
                            .init(label: "win rate", value: Formatting.percent(s.totals.winRate)),
                            .init(label: "best", value: Formatting.moneyShort(s.totals.bestDay?.net), color: Brand.posText),
                            .init(label: "worst", value: Formatting.moneyShort(s.totals.worstDay?.net), color: Brand.negText),
                        ])
                    }
                } else if let error {
                    ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
                } else {
                    ProgressView().frame(maxWidth: .infinity, minHeight: 200)
                }
            }
            .padding(16)
        }
        .background(Brand.bg)
        .navigationTitle("Calendar")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load() }
        .task(id: Formatting.day(month, tz: client.tz).prefix(7)) { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    private var isCurrentMonth: Bool { cal.isDate(month, equalTo: .now, toGranularity: .month) }

    private func shift(_ by: Int) {
        if let m = cal.date(byAdding: .month, value: by, to: month) { month = m }
    }

    /// Monday to Friday; the weekend columns come back only for a month that traded on one
    /// (a Sunday evening Globex session booked to the Sunday).
    private func grid(_ s: DailyStats) -> some View {
        let byDay = Dictionary(uniqueKeysWithValues: s.items.map { ($0.date, $0) })
        let scale = s.items.map { abs($0.net) }.max() ?? 0
        let first = cal.date(from: cal.dateComponents([.year, .month], from: month))!
        let days = cal.range(of: .day, in: .month, for: first)!.count
        let today = Formatting.day(.now, tz: client.tz)
        let dates = (1...days).map { cal.date(byAdding: .day, value: $0 - 1, to: first)! }
        func column(_ d: Date) -> Int { (cal.component(.weekday, from: d) + 5) % 7 } // Monday 0 ... Sunday 6
        let weekends = s.items.contains { column(Formatting.parseDay($0.date, tz: client.tz) ?? first) >= 5 }
        let width = weekends ? 7 : 5
        let shown = dates.filter { weekends || column($0) < 5 }
        let lead = shown.first.map(column) ?? 0
        let columns = Array(repeating: GridItem(.flexible(), spacing: 4), count: width)
        return VStack(spacing: 4) {
            HStack(spacing: 4) {
                ForEach(["Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"].prefix(width), id: \.self) { Text($0).font(Brand.text(10, weight: .medium, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle).frame(maxWidth: .infinity) }
            }
            LazyVGrid(columns: columns, spacing: 4) {
                ForEach(0..<lead, id: \.self) { _ in Color.clear.frame(minHeight: 48) }
                ForEach(shown, id: \.self) { date in
                    let key = Formatting.day(date, tz: client.tz)
                    let item = byDay[key]
                    NavigationLink(value: Route.day(key)) {
                        HeatCell(day: cal.component(.day, from: date), net: item?.net, trades: item?.trades ?? 0, scale: scale, isToday: key == today)
                    }
                    .buttonStyle(.plain)
                    .disabled(item == nil && key > today)
                }
            }
        }
    }

    private func load() async {
        let first = cal.date(from: cal.dateComponents([.year, .month], from: month))!
        let last = cal.date(byAdding: DateComponents(month: 1, day: -1), to: first)!
        do {
            stats = try await client.dailyStats(from: Formatting.day(first, tz: client.tz), to: Formatting.day(min(last, .now), tz: client.tz))
            error = nil
        } catch {
            if stats == nil { self.error = error.localizedDescription }
        }
    }
}

/// One trading day: its stats, trades, the day note and the nudges.
struct DayView: View {
    let day: String
    @Environment(Client.self) private var client
    @State private var stats: DailyStats?
    @State private var trades: [TradeRow] = []
    @State private var entries: [JournalRow] = []
    @State private var nudges: [Nudge] = []
    @State private var error: String?
    @State private var writing = false
    @State private var opened: Route?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let s = stats?.items.first {
                    Card {
                        CardTitle("Day")
                        PnlChip(s.net, size: .hero)
                        StatStrip(stats: [
                            .init(label: "trades", value: "\(s.trades)"),
                            .init(label: "wins", value: "\(s.wins)", color: Brand.posText),
                            .init(label: "losses", value: "\(s.losses)", color: Brand.negText),
                            .init(label: "win rate", value: Formatting.percent(s.winRate)),
                            .init(label: "fees", value: String(format: "%.2f", s.commission)),
                        ])
                    }
                } else if stats != nil {
                    Card { Text("No trades on this day.").font(Brand.text(14, relativeTo: .subheadline)).foregroundStyle(Brand.fgMuted) }
                }
                Card {
                    CardTitle("Notes", trailing: "\(entries.count)")
                    ForEach(entries) { e in
                        NavigationLink(value: Route.journal(e.id)) { JournalRowView(row: e, tz: client.tz) }.buttonStyle(.plain)
                    }
                    if stats != nil, !entries.contains(where: { $0.scope == .day }) {
                        Button { writing = true } label: { Label("Write the day note", systemImage: "square.and.pencil") }.font(Brand.text(14, relativeTo: .subheadline))
                    }
                }
                if !nudges.isEmpty {
                    Card(keyline: Brand.warn) {
                        CardTitle("Nudges", trailing: "\(nudges.count)")
                        ForEach(nudges) { n in NudgeRow(nudge: n, tz: client.tz, here: .day(day)) }
                    }
                }
                if !trades.isEmpty {
                    Card {
                        CardTitle("Trades", trailing: "\(trades.count)")
                        ForEach(trades) { t in
                            NavigationLink(value: Route.trade(t.id)) { TradeRowView(trade: t, tz: client.tz) }.buttonStyle(.plain)
                            if t.id != trades.last?.id { Divider() }
                        }
                    }
                }
                if let error { Text(error).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.negText) }
            }
            .padding(16)
        }
        .background(Brand.bg)
        .navigationTitle(Formatting.dayLabel(day, tz: client.tz))
        .navigationBarTitleDisplayMode(.inline)
        .task(id: day) { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
        .sheet(isPresented: $writing) {
            NewEntryView(scope: .day, date: day, onCreated: { opened = .journal($0.id) }, onExists: { opened = .journal($0) })
        }
        .navigationDestination(item: $opened) { r in
            if case .journal(let j) = r { JournalEntryView(id: j) }
        }
    }

    private func load() async {
        async let s = client.dailyStats(from: day, to: day)
        async let t = client.trades(.init(from: day, to: day))
        async let j = client.journal(.init(from: day, to: day))
        async let n = client.nudges(day: day)
        do {
            stats = try await s
            trades = try await t.items
            entries = try await j
            nudges = try await n.items
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}
