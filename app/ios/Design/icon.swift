// Renders the app icon: the brand's dark surface, the day's cumulative P&L line in accent
// teal with its soft fill, the zero line, and the last point lit. 1024 px, no alpha.
import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

let size = 1024.0
let cs = CGColorSpace(name: CGColorSpace.sRGB)!
let ctx = CGContext(data: nil, width: Int(size), height: Int(size), bitsPerComponent: 8, bytesPerRow: 0, space: cs, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)!

func rgb(_ hex: UInt32, _ a: CGFloat = 1) -> CGColor {
    CGColor(colorSpace: cs, components: [CGFloat((hex >> 16) & 0xff) / 255, CGFloat((hex >> 8) & 0xff) / 255, CGFloat(hex & 0xff) / 255, a])!
}

// background: bgRaised at the top to bg at the bottom
let bg = CGGradient(colorsSpace: cs, colors: [rgb(0x1c1c20), rgb(0x0a0a0b)] as CFArray, locations: [0, 1])!
ctx.drawLinearGradient(bg, start: CGPoint(x: 0, y: size), end: CGPoint(x: 0, y: 0), options: [])

// the line: a session that dips, then climbs. Points in icon space (origin bottom-left).
let pts: [CGPoint] = [(130, 500), (260, 540), (380, 380), (520, 470), (640, 640), (760, 600), (890, 790)].map { CGPoint(x: $0.0, y: $0.1) }
let zeroY = 500.0

// a smooth path through the points: Catmull-Rom, so the line bends only where the data does
func curve(_ p: [CGPoint]) -> CGMutablePath {
    let path = CGMutablePath()
    path.move(to: p[0])
    for i in 0..<(p.count - 1) {
        let p0 = p[max(i - 1, 0)], p1 = p[i], p2 = p[i + 1], p3 = p[min(i + 2, p.count - 1)]
        let c1 = CGPoint(x: p1.x + (p2.x - p0.x) / 6, y: p1.y + (p2.y - p0.y) / 6)
        let c2 = CGPoint(x: p2.x - (p3.x - p1.x) / 6, y: p2.y - (p3.y - p1.y) / 6)
        path.addCurve(to: p2, control1: c1, control2: c2)
    }
    return path
}
let line = curve(pts)

// the fill under the line down to the zero line, accent at 0.22 fading to 0
ctx.saveGState()
let fill = line.mutableCopy()!
fill.addLine(to: CGPoint(x: pts.last!.x, y: zeroY))
fill.addLine(to: CGPoint(x: pts[0].x, y: zeroY))
fill.closeSubpath()
ctx.addPath(fill)
ctx.clip()
let fillGrad = CGGradient(colorsSpace: cs, colors: [rgb(0x2EBABE, 0.34), rgb(0x2EBABE, 0.02)] as CFArray, locations: [0, 1])!
ctx.drawLinearGradient(fillGrad, start: CGPoint(x: 0, y: 790), end: CGPoint(x: 0, y: zeroY), options: [])
ctx.restoreGState()

// the zero line: border colour, dashed like the chart's
ctx.setStrokeColor(rgb(0x3f3f46))
ctx.setLineWidth(12)
ctx.setLineDash(phase: 0, lengths: [30, 26])
ctx.move(to: CGPoint(x: 130, y: zeroY))
ctx.addLine(to: CGPoint(x: 894, y: zeroY))
ctx.strokePath()
ctx.setLineDash(phase: 0, lengths: [])

// the line itself
ctx.setStrokeColor(rgb(0x2EBABE))
ctx.setLineWidth(58)
ctx.setLineCap(.round)
ctx.setLineJoin(.round)
ctx.addPath(line)
ctx.strokePath()

// the last point: a lit dot with a halo
let last = pts.last!
ctx.setFillColor(rgb(0x2EBABE, 0.25))
ctx.fillEllipse(in: CGRect(x: last.x - 84, y: last.y - 84, width: 168, height: 168))
ctx.setFillColor(rgb(0xfafafa))
ctx.fillEllipse(in: CGRect(x: last.x - 40, y: last.y - 40, width: 80, height: 80))

let img = ctx.makeImage()!
let out = URL(fileURLWithPath: CommandLine.arguments[1])
let dest = CGImageDestinationCreateWithURL(out as CFURL, UTType.png.identifier as CFString, 1, nil)!
CGImageDestinationAddImage(dest, img, nil)
CGImageDestinationFinalize(dest)
print("wrote", out.path)
