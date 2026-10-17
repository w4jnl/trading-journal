// The web app's components in SwiftUI: cards, pills, P&L chips, stat strips, the heat cell and
// an image that fetches with the key. Views compose these and never style a number themselves.
import SwiftUI

/// A bordered panel on the elevated background, 16 pt padding, an optional keyline on the left.
struct Card<Content: View>: View {
    var keyline: Color?
    @ViewBuilder var content: () -> Content

    init(keyline: Color? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.keyline = keyline
        self.content = content
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) { content() }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(Brand.cardPadding)
            .background(Brand.bgElev)
            .overlay(alignment: .leading) {
                if let keyline { Rectangle().fill(keyline).frame(width: 3) }
            }
            .clipShape(RoundedRectangle(cornerRadius: Brand.radius))
            .overlay(RoundedRectangle(cornerRadius: Brand.radius).strokeBorder(Brand.border, lineWidth: 1))
    }
}

/// The 11 pt uppercase tracked title a card starts with.
struct CardTitle: View {
    let text: String
    var trailing: String?

    init(_ text: String, trailing: String? = nil) {
        self.text = text
        self.trailing = trailing
    }

    var body: some View {
        HStack {
            Text(text.uppercased()).font(Brand.text(11, weight: .medium, relativeTo: .caption)).tracking(0.6).foregroundStyle(Brand.fgMuted)
            Spacer()
            if let trailing { Text(trailing).font(Brand.text(11, relativeTo: .caption)).foregroundStyle(Brand.fgSubtle) }
        }
    }
}

enum PillKind {
    case sunken, success, error, warn, info, accent, group, retired, suggest

    var fg: Color {
        switch self {
        case .sunken: Brand.fgMuted
        case .success: Brand.posText
        case .error: Brand.negText
        case .warn: Brand.warnText
        case .info: Brand.infoText
        case .accent, .suggest: Brand.accentText
        case .group: Brand.group
        case .retired: Brand.retired
        }
    }

    var bg: Color {
        switch self {
        case .sunken: Brand.bgSunken
        case .success: Brand.posSoft
        case .error: Brand.negSoft
        case .warn: Brand.warnSoft
        case .info: Brand.infoSoft
        case .accent, .suggest: Brand.accentSoft
        case .group: Brand.group.opacity(0.14)
        case .retired: Brand.retired.opacity(0.16)
        }
    }
}

/// A small rounded label: an account type, a direction, a tag.
struct Pill: View {
    let text: String
    var kind: PillKind = .sunken

    init(_ text: String, _ kind: PillKind = .sunken) {
        self.text = text
        self.kind = kind
    }

    var body: some View {
        Text(text)
            .font(Brand.text(11, weight: .medium, relativeTo: .caption2))
            .foregroundStyle(kind.fg)
            .padding(.horizontal, 6).padding(.vertical, 2)
            .background(kind.bg)
            .clipShape(RoundedRectangle(cornerRadius: 4))
            .overlay {
                if kind == .suggest {
                    RoundedRectangle(cornerRadius: 4).strokeBorder(Brand.accent, style: StrokeStyle(lineWidth: 1, dash: [3, 2]))
                }
            }
            .lineLimit(1)
    }
}

extension Pill {
    /// long is info, short is warn: the web app's reading.
    static func direction(_ d: Direction) -> Pill {
        Pill(d.label, d == .long ? .info : d == .short ? .warn : .sunken)
    }

    static func tag(_ t: JournalTag?) -> Pill? {
        guard let t, t != .unknown else { return nil }
        return Pill(t.rawValue, t == .good ? .success : t == .bad ? .error : .sunken)
    }

    static func accountType(_ a: Account) -> Pill {
        switch a.type {
        case .live: Pill(a.typeLabel, .success)
        case .propFunded: Pill(a.typeLabel, .info)
        case .propLive: Pill(a.typeLabel, .accent)
        case .propEval: Pill(a.typeLabel, .warn)
        case .paper: Pill(a.typeLabel, .sunken)
        default: Pill(a.typeLabel.isEmpty ? "?" : a.typeLabel, .sunken)
        }
    }
}

/// A P&L figure in the mono face, green for gains and zero, red for losses, muted when unknown.
struct PnlChip: View {
    enum Size { case hero, lg, md, sm }
    let value: Double?
    var size: Size = .md
    var solid = false
    var dashed = false

    init(_ value: Double?, size: Size = .md, solid: Bool = false, dashed: Bool = false) {
        self.value = value
        self.size = size
        self.solid = solid
        self.dashed = dashed
    }

    private var points: CGFloat {
        switch size {
        case .hero: 36
        case .lg: 24
        case .md: 16
        case .sm: 13
        }
    }

    var body: some View {
        let color = Brand.pnl(value)
        Text(Formatting.money(value))
            .font(Brand.mono(points, bold: size == .hero || size == .lg))
            .foregroundStyle(solid ? Color.white : color)
            .padding(.horizontal, solid ? 8 : 0).padding(.vertical, solid ? 2 : 0)
            .background(solid ? color : .clear)
            .clipShape(RoundedRectangle(cornerRadius: 4))
            .overlay {
                if dashed { RoundedRectangle(cornerRadius: 4).strokeBorder(Brand.fgSubtle, style: StrokeStyle(lineWidth: 1, dash: [3, 2])).padding(-3) }
            }
            .accessibilityLabel(value.map { $0 < 0 ? "loss \(Formatting.money($0))" : "gain \(Formatting.money($0))" } ?? "no figure")
    }
}

/// A row of small stats: label above, value below.
struct StatStrip: View {
    struct Stat: Identifiable {
        var label: String
        var value: String
        var color: Color?
        var id: String { label }
    }
    let stats: [Stat]

    var body: some View {
        HStack(alignment: .top, spacing: 0) {
            ForEach(stats) { s in
                VStack(alignment: .leading, spacing: 2) {
                    Text(s.label.uppercased()).font(Brand.text(10, weight: .medium, relativeTo: .caption2)).tracking(0.4).foregroundStyle(Brand.fgSubtle)
                    Text(s.value).font(Brand.mono(14)).foregroundStyle(s.color ?? Brand.fg)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }
}

/// A calendar day: the date and the net, tinted by the net against the month's largest.
struct HeatCell: View {
    let day: Int
    let net: Double?
    let trades: Int
    let scale: Double // the month's largest |net|
    var isToday = false

    private var tint: Color {
        guard let net, scale > 0, net != 0 else { return Brand.bgSunken }
        let step = min(3, Int((abs(net) / scale) * 4))
        return net > 0 ? Brand.heatPos[step] : Brand.heatNeg[step]
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("\(day)").font(Brand.text(11, weight: isToday ? .bold : .regular, relativeTo: .caption2)).foregroundStyle(isToday ? Brand.accentText : Brand.fgMuted)
            Spacer(minLength: 0)
            if let net {
                Text(Formatting.moneyShort(net)).font(Brand.mono(11)).foregroundStyle(Brand.pnl(net)).lineLimit(1).minimumScaleFactor(0.7)
                Text("\(trades)").font(Brand.text(9, relativeTo: .caption2)).foregroundStyle(Brand.fgSubtle)
            }
        }
        .padding(4)
        .frame(maxWidth: .infinity, minHeight: 48, alignment: .topLeading)
        .background(tint)
        .clipShape(RoundedRectangle(cornerRadius: 4))
        .overlay(RoundedRectangle(cornerRadius: 4).strokeBorder(isToday ? Brand.accent : Brand.border, lineWidth: 1))
    }
}

/// An image the API serves (a chart PNG, an attachment), fetched with the key and cached.
struct AuthImage: View {
    let href: String
    var maxHeight: CGFloat? = 260
    @Environment(Client.self) private var client
    @State private var image: UIImage?
    @State private var failed = false
    @State private var full = false

    var body: some View {
        Group {
            if let image {
                Image(uiImage: image)
                    .resizable()
                    .aspectRatio(contentMode: .fit)
                    .frame(maxHeight: maxHeight)
                    .clipShape(RoundedRectangle(cornerRadius: 4))
                    .onTapGesture { full = true }
                    .fullScreenCover(isPresented: $full) { FullImage(image: image) }
            } else if failed {
                Label("no image", systemImage: "photo").font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgSubtle)
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 60)
            }
        }
        .task(id: href) { await load() }
    }

    private func load() async {
        if let cached = ImageCache.shared.object(forKey: href as NSString) {
            image = cached
            return
        }
        do {
            let data = try await client.image(href)
            if let ui = UIImage(data: data) {
                ImageCache.shared.setObject(ui, forKey: href as NSString)
                image = ui
            } else {
                failed = true
            }
        } catch {
            failed = true
        }
    }
}

@MainActor
enum ImageCache {
    static let shared: NSCache<NSString, UIImage> = {
        let c = NSCache<NSString, UIImage>()
        c.countLimit = 60
        return c
    }()
}

struct FullImage: View {
    let image: UIImage
    @Environment(\.dismiss) private var dismiss
    @State private var scale: CGFloat = 1

    var body: some View {
        ZStack(alignment: .topTrailing) {
            Color.black.ignoresSafeArea()
            ScrollView([.horizontal, .vertical]) {
                Image(uiImage: image).resizable().aspectRatio(contentMode: .fit)
                    .frame(width: UIScreen.main.bounds.width * scale)
            }
            .gesture(MagnifyGesture().onChanged { v in scale = max(1, min(4, v.magnification)) })
            Button { dismiss() } label: { Image(systemName: "xmark.circle.fill").font(.title).foregroundStyle(.white.opacity(0.8)) }
                .padding()
                .accessibilityLabel("Close")
        }
    }
}

/// What the stream and the last fetch say, in words.
struct ConnectionLine: View {
    @Environment(Client.self) private var client

    var body: some View {
        HStack(spacing: 6) {
            Circle().fill(color).frame(width: 7, height: 7)
            Text(text).font(Brand.text(12, relativeTo: .caption)).foregroundStyle(Brand.fgMuted)
        }
        .accessibilityElement(children: .combine)
    }

    private var color: Color {
        switch client.connection {
        case .live: Brand.pos
        case .connecting: Brand.fgSubtle
        case .down, .unauthorized: Brand.neg
        case .off: Brand.fgSubtle
        }
    }

    private var text: String {
        switch client.connection {
        case .live: return "live"
        case .connecting: return "connecting"
        case .off: return "not paired"
        case .unauthorized: return "key revoked: pair again"
        case .down(let since):
            if let asOf = client.asOf { return "no news since \(Formatting.clock(asOf, tz: client.tz))" }
            return "offline since \(Formatting.clock(since, tz: client.tz))"
        }
    }
}

/// What a screen says over a stored answer: when the journal last gave it. Nothing when the
/// answer is live. A screen puts it first, over the content it qualifies.
struct StaleBanner: View {
    let asOf: Date?
    @Environment(Client.self) private var client

    init(_ asOf: Date?) { self.asOf = asOf }

    var body: some View {
        if let asOf {
            HStack(spacing: 6) {
                Image(systemName: "clock.arrow.circlepath")
                Text("Last known \(Self.when(asOf, tz: client.tz)): the journal is out of reach")
            }
            .font(Brand.text(12, relativeTo: .caption))
            .foregroundStyle(Brand.warnText)
            .frame(maxWidth: .infinity, alignment: .leading)
            .accessibilityIdentifier("stale")
        }
    }

    /// The clock alone on the same day, the day with it otherwise.
    nonisolated static func when(_ d: Date, tz: TimeZone, now: Date = .now) -> String {
        let clock = Formatting.clock(d, tz: tz)
        let day = Formatting.day(d, tz: tz)
        return day == Formatting.day(now, tz: tz) ? clock : "\(Formatting.dayLabel(day, tz: tz)) \(clock)"
    }
}

/// Loads again when the journal comes back within reach, so a screen on a stored answer gets
/// the live one (the version event does that only when something changed meanwhile).
struct ReloadWhenLive: ViewModifier {
    let stale: Bool
    let action: () async -> Void
    @Environment(Client.self) private var client

    func body(content: Content) -> some View {
        content.onChange(of: client.connection.isLive) { _, live in
            if live, stale { Task { await action() } }
        }
    }
}

extension View {
    func reloadsWhenLive(_ stale: Bool, _ action: @escaping () async -> Void) -> some View {
        modifier(ReloadWhenLive(stale: stale, action: action))
    }
}

/// A line of markdown blocks.
struct MarkdownView: View {
    let source: String
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(Markdown.blocks(source).enumerated()), id: \.offset) { _, block in
                blockView(block)
            }
        }
    }

    @ViewBuilder
    private func blockView(_ b: Markdown.Block) -> some View {
        switch b {
        case .heading(let level, let text):
            inline(text).font(Brand.text(level <= 2 ? 17 : 15, weight: .semibold, relativeTo: level <= 2 ? .title3 : .headline)).padding(.top, 4)
        case .paragraph(let text):
            inline(text).font(Brand.text(15, relativeTo: .body))
        case .listItem(let text, let ordered, let indent):
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(ordered ? "·" : "•").foregroundStyle(Brand.fgMuted)
                inline(text).font(Brand.text(15, relativeTo: .body))
            }
            .padding(.leading, CGFloat(indent) * 14)
        case .code(let code):
            Text(code).font(Brand.mono(12)).padding(8).frame(maxWidth: .infinity, alignment: .leading).background(Brand.bgSunken).clipShape(RoundedRectangle(cornerRadius: 4))
        case .rule:
            Rectangle().fill(Brand.border).frame(height: 1).padding(.vertical, 4)
        case .image(_, let href):
            AuthImage(href: href)
        case .noteMarker(let text):
            Text(text).font(Brand.text(12, relativeTo: .caption)).italic().foregroundStyle(Brand.fgMuted)
        case .quote(let text):
            HStack(spacing: 8) {
                Rectangle().fill(Brand.borderStrong).frame(width: 2)
                inline(text).font(Brand.text(15, relativeTo: .body)).foregroundStyle(Brand.fgMuted)
            }
        }
    }

    private func inline(_ s: String) -> Text {
        if let a = try? AttributedString(markdown: s, options: .init(interpretedSyntax: .inlineOnlyPreservingWhitespace)) {
            return Text(a)
        }
        return Text(s)
    }
}

/// The empty, loading and failed states every list shares.
struct LoadState<Content: View>: View {
    let loading: Bool
    let error: String?
    let empty: Bool
    var emptyTitle = "Nothing here"
    var emptySymbol = "tray"
    @ViewBuilder var content: () -> Content

    var body: some View {
        if let error {
            ContentUnavailableView(error, systemImage: "exclamationmark.triangle")
        } else if loading && empty {
            ProgressView().frame(maxWidth: .infinity, minHeight: 120)
        } else if empty {
            ContentUnavailableView(emptyTitle, systemImage: emptySymbol)
        } else {
            content()
        }
    }
}
