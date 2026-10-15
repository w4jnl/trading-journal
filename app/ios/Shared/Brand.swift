// The web app's design tokens (web/static/css/app.css) in code: the same colours in both
// themes, Inter for text and JetBrains Mono for numbers (bundled, SIL OFL 1.1, Fonts/). Views
// never name a colour or a font directly. Surfaces that must be dark whatever the system (the
// Live Activity, the notification card) take `Brand.dark` and set `.environment(\.colorScheme, .dark)`.
import CoreText
import SwiftUI
import UIKit

enum Brand {
    // MARK: colours, light / dark

    static let bg = dyn(0xFAFAFA, 0x0A0A0B)
    static let bgElev = dyn(0xFFFFFF, 0x131316)
    static let bgRaised = dyn(0xFFFFFF, 0x1C1C20)
    static let bgSunken = dyn(0xF4F4F5, 0x18181B)
    static let fg = dyn(0x18181B, 0xFAFAFA)
    static let fgMuted = dyn(0x71717A, 0xA1A1AA)
    static let fgSubtle = dyn(0x8B8B93, 0x71717A)
    static let border = dyn(0xE4E4E7, 0x27272A)
    static let borderStrong = dyn(0xD4D4D8, 0x3F3F46)
    static let accent = dyn(0x12999D, 0x2EBABE)
    static let accentSoft = dyn(0xE5F4F4, 0x2EBABE, darkAlpha: 0.16)
    static let accentText = dyn(0x0F7F84, 0x2EBABE)
    static let pos = dyn(0x16A34A, 0x22C55E)
    static let posText = dyn(0x15803D, 0x22C55E)
    static let posSoft = dyn(0xF0FDF4, 0x22C55E, darkAlpha: 0.12)
    static let neg = dyn(0xDC2626, 0xEF4444)
    static let negText = dyn(0xB91C1C, 0xEF4444)
    static let negSoft = dyn(0xFEF2F2, 0xEF4444, darkAlpha: 0.12)
    static let warn = dyn(0xD97706, 0xF59E0B)
    static let warnText = dyn(0x9A3412, 0xF59E0B)
    static let warnSoft = dyn(0xFFFBEB, 0xF59E0B, darkAlpha: 0.12)
    static let info = dyn(0x2563EB, 0x60A5FA)
    static let infoText = dyn(0x1E40AF, 0x60A5FA)
    static let infoSoft = dyn(0xEFF6FF, 0x60A5FA, darkAlpha: 0.12)
    static let group = dyn(0x7C3AED, 0xC084FC)
    static let retired = dyn(0xB45309, 0xFBBF24)

    /// The calendar's heat: four steps of green and of red, by |net| against the month's largest.
    static let heatPos: [Color] = [dyn(0xF0FDF4, 0x22C55E, darkAlpha: 0.08), dyn(0xDCFCE7, 0x22C55E, darkAlpha: 0.18), dyn(0x86EFAC, 0x22C55E, darkAlpha: 0.32), dyn(0x4ADE80, 0x22C55E, darkAlpha: 0.50)]
    static let heatNeg: [Color] = [dyn(0xFEF2F2, 0xEF4444, darkAlpha: 0.08), dyn(0xFEE2E2, 0xEF4444, darkAlpha: 0.18), dyn(0xFCA5A5, 0xEF4444, darkAlpha: 0.32), dyn(0xF87171, 0xEF4444, darkAlpha: 0.50)]

    /// The fixed dark set for branded surfaces that ignore the system theme.
    enum dark {
        static let bg = Color(hex: 0x0A0A0B)
        static let bgElev = Color(hex: 0x131316)
        static let fg = Color(hex: 0xFAFAFA)
        static let fgMuted = Color(hex: 0xA1A1AA)
        static let border = Color(hex: 0x27272A)
        static let accent = Color(hex: 0x2EBABE)
        static let pos = Color(hex: 0x22C55E)
        static let neg = Color(hex: 0xEF4444)
        static let warn = Color(hex: 0xF59E0B)
    }

    /// Green for gains and zero, red for losses, muted when unknown.
    static func pnl(_ v: Double?) -> Color {
        guard let v else { return fgMuted }
        return v < 0 ? negText : posText
    }

    private static func dyn(_ light: UInt32, _ dark: UInt32, darkAlpha: CGFloat = 1) -> Color {
        Color(UIColor { tc in
            tc.userInterfaceStyle == .dark ? UIColor(hex: dark, alpha: darkAlpha) : UIColor(hex: light)
        })
    }

    // MARK: type

    /// Inter at a Dynamic Type size (relative to the given text style), weight 400/500/600/700.
    static func text(_ size: CGFloat, weight: Font.Weight = .regular, relativeTo style: Font.TextStyle = .body) -> Font {
        _ = fontsRegistered
        let name: String
        switch weight {
        case .bold, .heavy, .black: name = "Inter-Bold"
        case .semibold: name = "Inter-SemiBold"
        case .medium: name = "Inter-Medium"
        default: name = "Inter-Regular"
        }
        if UIFont(name: name, size: size) != nil { return .custom(name, size: size, relativeTo: style) }
        return .system(size: size, weight: weight)
    }

    /// JetBrains Mono for numbers; fixed size, tabular digits.
    static func mono(_ size: CGFloat, bold: Bool = false) -> Font {
        _ = fontsRegistered
        let name = bold ? "JetBrainsMono-Bold" : "JetBrainsMono-Regular"
        if UIFont(name: name, size: size) != nil { return .custom(name, fixedSize: size).monospacedDigit() }
        return .system(size: size, weight: bold ? .bold : .regular, design: .monospaced).monospacedDigit()
    }

    /// UIAppFonts registers the bundle's fonts for the app; an extension's bundle, or a test
    /// host, may still need the explicit call.
    private static let fontsRegistered: Bool = {
        for name in ["Inter-Regular", "Inter-Medium", "Inter-SemiBold", "Inter-Bold", "JetBrainsMono-Regular", "JetBrainsMono-Bold"] where UIFont(name: name, size: 12) == nil {
            if let url = Bundle.main.url(forResource: name, withExtension: "ttf") {
                CTFontManagerRegisterFontsForURL(url as CFURL, .process, nil)
            }
        }
        return true
    }()

    // MARK: metrics

    static let radius: CGFloat = 6
    static let cardPadding: CGFloat = 16
}

extension Color {
    init(hex: UInt32) {
        self.init(red: Double(hex >> 16 & 0xFF) / 255, green: Double(hex >> 8 & 0xFF) / 255, blue: Double(hex & 0xFF) / 255)
    }
}

extension UIColor {
    convenience init(hex: UInt32, alpha: CGFloat = 1) {
        self.init(red: CGFloat(hex >> 16 & 0xFF) / 255, green: CGFloat(hex >> 8 & 0xFF) / 255, blue: CGFloat(hex & 0xFF) / 255, alpha: alpha)
    }
}
