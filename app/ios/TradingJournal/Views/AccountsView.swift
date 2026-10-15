// Accounts and equity: equity now by type, the equity curve, the drawdown, one row per account
// with its running balance. Nudges and the day plan have their own small screens here too.
import Charts
import SwiftUI

struct AccountsView: View {
    @Environment(Client.self) private var client
    @State private var accounts: [Account] = []
    @State private var equity: Equity?
    @State private var error: String?
    @State private var showRetired = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let e = equity {
                    Card {
                        CardTitle("Equity now", trailing: e.excluded > 0 ? "\(e.excluded) without an anchor" : "")
                        HStack(alignment: .firstTextBaseline) {
                            Text(Formatting.balance(e.equityNow)).font(Brand.mono(28, bold: true))
                            Spacer()
                            PnlChip(e.dayChange, size: .md)
                        }
                        ForEach(e.byType) { t in
                            HStack {
                                Text(t.label).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted)
                                Text("\(t.accounts)").font(Brand.mono(11)).foregroundStyle(Brand.fgSubtle)
                                Spacer()
                                Text(Formatting.balance(t.total)).font(Brand.mono(13))
                            }
                        }
                    }
                    if e.series.count > 1 {
                        Card {
                            CardTitle("Equity curve", trailing: "since \(e.series.first?.date ?? "")")
                            Chart(e.series, id: \.date) { p in
                                if let d = Formatting.parseDay(p.date, tz: client.tz) {
                                    LineMark(x: .value("day", d), y: .value("equity", p.equity)).foregroundStyle(Brand.accent)
                                }
                            }
                            .chartYScale(domain: .automatic(includesZero: false))
                            .chartXAxis { AxisMarks(values: .automatic(desiredCount: 4)) { _ in AxisValueLabel(format: .dateTime.month(.abbreviated)) } }
                            .frame(height: 140)
                        }
                    }
                    Card(keyline: e.drawdown.underwater ? Brand.neg : nil) {
                        CardTitle("Drawdown")
                        StatStrip(stats: [
                            .init(label: "max", value: Formatting.moneyShort(-e.drawdown.maxDrawdown), color: Brand.negText),
                            .init(label: "days", value: "\(e.drawdown.maxDrawdownDays)"),
                            .init(label: "current", value: e.drawdown.underwater ? Formatting.moneyShort(-e.drawdown.currentDrawdown) : "none", color: e.drawdown.underwater ? Brand.negText : Brand.fgMuted),
                        ])
                        if let peak = e.drawdown.maxDrawdownPeakAt, let trough = e.drawdown.maxDrawdownTroughAt {
                            Text("Max from \(Formatting.day(peak, tz: client.tz)) to \(Formatting.day(trough, tz: client.tz))\(e.drawdown.maxDrawdownRecoverAt.map { ", recovered \(Formatting.day($0, tz: client.tz))" } ?? ", not recovered")")
                                .font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                        }
                        if e.drawdown.underwater, let p = e.drawdown.peakAt {
                            Text("Underwater since \(Formatting.day(p, tz: client.tz))").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.negText)
                        }
                    }
                }
                Card {
                    CardTitle("Accounts", trailing: "\(accounts.filter { showRetired || !$0.retired }.count)")
                    ForEach(accounts.filter { showRetired || !$0.retired }) { a in
                        AccountRow(account: a)
                        if a.id != accounts.last?.id { Divider() }
                    }
                    Toggle("Show retired", isOn: $showRetired).font(Brand.text(13, relativeTo: .footnote)).tint(Brand.accent)
                        .onChange(of: showRetired) { _, _ in Task { await load() } }
                }
                if let error { Text(error).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.negText) }
            }
            .padding(16)
        }
        .background(Brand.bg)
        .navigationTitle("Accounts")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load() }
        .task { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    private func load() async {
        async let a = client.accounts(showRetired: showRetired)
        async let e = client.equity()
        do {
            accounts = try await a
            equity = try await e
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

struct AccountRow: View {
    let account: Account
    var body: some View {
        let a = account
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(a.name).font(Brand.text(14, weight: .medium, relativeTo: .subheadline)).lineLimit(1).truncationMode(.middle).foregroundStyle(a.retired ? Brand.fgMuted : Brand.fg)
                Spacer(minLength: 8)
                Text(Formatting.balance(a.balance?.balance)).font(Brand.mono(14)).lineLimit(1).foregroundStyle(a.balance == nil ? Brand.fgSubtle : Brand.fg).layoutPriority(1)
            }
            HStack(spacing: 6) {
                if let f = a.firm { Pill(f, .sunken) }
                Pill.accountType(a)
                if a.retired { Pill("retired", .retired) }
                if a.excludedFromStats { Pill("excluded", .sunken) }
                Spacer()
                if let d = a.dayNet { PnlChip(d, size: .sm) }
            }
            if let b = a.balance {
                Text("as of \(b.asOf) · anchor \(b.anchorDate)").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
            } else {
                Text("no balance anchor").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.warnText)
            }
        }
        .padding(.vertical, 4)
    }
}

/// Today's nudges (or any day's) with the seen state; a swipe marks one seen.
struct NudgesView: View {
    @Environment(Client.self) private var client
    @State private var list: NudgeList?
    @State private var error: String?

    var body: some View {
        List {
            if let l = list {
                if l.items.isEmpty {
                    ContentUnavailableView("No nudges today", systemImage: "bell.slash", description: Text("The guardrail had nothing to say."))
                }
                ForEach(l.items) { n in NudgeRow(nudge: n, tz: client.tz) }
            } else if let error {
                ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
            }
        }
        .listStyle(.plain)
        .background(Brand.bg)
        .scrollContentBackground(.hidden)
        .navigationTitle("Nudges")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            if let l = list, l.unseen > 0 {
                Button("Seen") { Task { for n in l.items where n.unseen { try? await client.markSeen(n.id) }; await load() } }
            }
        }
        .refreshable { await load() }
        .task { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    private func load() async {
        do {
            list = try await client.nudges(day: "today")
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
    }
}

/// The day plan as the guardrail reads it; editing it comes with the writes phase.
struct PlanView: View {
    @Environment(Client.self) private var client

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let t = client.today {
                    if let p = t.plan.plan {
                        Card {
                            CardTitle("Plan for \(Formatting.dayLabel(p.day, tz: t.tz))", trailing: p.bias.label)
                            StatStrip(stats: [
                                .init(label: "max trades", value: p.maxTrades.map(String.init) ?? "—"),
                                .init(label: "stop time", value: p.stopTime.isEmpty ? "—" : p.stopTime),
                                .init(label: "max loss", value: Formatting.moneyShort(p.maxLoss.map { -$0 }), color: Brand.negText),
                            ])
                            if !p.levels.isEmpty {
                                Text("Levels").font(Brand.text(11, weight: .medium, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                                Text(p.levels).font(Brand.mono(13))
                            }
                            if !p.notes.isEmpty { MarkdownView(source: p.notes) }
                            Text("Written \(Formatting.clock(p.createdAt, tz: t.tz))\(p.updatedAt != p.createdAt ? ", edited \(Formatting.clock(p.updatedAt, tz: t.tz))" : "")").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                        }
                        if let o = t.plan.outcome {
                            Card {
                                CardTitle("How it held", trailing: "\(o.held) of \(o.checks.count)")
                                ForEach(o.checks) { c in
                                    HStack(alignment: .firstTextBaseline) {
                                        Image(systemName: c.held ? "checkmark.circle.fill" : "xmark.circle.fill").foregroundStyle(c.held ? Brand.pos : Brand.neg)
                                        VStack(alignment: .leading, spacing: 1) {
                                            Text(c.rule).font(Brand.text(14, relativeTo: .subheadline))
                                            Text(c.actual).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                                        }
                                    }
                                }
                                if o.writtenLate { Text("Written after the open.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.warnText) }
                                if o.editedLate { Text("Edited after the open.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.warnText) }
                            }
                        }
                    } else {
                        ContentUnavailableView("No plan for today", systemImage: "doc.text", description: Text("Write it on the web before the open; writing it here comes with the next build."))
                    }
                } else {
                    ProgressView()
                }
            }
            .padding(16)
        }
        .background(Brand.bg)
        .navigationTitle("Day plan")
        .navigationBarTitleDisplayMode(.inline)
    }
}
