// The Library: the trader's strategies, each with its playbook, its checklist rules and what the
// last 90 days say about it. Reading only on the phone; the web and the MCP tools write.
import SwiftUI

struct LibraryView: View {
    @Environment(Client.self) private var client
    @State private var strategies: [Strategy] = []
    @State private var analytics: AnalyticsSummary?
    @State private var loading = false
    @State private var error: String?
    @State private var asOf: Date?

    var body: some View {
        List {
            if let asOf { StaleBanner(asOf).listRowBackground(Color.clear).listRowSeparator(.hidden) }
            if let error, strategies.isEmpty {
                Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText).listRowBackground(Color.clear)
            }
            if strategies.isEmpty, !loading, error == nil {
                Text("No strategies yet. Write one on the web, or ask the assistant to save a setup you worked out together.")
                    .font(Brand.text(14, relativeTo: .subheadline)).foregroundStyle(Brand.fgMuted).listRowBackground(Color.clear)
            }
            ForEach(strategies) { s in
                NavigationLink(value: Route.strategy(s.id)) { row(s) }
                    .listRowBackground(Brand.bgElev)
            }
        }
        .listStyle(.plain)
        .background(Brand.bg)
        .scrollContentBackground(.hidden)
        .navigationTitle("Library")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load(fresh: true) }
        .task { if strategies.isEmpty { await load() } }
        .onChange(of: client.version) { _, _ in Task { await load() } }
        .reloadsWhenLive(asOf != nil) { await load() }
    }

    @ViewBuilder
    private func row(_ s: Strategy) -> some View {
        let bucket = analytics?.bucket("byStrategy", s.name)
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text(s.name).font(Brand.text(15, weight: .semibold, relativeTo: .body))
                Spacer()
                if let bucket, bucket.trades > 0 { PnlChip(bucket.net, size: .sm) }
            }
            if !s.snippet.isEmpty {
                Text(s.snippet).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted).lineLimit(2)
            }
            HStack(spacing: 6) {
                if !s.checklist.isEmpty { Pill(s.checklist.count == 1 ? "1 rule" : "\(s.checklist.count) rules", .sunken) }
                if !s.attachments.isEmpty { Pill(s.attachments.count == 1 ? "1 image" : "\(s.attachments.count) images", .sunken) }
                if let bucket, bucket.trades > 0 {
                    Text("\(bucket.trades) trades").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
                    if bucket.winRate != nil { Text(Formatting.percent(bucket.winRate) + " hit").font(Brand.text(11, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle) }
                }
            }
        }
        .padding(.vertical, 4)
    }

    private func load(fresh: Bool = false) async {
        loading = true
        defer { loading = false }
        let range = client.libraryRange
        async let s = client.strategies(fresh: fresh)
        async let a = client.analytics(from: range.from, to: range.to, fresh: fresh)
        do {
            let loaded = try await s
            strategies = loaded.value
            asOf = loaded.asOf
            error = nil
        } catch {
            self.error = error.localizedDescription
        }
        analytics = (try? await a)?.value // the numbers are a garnish: the list stands without them
    }
}

/// One strategy: the playbook, the rules, the last 90 days' numbers and the notes filed under it.
struct StrategyView: View {
    let id: Int
    @Environment(Client.self) private var client
    @State private var strategy: Strategy?
    @State private var analytics: AnalyticsSummary?
    @State private var notes: [JournalRow] = []
    @State private var error: String?
    @State private var asOf: Date?

    var body: some View {
        ScrollView {
            if let s = strategy {
                VStack(alignment: .leading, spacing: 12) {
                    StaleBanner(asOf)
                    header(s)
                    Card {
                        CardTitle("Playbook")
                        if s.body.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty {
                            Text("No playbook written yet.").font(Brand.text(14, relativeTo: .subheadline)).foregroundStyle(Brand.fgMuted)
                        } else {
                            MarkdownView(source: s.body)
                        }
                    }
                    if !s.checklist.isEmpty {
                        Card {
                            CardTitle("Checklist", trailing: "\(s.checklist.count) rules")
                            ForEach(Array(s.checklist.enumerated()), id: \.offset) { i, rule in
                                HStack(alignment: .firstTextBaseline, spacing: 8) {
                                    Text("\(i + 1).").font(Brand.mono(12)).foregroundStyle(Brand.fgSubtle).frame(minWidth: 20, alignment: .trailing)
                                    Text(rule).font(Brand.text(14, relativeTo: .subheadline))
                                }
                            }
                        }
                    }
                    if let c = analytics?.compliance(for: s.name), c.trades > 0 {
                        Card {
                            CardTitle("Rules followed", trailing: Formatting.percent(c.avgPct))
                            Text("\(c.complete) of \(c.trades) trades ticked the whole checklist.")
                                .font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.fgMuted)
                            StatStrip(stats: [
                                .init(label: "complete", value: Formatting.moneyShort(c.completeNet), color: Brand.pnl(c.completeNet)),
                                .init(label: "incomplete", value: Formatting.moneyShort(c.incompleteNet), color: Brand.pnl(c.incompleteNet)),
                            ])
                        }
                    }
                    if !notes.isEmpty {
                        Card {
                            CardTitle("Notes", trailing: "\(notes.count)")
                            ForEach(notes) { r in
                                NavigationLink(value: Route.journal(r.id)) {
                                    HStack(alignment: .firstTextBaseline, spacing: 8) {
                                        Text(Formatting.dayLabel(r.date, tz: client.tz)).font(Brand.mono(12)).foregroundStyle(Brand.fgMuted)
                                        Pill(r.scope == .group ? "set" : r.scope.rawValue, r.scope == .group ? .group : .sunken)
                                        Text(r.plainSnippet).font(Brand.text(13, relativeTo: .footnote)).lineLimit(1)
                                        Spacer()
                                        if let sum = r.summary, sum.trades > 0 { PnlChip(sum.net, size: .sm) }
                                    }
                                    .contentShape(Rectangle())
                                }
                                .buttonStyle(.plain)
                            }
                        }
                    }
                }
                .padding(16)
            } else if let error {
                Text(error).font(Brand.text(13, relativeTo: .footnote)).foregroundStyle(Brand.negText).padding(16)
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 120)
            }
        }
        .background(Brand.bg)
        .navigationTitle(strategy?.name ?? "Strategy")
        .navigationBarTitleDisplayMode(.inline)
        .refreshable { await load(fresh: true) }
        .task(id: id) { await load() }
        .onChange(of: client.version) { _, _ in Task { await load() } }
        .reloadsWhenLive(asOf != nil) { await load() }
    }

    @ViewBuilder
    private func header(_ s: Strategy) -> some View {
        let bucket = analytics?.bucket("byStrategy", s.name)
        Card {
            HStack(spacing: 6) {
                Text(s.name).font(Brand.text(17, weight: .semibold, relativeTo: .title3))
                if s.archivedAt != nil { Pill("archived", .retired) }
                Spacer()
                if let bucket, bucket.trades > 0 { PnlChip(bucket.net, size: .md) }
            }
            Text("Updated \(Formatting.dayLabel(Formatting.day(s.updatedAt, tz: client.tz), tz: client.tz, style: .long))")
                .font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
            if let bucket, bucket.trades > 0 {
                StatStrip(stats: [
                    .init(label: "trades, 90d", value: "\(bucket.trades)"),
                    .init(label: "wins", value: "\(bucket.wins)", color: Brand.posText),
                    .init(label: "losses", value: "\(bucket.losses)", color: Brand.negText),
                    .init(label: "hit rate", value: Formatting.percent(bucket.winRate)),
                ])
            } else if analytics != nil {
                Text("No closed trades under this strategy in the last 90 days.").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
            }
        }
    }

    private func load(fresh: Bool = false) async {
        let range = client.libraryRange
        async let s = client.strategy(id, fresh: fresh)
        async let a = client.analytics(from: range.from, to: range.to, fresh: fresh)
        async let j = client.journal(.init(from: range.from, to: range.to, limit: 200), fresh: fresh)
        do {
            let loaded = try await s
            strategy = loaded.value
            asOf = loaded.asOf
            error = nil
        } catch {
            // The list carries every active strategy whole; it stands in when the detail is out of reach.
            if let kept = (try? await client.strategies())?.value.first(where: { $0.id == id }) {
                strategy = kept
            } else {
                self.error = error.localizedDescription
            }
        }
        analytics = (try? await a)?.value
        if let name = strategy?.name, let rows = (try? await j)?.value {
            notes = rows.filter { $0.strategy == name }
        }
    }
}
