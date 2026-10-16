// One open position: the plan (stop, risk, target, R:R, thesis), the cycle's fills, the legs
// of a copied decision. Reads from Today; a position that is no longer open says so.
import SwiftUI

struct PositionDetailView: View {
    let accountId: Int
    let symbol: String
    @Environment(Client.self) private var client
    @State private var editing = false

    private var position: Position? {
        client.today?.positions.first { $0.accountId == accountId && $0.symbol == symbol }
            ?? client.today?.positions.first { $0.symbol == symbol && $0.setLegs.contains(accountId) }
    }

    var body: some View {
        ScrollView {
            if let p = position, let t = client.today {
                VStack(alignment: .leading, spacing: 12) {
                    Card {
                        HStack {
                            Text(p.symbol).font(Brand.text(22, weight: .semibold, relativeTo: .title2))
                            Pill.direction(p.direction)
                            if p.setLegs.count > 1 { Pill("\(p.setLegs.count) legs", .group) }
                            Spacer()
                        }
                        PnlChip(p.unrealized, size: .lg, dashed: p.stale)
                        StatStrip(stats: [
                            .init(label: "size", value: Formatting.qty(p.qty)),
                            .init(label: "avg", value: Formatting.price(p.avgEntry, tickSize: p.tickSize)),
                            .init(label: "last", value: Formatting.price(p.lastPrice, tickSize: p.tickSize)),
                            .init(label: "opened", value: Formatting.clock(p.openedAt, tz: t.tz)),
                        ])
                        Text("\(p.account) · prices from \(p.priceSourceName)\(p.stale ? " (stale)" : "")").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                    }
                    Card(keyline: p.hasPlan ? nil : Brand.warn) {
                        CardTitle("Plan", trailing: p.hasPlan ? (p.isSuggestion ? "suggested by \(p.planSourceName)" : "yours") : "")
                        if p.hasPlan {
                            StatStrip(stats: [
                                .init(label: "stop", value: Formatting.price(p.stop, tickSize: p.tickSize), color: p.beyondStop ? Brand.negText : nil),
                                .init(label: "risk", value: Formatting.money(p.risk.map { -$0 }), color: Brand.negText),
                                .init(label: "target", value: Formatting.price(p.target, tickSize: p.tickSize)),
                                .init(label: "R:R", value: p.rewardRisk.map { String(format: "%.1f", $0) } ?? "—"),
                            ])
                            if p.beyondStop {
                                Text("Price is through the stop.").font(Brand.text(13, weight: .medium, relativeTo: .footnote)).foregroundStyle(Brand.negText)
                            } else if let ticks = p.ticksToStop {
                                Text("\(Formatting.ticks(ticks)) to the stop").font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted)
                            }
                            if !p.thesisText.isEmpty { Text(p.thesisText).font(Brand.text(14, relativeTo: .subheadline)) }
                            if p.isSuggestion { Pill("suggested: save it to make it yours", .suggest) }
                        } else {
                            Text("No stop on this position.").font(Brand.text(15, relativeTo: .body)).foregroundStyle(Brand.warnText)
                        }
                        Button { editing = true } label: { Label(p.hasPlan ? "Edit the plan" : "Set stop and target", systemImage: "pencil") }.font(Brand.text(14, relativeTo: .subheadline))
                    }
                    Card {
                        CardTitle("Fills this cycle", trailing: p.addedFills > 0 ? "scaled in ×\(p.addedFills)" : "")
                        ForEach(Array(p.cycleFills.enumerated()), id: \.offset) { _, f in
                            HStack {
                                Text(Formatting.clock(f.time, tz: t.tz)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                                Pill(f.side.rawValue, f.side == .buy ? .info : .warn)
                                Text(Formatting.qty(f.qty)).font(Brand.mono(13))
                                Spacer()
                                Text(Formatting.price(f.price, tickSize: p.tickSize)).font(Brand.mono(13))
                            }
                        }
                        if p.addedFills > 0 {
                            Text("Opened at \(Formatting.price(p.openPrice, tickSize: p.tickSize)); the average moved to \(Formatting.price(p.avgEntry, tickSize: p.tickSize)) with \(Formatting.qty(p.addedQty)) added.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                        }
                    }
                    if p.setLegs.count > 1 {
                        Card {
                            CardTitle("Copied decision", trailing: "\(p.setLegs.count) accounts")
                            ForEach(t.positions.filter { $0.symbol == p.symbol && p.setLegs.contains($0.accountId) }) { leg in
                                NavigationLink(value: Route.position(account: leg.accountId, symbol: leg.symbol)) {
                                    HStack {
                                        Text(leg.account).font(Brand.text(13, relativeTo: .footnote)).lineLimit(1)
                                        Spacer()
                                        Text("\(Formatting.qty(leg.qty)) @ \(Formatting.price(leg.avgEntry, tickSize: leg.tickSize))").font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                                        PnlChip(leg.unrealized, size: .sm)
                                    }
                                }
                                .buttonStyle(.plain)
                                .disabled(leg.accountId == p.accountId)
                            }
                        }
                    }
                }
                .padding(16)
            } else if client.today == nil {
                ProgressView().frame(maxWidth: .infinity, minHeight: 200)
            } else {
                ContentUnavailableView("Position closed", systemImage: "checkmark.circle", description: Text("\(symbol) is no longer open on this account. Its trade is in the log."))
            }
        }
        .background(Brand.bg)
        .navigationTitle(symbol)
        .navigationBarTitleDisplayMode(.inline)
        .sheet(isPresented: $editing) {
            if let p = position { PositionPlanEditView(position: p) }
        }
    }
}
