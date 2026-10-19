// The Session activity's faces, in the brand's fixed dark dress: the Lock Screen card and the
// Dynamic Island's compact, minimal and expanded parts. The widget extension puts them in its
// ActivityConfiguration; they live here so the app's tests can render them. The clocks are
// SwiftUI timers: the server pushes state changes only.
import SwiftUI

enum SessionStyle {
    static func color(_ k: SessionActivityAttributes.ContentState.Kind) -> Color {
        switch k {
        case .quiet: Brand.dark.accent
        case .pos: Brand.dark.pos
        case .neg: Brand.dark.neg
        case .warn: Brand.dark.warn
        }
    }

    static func glyph(_ k: SessionActivityAttributes.ContentState.Kind) -> String {
        switch k {
        case .quiet: "●"
        case .pos: "▲"
        case .neg: "▼"
        case .warn: "⚠"
        }
    }

    static func pnl(_ v: Double) -> Color { v < 0 ? Brand.dark.neg : Brand.dark.pos }
}

/// The glyph that carries the state: colour and shape.
struct SessionGlyph: View {
    let state: SessionActivityAttributes.ContentState
    var size: CGFloat = 14
    var body: some View {
        Text(SessionStyle.glyph(state.kind)).font(Brand.mono(size, bold: true)).foregroundStyle(SessionStyle.color(state.kind))
    }
}

/// The total, signed, no decimals.
struct SessionTotal: View {
    let state: SessionActivityAttributes.ContentState
    var size: CGFloat = 13
    var body: some View {
        Text(Formatting.moneyShort(state.total)).font(Brand.mono(size, bold: true)).foregroundStyle(SessionStyle.pnl(state.total)).amount()
    }
}

/// One open position: contract, direction, size (with the legs it stands for when folded by
/// set), unrealised and the stop distance.
struct SessionPositionRow: View {
    let p: SessionPosition
    var compact = false

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Text(p.s).font(Brand.mono(compact ? 12 : 13, bold: true)).foregroundStyle(Brand.dark.fg)
            Text("\(p.d) \(Formatting.qty(p.q))").font(Brand.text(compact ? 10 : 11)).foregroundStyle(Brand.dark.fgMuted)
            if p.legs > 1 { Text("×\(p.legs)").font(Brand.mono(compact ? 10 : 11, bold: true)).foregroundStyle(Brand.dark.group) }
            Spacer(minLength: 4)
            if p.beyond {
                Text("through stop").font(Brand.text(compact ? 10 : 11, weight: .medium)).foregroundStyle(Brand.dark.neg)
            } else if p.hasStop {
                Text("\(Int(p.tts.rounded())) to stop").font(Brand.text(compact ? 10 : 11)).foregroundStyle(Brand.dark.fgMuted)
            } else {
                Text("no stop").font(Brand.text(compact ? 10 : 11, weight: .medium)).foregroundStyle(Brand.dark.warn)
            }
            Text(Formatting.money(p.u)).font(Brand.mono(compact ? 12 : 13)).foregroundStyle(SessionStyle.pnl(p.u)).frame(minWidth: 64, alignment: .trailing).amount()
        }
        .lineLimit(1)
    }
}

/// The budget bar: max loss used, else trades used, else the session clock.
struct SessionProgress: View {
    let attributes: SessionActivityAttributes
    let state: SessionActivityAttributes.ContentState
    var compact = false

    var body: some View {
        if let f = state.lossFraction {
            VStack(alignment: .leading, spacing: 2) {
                BudgetBar(fraction: f, tint: f >= 1 ? Brand.dark.neg : f >= 0.8 ? Brand.dark.warn : Brand.dark.accent)
                if !compact {
                    Text("\(Formatting.moneyShort(-state.lossUsed)) of \(Formatting.moneyShort(-state.maxLoss)) max loss\(state.maxTrades > 0 ? " · \(state.trades) of \(state.maxTrades) trades" : "")")
                        .font(Brand.text(10)).foregroundStyle(Brand.dark.fgMuted).amount()
                }
            }
        } else if state.maxTrades > 0 {
            VStack(alignment: .leading, spacing: 2) {
                BudgetBar(fraction: min(1, Double(state.trades) / Double(state.maxTrades)), tint: state.trades >= state.maxTrades ? Brand.dark.neg : Brand.dark.accent)
                if !compact { Text("\(state.trades) of \(state.maxTrades) trades").font(Brand.text(10)).foregroundStyle(Brand.dark.fgMuted) }
            }
        } else {
            ProgressView(timerInterval: attributes.rthRange, countsDown: false, label: { EmptyView() }, currentValueLabel: { EmptyView() }).tint(Brand.dark.accent)
        }
    }
}

/// A determinate bar drawn by hand: the same in the widget, on the Lock Screen and in a test's render.
struct BudgetBar: View {
    let fraction: Double
    let tint: Color
    var height: CGFloat = 4

    var body: some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(Brand.dark.border)
                Capsule().fill(tint).frame(width: max(height, geo.size.width * CGFloat(min(1, max(0, fraction)))))
            }
        }
        .frame(height: height)
        .accessibilityLabel("\(Int(fraction * 100)) percent used")
    }
}

/// The Lock Screen card (and the banner on a phone without the island), at most 160 pt.
struct SessionLockScreen: View {
    let attributes: SessionActivityAttributes
    let state: SessionActivityAttributes.ContentState
    var isStale = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                SessionGlyph(state: state)
                Text("Session").font(Brand.text(12, weight: .semibold)).foregroundStyle(Brand.dark.fg)
                Text(Formatting.dayLabel(attributes.day, tz: attributes.timeZone)).font(Brand.text(12)).foregroundStyle(Brand.dark.fgMuted)
                Spacer()
                if isStale {
                    Text("no news since \(Formatting.clock(state.updatedDate, tz: attributes.timeZone))").font(Brand.text(11)).foregroundStyle(Brand.dark.fgMuted)
                } else {
                    SessionClock(attributes: attributes)
                }
            }
            HStack(alignment: .firstTextBaseline, spacing: 10) {
                Text(Formatting.money(state.total)).font(Brand.mono(28, bold: true)).foregroundStyle(isStale ? Brand.dark.fgMuted : SessionStyle.pnl(state.total)).amount()
                VStack(alignment: .leading, spacing: 0) {
                    Text("realised \(Formatting.money(state.realized))").font(Brand.mono(11)).foregroundStyle(Brand.dark.fgMuted).amount()
                    Text(state.openCount > 0 ? "open \(Formatting.money(state.unrealized))\(state.stale ? " ·" : "")" : "\(state.trades) trades · \(state.wins)W \(state.losses)L").amount()
                        .font(Brand.mono(11)).foregroundStyle(Brand.dark.fgMuted)
                }
                Spacer()
            }
            SessionProgress(attributes: attributes, state: state, compact: true)
            ForEach(state.positions.prefix(2)) { p in
                Link(destination: p.link) { SessionPositionRow(p: p) }
            }
            if state.nudges > 0 {
                Link(destination: Route.nudges.url) {
                    HStack(spacing: 6) {
                        Text("⚠").font(Brand.mono(11, bold: true)).foregroundStyle(Brand.dark.warn)
                        Text(state.nudges == 1 ? "1 nudge waits" : "\(state.nudges) nudges wait").font(Brand.text(11, weight: .medium)).foregroundStyle(Brand.dark.warn)
                    }
                }
            }
        }
        .padding(14)
        .environment(\.colorScheme, .dark)
    }
}

/// The session's hours in the exchange's time and the phone's, and while the session runs the
/// countdown to its close. Before the open nothing counts down: a frozen countdown showing the
/// session's length read like a clock in a zone the trader does not live in.
struct SessionClock: View {
    let attributes: SessionActivityAttributes
    var now: Date = .now
    var local: TimeZone = .current

    var body: some View {
        VStack(alignment: .trailing, spacing: 1) {
            if attributes.rthRange.contains(now) {
                HStack(spacing: 3) {
                    Text("closes in").font(Brand.text(10)).foregroundStyle(Brand.dark.fgMuted)
                    Text(timerInterval: attributes.rthRange, countsDown: true, showsHours: true).font(Brand.mono(11)).monospacedDigit().foregroundStyle(Brand.dark.fg)
                        .multilineTextAlignment(.trailing).frame(maxWidth: 64)
                }
            } else if now > attributes.rthRange.upperBound {
                Text("closed").font(Brand.text(10)).foregroundStyle(Brand.dark.fgMuted)
            }
            ForEach(Self.hours(attributes, local: local), id: \.self) { Text($0).font(Brand.mono(10)).foregroundStyle(Brand.dark.fgMuted) }
        }
        .lineLimit(1)
    }

    /// "08:30–15:00 Chicago", then "15:30–22:00 Amsterdam" when the phone keeps another time.
    nonisolated static func hours(_ a: SessionActivityAttributes, local: TimeZone) -> [String] {
        let start = a.rthRange.lowerBound, end = a.rthRange.upperBound
        func line(_ tz: TimeZone) -> String {
            "\(Formatting.clock(start, tz: tz))–\(Formatting.clock(end, tz: tz)) \(city(tz))"
        }
        let exchange = a.exchangeZone
        var out = [line(exchange)]
        if local.secondsFromGMT(for: start) != exchange.secondsFromGMT(for: start) { out.append(line(local)) }
        return out
    }

    nonisolated static func city(_ tz: TimeZone) -> String {
        tz.identifier.split(separator: "/").last.map { $0.replacingOccurrences(of: "_", with: " ") } ?? tz.identifier
    }
}

// MARK: the Dynamic Island

struct SessionCompactLeading: View {
    let state: SessionActivityAttributes.ContentState
    var body: some View { SessionGlyph(state: state) }
}

struct SessionCompactTrailing: View {
    let state: SessionActivityAttributes.ContentState
    var body: some View { SessionTotal(state: state) }
}

struct SessionMinimal: View {
    let state: SessionActivityAttributes.ContentState
    var body: some View { SessionGlyph(state: state, size: 13) }
}

/// The expanded island: the total with the realised and open parts, the clock to the close,
/// the budget bar, and the positions or the nudge line. At most three rows, never a scroll.
struct SessionExpandedBottom: View {
    let attributes: SessionActivityAttributes
    let state: SessionActivityAttributes.ContentState

    var body: some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Text("realised \(Formatting.money(state.realized))").font(Brand.mono(11)).foregroundStyle(Brand.dark.fgMuted).amount()
                if state.openCount > 0 { Text("open \(Formatting.money(state.unrealized))").font(Brand.mono(11)).foregroundStyle(SessionStyle.pnl(state.unrealized)).amount() }
                Spacer()
                SessionClock(attributes: attributes)
            }
            SessionProgress(attributes: attributes, state: state, compact: true)
            if state.nudges > 0 {
                Link(destination: Route.nudges.url) {
                    Text("⚠ \(state.nudges == 1 ? "1 nudge waits" : "\(state.nudges) nudges wait")").font(Brand.text(11, weight: .medium)).foregroundStyle(Brand.dark.warn)
                }
            }
            ForEach(state.positions.prefix(state.nudges > 0 ? 1 : 2)) { p in
                Link(destination: p.link) { SessionPositionRow(p: p, compact: true) }
            }
        }
        .environment(\.colorScheme, .dark)
    }
}
