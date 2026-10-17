// One trade: the P&L block, the plan and excursion numbers, the server's chart, the fills, and
// the way to its journal note.
import SwiftUI

struct TradeDetailView: View {
    let id: Int
    @Environment(Client.self) private var client
    @State private var trade: TradeDetail?
    @State private var error: String?
    @State private var asOf: Date?
    @State private var writing = false
    @State private var opened: Route?
    @State private var aiError: String?
    @State private var starting = false

    var body: some View {
        ScrollView {
            if let t = trade {
                VStack(alignment: .leading, spacing: 12) {
                    StaleBanner(asOf)
                    Card {
                        HStack {
                            Text(t.symbol).font(Brand.text(22, weight: .semibold, relativeTo: .title2))
                            Pill.direction(t.direction)
                            Text(Formatting.qty(t.qty)).font(Brand.mono(14)).foregroundStyle(Brand.fgMuted)
                            if t.setSize > 1 { Pill("×\(t.setSize) accounts", .group) }
                            if t.isOpen { Pill("open", .accent) }
                            Spacer()
                        }
                        PnlChip(t.netPnl, size: .lg)
                        StatStrip(stats: [
                            .init(label: "gross", value: Formatting.money(t.grossPnl)),
                            .init(label: "fees", value: t.commission.map { String(format: "%.2f", $0) } ?? "—"),
                            .init(label: "entry", value: Formatting.price(t.entryPrice, tickSize: tick(t))),
                            .init(label: "exit", value: Formatting.price(t.exitPrice, tickSize: tick(t))),
                        ])
                        Text("\(t.account) · \(Formatting.clock(t.entryTime, tz: client.tz)) → \(Formatting.clock(t.exitTime, tz: client.tz)) · \(Formatting.dayLabel(Formatting.day(t.exitTime ?? t.entryTime, tz: client.tz), tz: client.tz))")
                            .font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                    }
                    Card {
                        CardTitle("Plan and excursion", trailing: t.excursionBars.map { "\($0) bars" } ?? "")
                        StatStrip(stats: [
                            .init(label: "stop", value: Formatting.price(t.plannedStop, tickSize: tick(t))),
                            .init(label: "risk", value: Formatting.money(t.plannedRisk.map { -$0 }), color: t.plannedRisk == nil ? nil : Brand.negText),
                            .init(label: "R", value: Formatting.r(t.r), color: t.r.map { $0 < 0 ? Brand.negText : Brand.posText }),
                            .init(label: "exit eff.", value: Formatting.percent(t.exitEfficiency)),
                        ])
                        StatStrip(stats: [
                            .init(label: "MAE", value: Formatting.money(t.mae.map { -$0 }), color: Brand.negText),
                            .init(label: "MFE", value: Formatting.money(t.mfe), color: Brand.posText),
                            .init(label: "MAE pts", value: t.maePoints.map { String(format: "%.2f", $0) } ?? "—"),
                            .init(label: "MFE pts", value: t.mfePoints.map { String(format: "%.2f", $0) } ?? "—"),
                        ])
                        if t.plannedStop == nil { Text("No stop was recorded for this trade.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.warnText) }
                    }
                    if let png = t.chartPng, !png.isEmpty {
                        Card {
                            CardTitle("Chart")
                            AuthImage(href: png, maxHeight: 320)
                        }
                    }
                    Card {
                        CardTitle("Fills", trailing: "\(t.executions.count)")
                        ForEach(t.executions) { e in
                            HStack {
                                Text(Formatting.clock(e.time, tz: client.tz)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                                Pill(e.side.rawValue, e.side == .buy ? .info : .warn)
                                Text(e.role.rawValue).font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                                Text(Formatting.qty(e.qty)).font(Brand.mono(13))
                                Spacer()
                                Text(Formatting.price(e.price, tickSize: tick(t))).font(Brand.mono(13))
                            }
                        }
                    }
                    if let j = t.journalEntryId {
                        NavigationLink(value: Route.journal(j)) {
                            Card(keyline: Brand.accent) {
                                HStack {
                                    Label("Journal note", systemImage: "book").font(Brand.text(14, weight: .medium, relativeTo: .subheadline))
                                    if let tag = t.journalTag, !tag.isEmpty { Pill(tag, tag == "good" ? .success : tag == "bad" ? .error : .sunken) }
                                    Spacer()
                                    Image(systemName: "chevron.right").foregroundStyle(Brand.fgSubtle)
                                }
                            }
                        }
                        .buttonStyle(.plain)
                    } else {
                        Button { writing = true } label: {
                            Card {
                                HStack {
                                    Label("Write a note", systemImage: "square.and.pencil").font(Brand.text(14, weight: .medium, relativeTo: .subheadline)).foregroundStyle(Brand.accentText)
                                    Spacer()
                                }
                            }
                        }
                        .buttonStyle(.plain)
                        if client.aiReady {
                            // The review creates the note and lands in it; the note opens, shown as under review.
                            Button { Task { await analyze() } } label: {
                                Card {
                                    HStack {
                                        Label(starting ? "Starting the review…" : "Analyze with the AI", systemImage: "sparkles").font(Brand.text(14, weight: .medium, relativeTo: .subheadline)).foregroundStyle(Brand.accentText)
                                        Spacer()
                                    }
                                    if let aiError { Text(aiError).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.negText) }
                                }
                            }
                            .buttonStyle(.plain)
                            .disabled(starting)
                            .accessibilityIdentifier("analyze-trade")
                        }
                    }
                }
                .padding(16)
            } else if let error {
                ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 200)
            }
        }
        .background(Brand.bg)
        .navigationTitle(trade.map { "\($0.symbol) \($0.direction.label)" } ?? "Trade")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: id) { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
        .reloadsWhenLive(asOf != nil) { await load() }
        .sheet(isPresented: $writing) {
            NewEntryView(scope: .trade, tradeId: id, onCreated: { opened = .journal($0.id) }, onExists: { opened = .journal($0) })
        }
        .navigationDestination(item: $opened) { r in
            if case .journal(let j) = r { JournalEntryView(id: j) }
        }
    }

    private func analyze() async {
        starting = true
        defer { starting = false }
        do {
            let entryId = try await client.startReview(.trade(id))
            aiError = nil
            opened = .journal(entryId)
        } catch Client.AIError.reviewPending(let entryId) {
            if let entryId { opened = .journal(entryId) }
        } catch Client.AIError.off {
            aiError = "No AI provider: add your key in the web app's Settings > AI."
        } catch {
            aiError = error.localizedDescription
        }
    }

    /// The tick for price formatting: the open position's if known, else a guess from the prices.
    private func tick(_ t: TradeDetail) -> Double {
        if let p = client.today?.positions.first(where: { $0.symbolRoot == t.symbolRoot }) { return p.tickSize }
        let prices = [t.entryPrice, t.exitPrice ?? t.entryPrice] + t.executions.map(\.price)
        return prices.contains { $0 != $0.rounded(.towardZero) } ? 0.25 : 1
    }

    private func load() async {
        do {
            let l = try await client.trade(id)
            trade = l.value
            asOf = l.asOf
            error = nil
        } catch {
            if trade == nil { self.error = error.localizedDescription }
        }
    }
}
