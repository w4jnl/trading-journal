// The expanded notification (long press), in the brand's dark dress: what the push said, the
// lesson, and the live numbers behind it fetched from the journal when the card opens (the day's
// P&L for a nudge, the position for a position nudge, the trade for a closed trade), with Seen
// and Open in place. The content extension hosts it; it lives here so the app's tests render it.
import SwiftUI

enum CardData: Equatable, Sendable {
    case loading
    case today(net: Double, open: Double?, stale: Bool)
    case position(Position)
    case trade(TradeDetail)
    case unavailable(String)
}

struct NotificationCard: View {
    let push: PushPayload
    var title: String
    var subtitle: String
    var body_: String
    var data: CardData = .loading
    var status: String?
    var tz: TimeZone = .current
    var onSeen: () -> Void = {}
    var onOpen: () -> Void = {}

    private var accent: Color {
        switch push.kind {
        case .nudge: Brand.dark.warn
        case .trade: Brand.pnl(push.netPnl)
        default: Brand.dark.accent
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                Circle().fill(accent).frame(width: 8, height: 8).offset(y: -2)
                VStack(alignment: .leading, spacing: 2) {
                    Text(title).font(Brand.text(15, weight: .semibold)).foregroundStyle(Brand.dark.fg)
                    if !subtitle.isEmpty { Text(subtitle).font(Brand.text(12)).foregroundStyle(Brand.dark.fgMuted) }
                }
                .lineLimit(2)
                Spacer(minLength: 8)
                Text("JOURNAL").font(Brand.text(10, weight: .semibold)).tracking(1).foregroundStyle(Brand.dark.accent)
            }
            if !body_.isEmpty {
                Text(bodyWithoutLesson).font(Brand.text(14)).foregroundStyle(Brand.dark.fg.opacity(0.92)).lineLimit(6).fixedSize(horizontal: false, vertical: true)
            }
            if !push.lesson.isEmpty {
                HStack(alignment: .top, spacing: 8) {
                    Rectangle().fill(Brand.dark.accent).frame(width: 2)
                    Text("Your lesson: \(push.lesson)").font(Brand.text(13)).italic().foregroundStyle(Brand.dark.fgMuted).lineLimit(4).fixedSize(horizontal: false, vertical: true)
                }
            }
            panel
            if push.kind == .nudge || push.kind == .trade {
                HStack(spacing: 8) {
                    if push.kind == .nudge, push.id > 0 {
                        cardButton("Seen", symbol: "eye", action: onSeen)
                    }
                    cardButton("Open", symbol: "arrow.up.right.square", action: onOpen)
                    Spacer()
                    if let status { Text(status).font(Brand.text(11)).foregroundStyle(Brand.dark.fgMuted) }
                }
            }
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Brand.dark.bg)
        .environment(\.colorScheme, .dark)
    }

    /// The server appends " — Your lesson: …" to the body; the card shows the lesson on its own.
    private var bodyWithoutLesson: String {
        if let r = body_.range(of: " — Your lesson: ") { return String(body_[..<r.lowerBound]) }
        return body_
    }

    @ViewBuilder private var panel: some View {
        Group {
            switch data {
            case .loading:
                Text("looking at the journal…").font(Brand.text(12)).foregroundStyle(Brand.dark.fgMuted)
            case .unavailable(let why):
                Text(why).font(Brand.text(12)).foregroundStyle(Brand.dark.fgMuted)
            case .today(let net, let open, let stale):
                HStack(alignment: .firstTextBaseline, spacing: 12) {
                    stat("today", Formatting.money(net), color: Brand.pnl(net))
                    if let open { stat("open", Formatting.money(open) + (stale ? " ·" : ""), color: Brand.pnl(open)) }
                    Spacer()
                }
            case .position(let p):
                VStack(alignment: .leading, spacing: 6) {
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        stat(p.symbol, "\(p.direction.label) \(Formatting.qty(p.qty)) @ \(Formatting.price(p.avgEntry, tickSize: p.tickSize))")
                        stat("last", Formatting.price(p.lastPrice, tickSize: p.tickSize))
                        stat("open", Formatting.money(p.unrealized), color: Brand.pnl(p.unrealized))
                        Spacer()
                    }
                    HStack(alignment: .firstTextBaseline, spacing: 12) {
                        stat("stop", Formatting.price(p.stop, tickSize: p.tickSize), color: p.beyondStop ? Brand.dark.neg : nil)
                        stat("target", Formatting.price(p.target, tickSize: p.tickSize))
                        stat("to stop", p.beyondStop ? "through" : Formatting.ticks(p.ticksToStop), color: p.beyondStop ? Brand.dark.neg : nil)
                        Spacer()
                    }
                }
            case .trade(let t):
                HStack(alignment: .firstTextBaseline, spacing: 12) {
                    stat("net", Formatting.money(t.netPnl), color: Brand.pnl(t.netPnl))
                    stat("R", Formatting.r(t.r))
                    stat("in", Formatting.clock(t.entryTime, tz: tz))
                    stat("out", Formatting.clock(t.exitTime, tz: tz))
                    Spacer()
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10)
        .background(Brand.dark.bgElev, in: RoundedRectangle(cornerRadius: 8))
    }

    private func stat(_ label: String, _ value: String, color: Color? = nil) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(label.uppercased()).font(Brand.text(9, weight: .medium)).tracking(0.5).foregroundStyle(Brand.dark.fgMuted)
            Text(value).font(Brand.mono(13)).foregroundStyle(color ?? Brand.dark.fg)
        }
    }

    private func cardButton(_ title: String, symbol: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Label(title, systemImage: symbol).font(Brand.text(13, weight: .medium)).foregroundStyle(Brand.dark.fg)
                .padding(.horizontal, 12).frame(minHeight: 32)
                .background(Brand.dark.bgElev, in: RoundedRectangle(cornerRadius: 7))
                .overlay(RoundedRectangle(cornerRadius: 7).strokeBorder(Brand.dark.accent.opacity(0.45), lineWidth: 1))
        }
        .buttonStyle(.plain)
    }
}
