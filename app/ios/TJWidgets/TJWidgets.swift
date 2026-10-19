// The widget extension: the Session Live Activity in the Dynamic Island and on the Lock Screen
// (Shared/SessionActivity.swift for the data, Shared/LiveViews.swift for the faces), and the
// Today widgets on the Lock Screen and the Home Screen, which read what the app last cached in
// the App Group (Shared/TodayCache.swift). A tap opens the app on Today; a position row opens
// that position.
import ActivityKit
import AppIntents
import SwiftUI
import WidgetKit

@main
struct TJWidgets: WidgetBundle {
    var body: some Widget {
        SessionLiveActivity()
        TodayPnlWidget()
        GuardrailControl()
    }
}

struct SessionLiveActivity: Widget {
    var body: some WidgetConfiguration {
        // The faces follow the trader's "Trades grouped by set": one row per decision when on.
        ActivityConfiguration(for: SessionActivityAttributes.self) { context in
            SessionLockScreen(attributes: context.attributes, state: context.state.display(bySet: Grouping.bySet()), isStale: context.isStale)
                .activityBackgroundTint(Brand.dark.bg)
                .activitySystemActionForegroundColor(Brand.dark.fg)
                .widgetURL(Route.today.url)
        } dynamicIsland: { context in
            let state = context.state.display(bySet: Grouping.bySet())
            return DynamicIsland {
                DynamicIslandExpandedRegion(.leading) {
                    HStack(spacing: 6) {
                        SessionGlyph(state: state)
                        Text("Session").font(Brand.text(12, weight: .semibold)).foregroundStyle(Brand.dark.fg)
                    }
                }
                DynamicIslandExpandedRegion(.trailing) {
                    SessionTotal(state: state, size: 15)
                }
                DynamicIslandExpandedRegion(.bottom) {
                    SessionExpandedBottom(attributes: context.attributes, state: state)
                }
            } compactLeading: {
                SessionCompactLeading(state: state)
            } compactTrailing: {
                SessionCompactTrailing(state: state)
            } minimal: {
                SessionMinimal(state: state)
            }
            .widgetURL(Route.today.url)
            .keylineTint(SessionStyle.color(state.kind))
        }
    }
}

// MARK: the Today widgets

struct TodayEntry: TimelineEntry {
    let date: Date
    let today: TodayCache?
}

struct TodayProvider: TimelineProvider {
    func placeholder(in context: Context) -> TodayEntry { TodayEntry(date: .now, today: .preview) }

    func getSnapshot(in context: Context, completion: @escaping (TodayEntry) -> Void) {
        completion(TodayEntry(date: .now, today: context.isPreview ? .preview : TodayCache.read()?.display(bySet: Grouping.bySet())))
    }

    func getTimeline(in context: Context, completion: @escaping (Timeline<TodayEntry>) -> Void) {
        let t = TodayCache.read()?.display(bySet: Grouping.bySet()) // counts per decision when the trader groups by set
        // The app and the silent sync reload the timeline on every change; this is the fallback
        // cadence: a quarter hour in the session, an hour outside it.
        let next = Date.now.addingTimeInterval(t?.inGlobex == true ? 15 * 60 : 60 * 60)
        completion(Timeline(entries: [TodayEntry(date: .now, today: t)], policy: .after(next)))
    }
}

struct TodayPnlWidget: Widget {
    var body: some WidgetConfiguration {
        StaticConfiguration(kind: "nl.w4j.tradingjournal.today", provider: TodayProvider()) { entry in
            TodayWidgetView(today: entry.today)
                .containerBackground(for: .widget) { Brand.dark.bg }
                .widgetURL(Route.today.url)
        }
        .configurationDisplayName("Today")
        .description("Today's P&L, the open positions and the plan's budget.")
        .supportedFamilies([.accessoryCircular, .accessoryRectangular, .accessoryInline, .systemSmall, .systemMedium])
    }
}

struct TodayWidgetView: View {
    let today: TodayCache?
    @Environment(\.widgetFamily) private var family

    var body: some View {
        Group {
            if let t = today {
                switch family {
                case .accessoryCircular: circular(t)
                case .accessoryRectangular: rectangular(t)
                case .accessoryInline: Text("Journal \(Formatting.moneyShort(t.total))")
                case .systemMedium: medium(t)
                default: small(t)
                }
            } else {
                switch family {
                case .accessoryInline: Text("Journal: open the app")
                case .accessoryCircular: Image(systemName: "chart.line.uptrend.xyaxis")
                default: Text("Open the app once to fill this.").font(Brand.text(12)).foregroundStyle(Brand.dark.fgMuted)
                }
            }
        }
        .environment(\.colorScheme, .dark)
    }

    private func circular(_ t: TodayCache) -> some View {
        Gauge(value: t.maxLoss > 0 ? min(1, t.lossUsed / t.maxLoss) : (t.maxTrades > 0 ? min(1, Double(t.tradesUsed) / Double(t.maxTrades)) : 0)) {
            Text("J")
        } currentValueLabel: {
            Text(Formatting.moneyShort(t.total)).font(.system(size: 11, weight: .semibold, design: .monospaced)).minimumScaleFactor(0.6)
        }
        .gaugeStyle(.accessoryCircularCapacity)
    }

    private func rectangular(_ t: TodayCache) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            HStack(spacing: 4) {
                Text("Trade Log").font(.system(size: 12, weight: .semibold))
                Text(Formatting.moneyShort(t.total)).font(.system(size: 14, weight: .bold, design: .monospaced))
            }
            Text("\(t.trades) trades · \(t.openCount) open\(t.unseen > 0 ? " · \(t.unseen) nudge\(t.unseen == 1 ? "" : "s")" : "")").font(.system(size: 11))
            Text(t.hasPlan ? "plan \(t.tradesUsed)\(t.maxTrades > 0 ? "/\(t.maxTrades)" : "")\(t.maxLoss > 0 ? " · \(Int(t.lossUsed))/\(Int(t.maxLoss))" : "")" : "no plan").font(.system(size: 11)).opacity(0.8)
        }
    }

    private func small(_ t: TodayCache) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text("TODAY").font(Brand.text(10, weight: .medium)).tracking(0.6).foregroundStyle(Brand.dark.fgMuted)
                Spacer()
                if t.unseen > 0 { Text("⚠ \(t.unseen)").font(Brand.mono(10, bold: true)).foregroundStyle(Brand.dark.warn) }
            }
            Text(Formatting.money(t.net)).font(Brand.mono(26, bold: true)).foregroundStyle(SessionStyle.pnl(t.net)).minimumScaleFactor(0.6).lineLimit(1)
            if t.openCount > 0 {
                Text("open \(Formatting.money(t.unrealized))\(t.stale ? " ·" : "")").font(Brand.mono(11)).foregroundStyle(SessionStyle.pnl(t.unrealized))
            }
            Spacer(minLength: 0)
            HStack(spacing: 8) {
                stat("trades", "\(t.trades)")
                stat("W", "\(t.wins)")
                stat("L", "\(t.losses)")
                Spacer()
            }
            Text("as of \(Formatting.clock(t.asOf, tz: t.timeZone))").font(Brand.text(9)).foregroundStyle(Brand.dark.fgMuted)
        }
    }

    private func medium(_ t: TodayCache) -> some View {
        HStack(alignment: .top, spacing: 14) {
            VStack(alignment: .leading, spacing: 4) {
                Text("TODAY").font(Brand.text(10, weight: .medium)).tracking(0.6).foregroundStyle(Brand.dark.fgMuted)
                Text(Formatting.money(t.net)).font(Brand.mono(24, bold: true)).foregroundStyle(SessionStyle.pnl(t.net)).minimumScaleFactor(0.6).lineLimit(1)
                Text("\(t.trades) trades · \(t.openCount) open").font(Brand.text(11)).foregroundStyle(Brand.dark.fgMuted)
                Spacer(minLength: 0)
                Text("WEEK").font(Brand.text(10, weight: .medium)).tracking(0.6).foregroundStyle(Brand.dark.fgMuted)
                Text(Formatting.money(t.weekNet + t.net)).font(Brand.mono(16, bold: true)).foregroundStyle(SessionStyle.pnl(t.weekNet + t.net))
            }
            VStack(alignment: .leading, spacing: 4) {
                Text("LAST DAYS").font(Brand.text(10, weight: .medium)).tracking(0.6).foregroundStyle(Brand.dark.fgMuted)
                let scale = t.days.map { abs($0.net) }.max() ?? 0
                HStack(spacing: 3) {
                    ForEach(t.days.suffix(10)) { d in
                        RoundedRectangle(cornerRadius: 2)
                            .fill(d.net == 0 || scale == 0 ? Brand.dark.border : (d.net > 0 ? Brand.dark.pos : Brand.dark.neg).opacity(0.25 + 0.75 * min(1, abs(d.net) / scale)))
                            .frame(maxWidth: .infinity, minHeight: 28)
                    }
                }
                if let best = t.days.max(by: { $0.net < $1.net }), let worst = t.days.min(by: { $0.net < $1.net }) {
                    Text("best \(Formatting.moneyShort(best.net)) · worst \(Formatting.moneyShort(worst.net))").font(Brand.mono(10)).foregroundStyle(Brand.dark.fgMuted)
                }
                Spacer(minLength: 0)
                if t.hasPlan {
                    Text("plan \(t.tradesUsed)\(t.maxTrades > 0 ? " of \(t.maxTrades)" : "") trades\(t.maxLoss > 0 ? " · \(Formatting.moneyShort(-t.lossUsed)) of \(Formatting.moneyShort(-t.maxLoss))" : "")").font(Brand.text(10)).foregroundStyle(Brand.dark.fgMuted)
                } else {
                    Text("no plan today").font(Brand.text(10, weight: .medium)).foregroundStyle(Brand.dark.warn)
                }
            }
        }
    }

    private func stat(_ label: String, _ value: String) -> some View {
        HStack(spacing: 3) {
            Text(value).font(Brand.mono(12, bold: true)).foregroundStyle(Brand.dark.fg)
            Text(label).font(Brand.text(10)).foregroundStyle(Brand.dark.fgMuted)
        }
    }
}

// MARK: the guardrail switch in Control Center

/// The guardrail on or off from Control Center or the Lock Screen: the one server-side switch
/// the trader flips most, through PUT /settings with the key from the shared keychain.
struct GuardrailControl: ControlWidget {
    var body: some ControlWidgetConfiguration {
        StaticControlConfiguration(kind: "nl.w4j.tradingjournal.guardrail", provider: GuardrailProvider()) { on in
            ControlWidgetToggle("Guardrail", isOn: on, action: ToggleGuardrailIntent()) { on in
                Label(on ? "On" : "Off", systemImage: on ? "shield.checkered" : "shield.slash")
            }
            .tint(Brand.dark.accent)
        }
        .displayName("Guardrail")
        .description("The journal's guardrail: nudges on the day's plan and your lessons.")
    }
}

struct GuardrailProvider: ControlValueProvider {
    var previewValue: Bool { true }

    func currentValue() async throws -> Bool {
        guard let api = TJAPI.fromKeychain() else { return false }
        return try await api.get(Me.self, "/me").settings.guardrail
    }
}

struct ToggleGuardrailIntent: SetValueIntent {
    static let title: LocalizedStringResource = "Guardrail"
    static let isDiscoverable = false

    @Parameter(title: "On") var value: Bool

    func perform() async throws -> some IntentResult {
        guard let api = TJAPI.fromKeychain() else { return .result() }
        _ = try await api.send("PUT", "/settings", json: SettingsUpdate(guardrail: value, autoPlan: nil), as: SettingsSaved.self)
        return .result()
    }
}

#Preview("lock", as: .content, using: SessionActivityAttributes.preview) {
    SessionLiveActivity()
} contentStates: {
    SessionActivityAttributes.ContentState.previewOpen
    SessionActivityAttributes.ContentState.previewWarn
}

#Preview("expanded", as: .dynamicIsland(.expanded), using: SessionActivityAttributes.preview) {
    SessionLiveActivity()
} contentStates: {
    SessionActivityAttributes.ContentState.previewOpen
}

#Preview("compact", as: .dynamicIsland(.compact), using: SessionActivityAttributes.preview) {
    SessionLiveActivity()
} contentStates: {
    SessionActivityAttributes.ContentState.previewOpen
    SessionActivityAttributes.ContentState.previewWarn
}

extension SessionActivityAttributes.ContentState {
    static let previewOpen = Self(realized: 120.5, unrealized: 13.75, trades: 4, wins: 3, losses: 1, maxTrades: 6, lossUsed: 84.2, maxLoss: 300,
                                  positions: [SessionPosition(a: 7, s: "MESZ6", d: "long", q: 3, avg: 7766.25, last: 7767.25, u: 13.75, tts: 49, beyond: false)],
                                  openCount: 1, nudges: 0, lastFill: 1_760_530_000, updated: 1_760_530_060, stale: false, alert: "")
    static let previewWarn = Self(realized: -210, unrealized: -48.5, trades: 5, wins: 2, losses: 3, maxTrades: 6, lossUsed: 258.5, maxLoss: 300,
                                  positions: [SessionPosition(a: 7, s: "MESZ6", d: "short", q: 2, avg: 7760, last: 7768.25, u: -48.5, tts: -3, beyond: true)],
                                  openCount: 1, nudges: 1, lastFill: 1_760_530_000, updated: 1_760_530_060, stale: false, alert: "")
}
