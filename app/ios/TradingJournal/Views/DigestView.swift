// The weekly edge digest: the week's numbers against the week before, what to keep and what to
// fix, the best and worst decision, the coach's warnings, how the plans held, and the lessons
// the AI reviews named. One object from GET /digest; the week steps back and forward.
import SwiftUI

struct DigestView: View {
    var week: String?
    @Environment(Client.self) private var client
    @State private var digest: Digest?
    @State private var error: String?
    @State private var current: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                if let d = digest {
                    HStack {
                        Button { step(d, -1) } label: { Image(systemName: "chevron.left") }.accessibilityLabel("Previous week")
                        Spacer()
                        VStack(spacing: 1) {
                            Text(d.week).font(Brand.text(17, weight: .semibold, relativeTo: .title3))
                            Text("\(Formatting.dayLabel(d.from, tz: client.tz)) – \(Formatting.dayLabel(d.to, tz: client.tz)) · \(d.tradingDays) trading days").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgMuted)
                        }
                        Spacer()
                        Button { step(d, 1) } label: { Image(systemName: "chevron.right") }.accessibilityLabel("Next week")
                    }
                    .padding(.horizontal, 4)
                    Card {
                        CardTitle("The week", trailing: "against the week before")
                        let cols = [GridItem(.flexible()), GridItem(.flexible()), GridItem(.flexible())]
                        LazyVGrid(columns: cols, alignment: .leading, spacing: 10) {
                            ForEach(d.kpis) { k in
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(k.label.uppercased()).font(Brand.text(10, weight: .medium, relativeTo: .caption2)).tracking(0.4).foregroundStyle(Brand.fgSubtle)
                                    Text(k.text).font(Brand.mono(16, bold: true)).foregroundStyle(k.key == "net" ? Brand.pnl(k.value) : Brand.fg)
                                    Text("was \(k.previousText)").font(Brand.mono(10)).foregroundStyle(Brand.fgSubtle)
                                }
                            }
                        }
                    }
                    if !d.keep.isEmpty {
                        Card(keyline: Brand.pos) {
                            CardTitle("Keep")
                            ForEach(d.keep, id: \.self) { Text($0).font(Brand.text(14, relativeTo: .subheadline)) }
                        }
                    }
                    if !d.fix.isEmpty {
                        Card(keyline: Brand.warn) {
                            CardTitle("Fix")
                            ForEach(d.fix, id: \.self) { Text($0).font(Brand.text(14, relativeTo: .subheadline)) }
                        }
                    }
                    if d.bestDecision != nil || d.worstDecision != nil {
                        Card {
                            CardTitle("Best and worst decision")
                            if let b = d.bestDecision { decision(b, label: "best") }
                            if let w = d.worstDecision { decision(w, label: "worst") }
                        }
                    }
                    if !d.lessons.isEmpty {
                        Card(keyline: Brand.accent) {
                            CardTitle("Lessons", trailing: "\(d.lessons.reduce(0) { $0 + $1.open }) open")
                            ForEach(d.lessons) { l in
                                VStack(alignment: .leading, spacing: 2) {
                                    HStack(spacing: 6) {
                                        if let n = l.mistakeName, !n.isEmpty { Pill(n, .error) }
                                        Text("×\(l.count)").font(Brand.mono(11)).foregroundStyle(Brand.fgSubtle)
                                        Spacer()
                                        if let day = l.day, !day.isEmpty {
                                            NavigationLink(value: Route.day(day)) { Text(Formatting.dayLabel(day, tz: client.tz)).font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.accentText) }
                                        }
                                    }
                                    Text(l.lesson).font(Brand.text(14, relativeTo: .subheadline)).italic()
                                }
                            }
                        }
                    }
                    if !d.coach.isEmpty {
                        Card {
                            CardTitle("Coach")
                            ForEach(d.coach) { c in
                                HStack(alignment: .firstTextBaseline, spacing: 6) {
                                    Circle().fill(c.severity == "warn" || c.severity == "error" ? Brand.warn : Brand.info).frame(width: 6, height: 6)
                                    Text(c.title).font(Brand.text(14, relativeTo: .subheadline))
                                    Spacer()
                                    Text("\(c.count)× on \(c.days) day\(c.days == 1 ? "" : "s")").font(Brand.mono(11)).foregroundStyle(Brand.fgMuted)
                                }
                            }
                        }
                    }
                    Card {
                        CardTitle("Plans", trailing: "\(d.plan.planned) of \(d.plan.tradingDays) days\(d.plan.writtenLate > 0 ? " · \(d.plan.writtenLate) late" : "")")
                        if d.plan.rules.isEmpty {
                            Text("No plan rules were measured this week.").font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted)
                        }
                        ForEach(d.plan.rules) { r in
                            HStack {
                                Text(r.name).font(Brand.text(14, relativeTo: .subheadline))
                                Spacer()
                                Text("held \(r.held) of \(r.total)").font(Brand.mono(12)).foregroundStyle(r.held < r.total ? Brand.negText : Brand.posText)
                            }
                        }
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
        .navigationTitle("Weekly digest")
        .navigationBarTitleDisplayMode(.inline)
        .task(id: current ?? week ?? "") { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
    }

    private func decision(_ d: DigestDecision, label: String) -> some View {
        NavigationLink(value: Route.trade(d.tradeId)) {
            HStack(spacing: 6) {
                Text(label).font(Brand.text(11, weight: .medium, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle).frame(width: 40, alignment: .leading)
                Text(d.symbolRoot).font(Brand.text(14, weight: .semibold, relativeTo: .subheadline))
                Pill.direction(d.direction)
                if d.accounts > 1 { Pill("×\(d.accounts)", .group) }
                Text(Formatting.dayLabel(d.day, tz: client.tz)).font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgMuted)
                Spacer()
                PnlChip(d.net, size: .md)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    /// The ISO week before or after: "2026-W41" ± 1 through the calendar.
    private func step(_ d: Digest, _ by: Int) {
        guard let from = Formatting.parseDay(d.from, tz: client.tz) else { return }
        var cal = Calendar(identifier: .iso8601)
        cal.timeZone = client.tz
        guard let next = cal.date(byAdding: .day, value: 7 * by, to: from) else { return }
        let c = cal.dateComponents([.yearForWeekOfYear, .weekOfYear], from: next)
        current = String(format: "%04d-W%02d", c.yearForWeekOfYear ?? 0, c.weekOfYear ?? 0)
    }

    private func load() async {
        do {
            digest = try await client.digest(week: current ?? week)
            error = nil
        } catch {
            if digest == nil { self.error = error.localizedDescription }
        }
    }
}
