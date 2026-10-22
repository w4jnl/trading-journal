import Foundation

/// An SVG attachment: a drawing an agent authored as text and the server stored sanitized
/// (no scripts, no external references). UIImage cannot decode one, so the app draws it with
/// WebKit; these helpers tell the two apart and size the frame before the drawing is laid out.
enum SVGDrawing {
    /// Whether the bytes are an SVG document rather than a raster image: markup with an
    /// <svg element near the top (the server accepts nothing else that starts with '<').
    nonisolated static func isSVG(_ data: Data) -> Bool {
        let head = data.prefix(4096)
        guard let text = String(data: head, encoding: .utf8) ?? String(data: head, encoding: .ascii) else { return false }
        let trimmed = text.drop { $0 == "\u{FEFF}" || $0.isWhitespace }
        return trimmed.hasPrefix("<") && trimmed.contains("<svg")
    }

    /// The drawing's width divided by its height, from the viewBox or the width and height
    /// attributes of the root; 16:9 when the root says nothing usable.
    nonisolated static func aspectRatio(of svg: String) -> Double {
        let fallback = 16.0 / 9.0
        guard let open = svg.range(of: "<svg"), let close = svg[open.upperBound...].firstIndex(of: ">") else { return fallback }
        let root = svg[open.upperBound..<close]
        if let box = attribute("viewBox", in: root) {
            let parts = box.split(whereSeparator: { $0 == " " || $0 == "," }).compactMap { Double($0) }
            if parts.count == 4, parts[2] > 0, parts[3] > 0 { return parts[2] / parts[3] }
        }
        if let w = attribute("width", in: root).flatMap(length), let h = attribute("height", in: root).flatMap(length), w > 0, h > 0 {
            return w / h
        }
        return fallback
    }

    private nonisolated static func attribute(_ name: String, in tag: Substring) -> String? {
        guard let r = tag.range(of: " \(name)=") ?? tag.range(of: "\n\(name)=") ?? tag.range(of: "\t\(name)=") else { return nil }
        let rest = tag[r.upperBound...]
        guard let quote = rest.first, quote == "\"" || quote == "'" else { return nil }
        let value = rest.dropFirst()
        guard let end = value.firstIndex(of: quote) else { return nil }
        return String(value[..<end])
    }

    /// A length attribute as a number: "200", "200px", "12.5em" all count by their number;
    /// a percentage says nothing about the shape.
    private nonisolated static func length(_ s: String) -> Double? {
        let t = s.trimmingCharacters(in: .whitespaces)
        if t.hasSuffix("%") { return nil }
        let digits = t.prefix { $0.isNumber || $0 == "." }
        return Double(digits)
    }
}
