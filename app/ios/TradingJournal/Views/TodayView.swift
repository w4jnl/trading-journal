// Today: the realised P&L as the hero, the open positions with their unrealised, the day plan's
// budget, the nudges, equity, and the cumulative line. Fed by Client.today over the stream.
import Charts
import SwiftUI

struct TodayView: View {
    @Environment(Client.self) private var client
    @Environment(Settings.self) private var settings

    var body: some View {
        ScrollView {
            if let t = client.today {
                VStack(alignment: .leading, spacing: 12) {
                    hero(t)
                    if !t.positions.isEmpty { positions(t) }
                    plan(t)
                    if !t.nudges.items.isEmpty { nudges(t) }
                    if !t.curve.isEmpty { curve(t) }
                    equity(t)
                }
                .padding(16)
            } else if client.connection == .off {
                ContentUnavailableView("Not paired", systemImage: "iphone.and.arrow.forward", description: Text("Pair this phone with the journal from its Settings page."))
            } else if let e = client.lastError {
                ContentUnavailableView("No answer from the journal", systemImage: "wifi.exclamationmark", description: Text(e))
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 200)
            }
        }
        .background(Brand.bg)
        .refreshable { await client.refreshToday(refresh: true) }
        .navigationTitle(client.today.map { Formatting.dayLabel($0.day, tz: $0.tz) } ?? "Today")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .topBarLeading) { ConnectionLine() }
            ToolbarItem(placement: .topBarTrailing) {
                Button { Task { await client.refreshToday(refresh: true) } } label: { Label("Refresh", systemImage: "arrow.clockwise") }
                    .keyboardShortcut("r", modifiers: .command)
            }
            ToolbarItem(placement: .topBarTrailing) {
                NavigationLink(value: Route.nudges) {
                    Image(systemName: "bell").overlay(alignment: .topTrailing) {
                        if let n = client.today?.nudges.unseen, n > 0 {
                            Text("\(n)").font(Brand.text(9, weight: .bold)).foregroundStyle(.white).padding(2).background(Brand.neg).clipShape(Circle()).offset(x: 6, y: -6)
                        }
                    }
                }
                .accessibilityLabel("Nudges")
            }
        }
    }

    // MARK: cards

    private func hero(_ t: Today) -> some View {
        Card {
            HStack(alignment: .firstTextBaseline) {
                CardTitle("Today", trailing: t.session.inRth ? "RTH" : t.session.inGlobex ? "Globex" : "closed")
            }
            PnlChip(t.stats.net, size: .hero)
            HStack(spacing: 8) {
                Text("open").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                PnlChip(t.positions.isEmpty ? nil : t.unrealized.total, size: .md, dashed: t.unrealized.stale)
                if t.unrealized.stale { Text("stale").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle) }
                if t.unrealized.unpriced > 0 { Text("\(t.unrealized.unpriced) unpriced").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle) }
                Spacer()
            }
            StatStrip(stats: [
                .init(label: "trades", value: "\(t.stats.trades)"),
                .init(label: "wins", value: "\(t.stats.wins)", color: t.stats.wins > 0 ? Brand.posText : nil),
                .init(label: "losses", value: "\(t.stats.losses)", color: t.stats.losses > 0 ? Brand.negText : nil),
                .init(label: "win rate", value: Formatting.percent(t.stats.winRate)),
                .init(label: "fees", value: String(format: "%.2f", t.stats.commission)),
            ])
            if let last = t.lastTrade {
                NavigationLink(value: Route.trade(last.id)) {
                    HStack {
                        Text("last").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                        Text("\(last.symbolRoot) \(last.direction.label)").font(Brand.text(13, weight: .medium, relativeTo: .footnote))
                        PnlChip(last.netPnl, size: .sm)
                        Text(Formatting.clock(last.exitTime, tz: t.tz)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                        Spacer()
                        Image(systemName: "chevron.right").font(.caption).foregroundStyle(Brand.fgSubtle)
                    }
                }
                .buttonStyle(.plain)
            } else if let fill = t.lastFillAt {
                Text("last fill \(Formatting.clock(fill, tz: t.tz))").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
            }
        }
    }

    private func positions(_ t: Today) -> some View {
        Card {
            CardTitle("Open positions", trailing: "\(t.positions.count)")
            ForEach(t.positions) { p in
                NavigationLink(value: Route.position(account: p.accountId, symbol: p.symbol)) {
                    PositionRow(position: p, tz: t.tz)
                }
                .buttonStyle(.plain)
                if p.id != t.positions.last?.id { Divider() }
            }
        }
    }

    private func plan(_ t: Today) -> some View {
        NavigationLink(value: Route.plan) {
            Card(keyline: t.plan.plan == nil ? Brand.warn : nil) {
                CardTitle("Day plan", trailing: t.plan.plan.map { $0.bias.label } ?? "")
                if let p = t.plan.plan {
                    HStack {
                        if let m = t.plan.maxTrades {
                            Text("\(t.plan.tradesUsed) of \(m) trades").font(Brand.mono(13))
                                .foregroundStyle(t.plan.tradesUsed >= m ? Brand.negText : Brand.fg)
                        } else {
                            Text("\(t.plan.tradesUsed) trades").font(Brand.mono(13))
                        }
                        Spacer()
                        if !p.stopTime.isEmpty { Text("stop \(p.stopTime)").font(Brand.mono(13)).foregroundStyle(Brand.fgMuted) }
                    }
                    if let f = t.plan.lossFraction, let m = t.plan.maxLoss {
                        ProgressView(value: f).tint(f >= 1 ? Brand.neg : f >= 0.8 ? Brand.warn : Brand.accent)
                        Text("\(Formatting.moneyShort(-t.plan.lossUsed)) of \(Formatting.moneyShort(-m)) max loss").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                    }
                    if let o = t.plan.outcome {
                        HStack(spacing: 6) {
                            ForEach(o.checks) { c in
                                Pill(c.key.replacingOccurrences(of: "-", with: " "), c.held ? .success : .error)
                            }
                        }
                    }
                } else {
                    Text("No plan yet").font(Brand.text(15, relativeTo: .body)).foregroundStyle(Brand.warnText)
                    Text("Write a bias, a max number of trades, a stop time and a max loss before the open.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
                }
            }
        }
        .buttonStyle(.plain)
    }

    private func nudges(_ t: Today) -> some View {
        Card(keyline: t.nudges.unseen > 0 ? Brand.warn : nil) {
            CardTitle("Nudges", trailing: t.nudges.unseen > 0 ? "\(t.nudges.unseen) unseen" : "")
            ForEach(t.nudges.items.prefix(3)) { n in
                NudgeRow(nudge: n, tz: t.tz)
            }
            if t.nudges.items.count > 3 {
                NavigationLink("All of today's nudges", value: Route.nudges).font(Brand.text(13, relativeTo: .footnote))
            }
        }
    }

    private func curve(_ t: Today) -> some View {
        Card {
            CardTitle("Cumulative P&L")
            let final = t.curve.last?.net ?? 0
            let color = final < 0 ? Brand.neg : Brand.pos
            Chart(t.curve, id: \.t) { p in
                LineMark(x: .value("time", p.t), y: .value("net", p.net)).foregroundStyle(color).interpolationMethod(.stepEnd)
                AreaMark(x: .value("time", p.t), y: .value("net", p.net)).foregroundStyle(color.opacity(0.16)).interpolationMethod(.stepEnd)
            }
            .chartXAxis { AxisMarks(values: .automatic(desiredCount: 4)) { v in AxisValueLabel(format: .dateTime.hour().minute()) } }
            .chartYAxis { AxisMarks(position: .leading) }
            .frame(height: 140)
        }
    }

    private func equity(_ t: Today) -> some View {
        NavigationLink(value: Route.accounts) {
            Card {
                CardTitle("Equity", trailing: t.equity.known ? "" : "no balances yet")
                HStack(alignment: .firstTextBaseline) {
                    Text(Formatting.balance(t.equity.known ? t.equity.now : nil)).font(Brand.mono(24, bold: true))
                    Spacer()
                    PnlChip(t.equity.known ? t.equity.dayChange : nil, size: .md)
                }
                if !t.equity.byType.isEmpty {
                    Text(t.equity.byType.map { "\($0.label) \(Formatting.balance($0.total))" }.joined(separator: " · ")).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted).lineLimit(2)
                }
                if t.equity.underwater {
                    Text("drawdown \(Formatting.moneyShort(-t.equity.currentDrawdown))").font(Brand.mono(12)).foregroundStyle(Brand.negText)
                }
            }
        }
        .buttonStyle(.plain)
    }
}

/// One open position: symbol, direction, size at average, last, unrealised, the stop distance.
struct PositionRow: View {
    let position: Position
    let tz: TimeZone

    var body: some View {
        let p = position
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(p.symbol).font(Brand.text(15, weight: .semibold, relativeTo: .body))
                Pill.direction(p.direction)
                if p.setLegs.count > 1 { Pill("×\(p.setLegs.count)", .group) }
                if p.addedFills > 0 { Pill("scaled in ×\(p.addedFills)", .sunken) }
                Spacer()
                PnlChip(p.unrealized, size: .md, dashed: p.stale)
            }
            HStack(spacing: 10) {
                Text("\(Formatting.qty(p.qty)) @ \(Formatting.price(p.avgEntry, tickSize: p.tickSize))").font(Brand.mono(12))
                if let last = p.lastPrice { Text("last \(Formatting.price(last, tickSize: p.tickSize))").font(Brand.mono(12)).foregroundStyle(Brand.fgMuted) }
                Spacer()
                if p.beyondStop {
                    Text("through stop").font(Brand.text(11, weight: .medium, relativeTo: .caption2)).foregroundStyle(Brand.negText)
                } else if let ticks = p.ticksToStop {
                    Text("\(Formatting.ticks(ticks)) to stop").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgMuted)
                } else {
                    Text("no stop").font(Brand.text(11, weight: .medium, relativeTo: .caption2)).foregroundStyle(Brand.warnText)
                }
            }
            Text(p.account).font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle).lineLimit(1)
        }
        .padding(.vertical, 4)
        .contentShape(Rectangle())
    }
}

struct NudgeRow: View {
    let nudge: Nudge
    let tz: TimeZone
    @Environment(Client.self) private var client

    var body: some View {
        let n = nudge
        NavigationLink(value: Route.of(n)) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .firstTextBaseline) {
                    Circle().fill(n.isWarning ? Brand.warn : Brand.info).frame(width: 6, height: 6)
                    Text(n.title).font(Brand.text(14, weight: n.unseen ? .semibold : .regular, relativeTo: .subheadline))
                    Spacer()
                    Text(Formatting.clock(n.createdAt, tz: tz)).font(Brand.mono(11)).foregroundStyle(Brand.fgSubtle)
                }
                Text(n.detail).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted).lineLimit(3)
                if let l = n.lesson, !l.isEmpty {
                    Text("Your lesson: \(l)").font(Brand.text(12, relativeTo: .caption)).italic().foregroundStyle(Brand.fgMuted).lineLimit(2)
                }
            }
            .padding(.vertical, 4)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .swipeActions(edge: .trailing) {
            if n.unseen {
                Button("Seen") { Task { try? await client.markSeen(n.id) } }.tint(Brand.accent)
            }
        }
    }
}
