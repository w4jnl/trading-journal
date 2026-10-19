// The journal's notes are markdown written by the trader and by the AI review. This splits a
// note into blocks the view renders one by one: headings, paragraphs, list items, code, rules,
// images (attachments the API serves), pipe tables and the "_Note added …_" markers. HTML
// comments (the GEX annotation's machine-readable tail) are dropped, as the web does. Pure, tested.
import Foundation

enum Markdown {
    enum Block: Equatable, Sendable {
        case heading(level: Int, text: String)
        case paragraph(String)
        case listItem(text: String, ordered: Bool, indent: Int)
        case code(String)
        case rule
        case image(alt: String, href: String)
        case noteMarker(String) // "_Note added 2026-10-14 21:16 CEST_"
        case quote(String)
        case table(header: [String], rows: [[String]])
    }

    nonisolated static func blocks(_ source: String) -> [Block] {
        var out: [Block] = []
        var paragraph: [String] = []
        var code: [String]?
        func flush() {
            if !paragraph.isEmpty {
                out.append(.paragraph(paragraph.joined(separator: " ")))
                paragraph = []
            }
        }
        let lines = source.replacingOccurrences(of: "\r\n", with: "\n")
            .replacingOccurrences(of: "<!--[\\s\\S]*?-->", with: "", options: .regularExpression)
            .split(separator: "\n", omittingEmptySubsequences: false)
            .map(String.init)
        var i = 0
        while i < lines.count {
            let line = lines[i]
            i += 1
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if var c = code {
                if trimmed.hasPrefix("```") {
                    out.append(.code(c.joined(separator: "\n")))
                    code = nil
                } else {
                    c.append(line)
                    code = c
                }
                continue
            }
            if trimmed.hasPrefix("```") {
                flush()
                code = []
                continue
            }
            if trimmed.isEmpty {
                flush()
                continue
            }
            if trimmed == "---" || trimmed == "***" || trimmed == "___" {
                flush()
                out.append(.rule)
                continue
            }
            if let img = image(trimmed) {
                flush()
                out.append(img)
                continue
            }
            if trimmed.hasPrefix("#") {
                let level = trimmed.prefix(while: { $0 == "#" }).count
                if level <= 6, trimmed.dropFirst(level).first == " " {
                    flush()
                    out.append(.heading(level: level, text: String(trimmed.dropFirst(level)).trimmingCharacters(in: .whitespaces)))
                    continue
                }
            }
            if trimmed.hasPrefix("_Note added") && trimmed.hasSuffix("_") || trimmed.hasPrefix("*Note added") && trimmed.hasSuffix("*") {
                flush()
                out.append(.noteMarker(String(trimmed.dropFirst().dropLast())))
                continue
            }
            if trimmed.hasPrefix("> ") || trimmed == ">" {
                flush()
                let text = String(trimmed.dropFirst(trimmed == ">" ? 1 : 2))
                if case .quote(let prev)? = out.last {
                    out[out.count - 1] = .quote(prev + " " + text)
                } else {
                    out.append(.quote(text))
                }
                continue
            }
            if trimmed.hasPrefix("|"), i < lines.count, let cells = tableCells(lines[i]), cells.allSatisfy(isTableDelimiter) {
                // a pipe table: header, delimiter row, then every pipe line that follows
                flush()
                var rows: [[String]] = []
                i += 1
                while i < lines.count, lines[i].trimmingCharacters(in: .whitespaces).hasPrefix("|"), let row = tableCells(lines[i]) {
                    rows.append(row)
                    i += 1
                }
                out.append(.table(header: tableCells(trimmed) ?? [], rows: rows))
                continue
            }
            if let item = listItem(line) {
                flush()
                out.append(item)
                continue
            }
            if let last = out.last, case .listItem(let text, let ordered, let indent) = last, line.hasPrefix("  "), paragraph.isEmpty {
                // a wrapped list item's continuation line
                out[out.count - 1] = .listItem(text: text + " " + trimmed, ordered: ordered, indent: indent)
                continue
            }
            paragraph.append(trimmed)
        }
        if let c = code { out.append(.code(c.joined(separator: "\n"))) }
        flush()
        return out
    }

    /// The cells of a pipe-table line, outer pipes dropped and each cell trimmed; nil off a table line.
    nonisolated static func tableCells(_ line: String) -> [String]? {
        var t = line.trimmingCharacters(in: .whitespaces)
        guard t.hasPrefix("|") else { return nil }
        t.removeFirst()
        if t.hasSuffix("|") { t.removeLast() }
        return t.replacingOccurrences(of: "\\|", with: "\u{1}").split(separator: "|", omittingEmptySubsequences: false)
            .map { $0.replacingOccurrences(of: "\u{1}", with: "|").trimmingCharacters(in: .whitespaces) }
    }

    /// "---", ":---:" and the like: a cell of the row under a table's header.
    nonisolated static func isTableDelimiter(_ cell: String) -> Bool {
        var c = Substring(cell)
        if c.hasPrefix(":") { c = c.dropFirst() }
        if c.hasSuffix(":") { c = c.dropLast() }
        return !c.isEmpty && c.allSatisfy { $0 == "-" }
    }

    /// "![alt](/api/v1/attachments/3)" on a line of its own.
    nonisolated static func image(_ line: String) -> Block? {
        guard line.hasPrefix("!["), let close = line.firstIndex(of: "]"), line[line.index(after: close)...].hasPrefix("("), line.hasSuffix(")") else { return nil }
        let alt = String(line[line.index(line.startIndex, offsetBy: 2)..<close])
        let href = String(line[line.index(close, offsetBy: 2)..<line.index(before: line.endIndex)])
        guard !href.isEmpty, !href.contains(" ") else { return nil }
        return .image(alt: alt, href: href)
    }

    nonisolated static func listItem(_ line: String) -> Block? {
        let indent = line.prefix(while: { $0 == " " }).count / 2
        let t = line.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("- [ ] ") || t.hasPrefix("- [x] ") || t.hasPrefix("- [X] ") {
            return .listItem(text: (t.hasPrefix("- [ ] ") ? "☐ " : "☑ ") + t.dropFirst(6), ordered: false, indent: indent)
        }
        for bullet in ["- ", "* ", "+ "] where t.hasPrefix(bullet) {
            return .listItem(text: String(t.dropFirst(2)), ordered: false, indent: indent)
        }
        let digits = t.prefix(while: { $0.isNumber })
        if !digits.isEmpty, t.dropFirst(digits.count).hasPrefix(". ") {
            return .listItem(text: String(t.dropFirst(digits.count + 2)), ordered: true, indent: indent)
        }
        return nil
    }

    /// The first words of a note for a list row: markup stripped, one line.
    nonisolated static func snippet(_ source: String, limit: Int = 140) -> String {
        var words: [String] = []
        for b in blocks(source) {
            switch b {
            case .paragraph(let s), .listItem(let s, _, _), .quote(let s), .heading(_, let s):
                if s.hasPrefix("|") { continue } // a table row
                words.append(plain(s))
            default: continue
            }
            if words.joined(separator: " ").count > limit { break }
        }
        let s = words.joined(separator: " ")
        if s.count <= limit { return s }
        return String(s.prefix(limit)).trimmingCharacters(in: .whitespaces) + "…"
    }

    /// Inline markup removed: **bold**, *em*, `code`, [text](link).
    nonisolated static func plain(_ s: String) -> String {
        var t = s
        for mark in ["**", "__", "`", "*", "_"] { t = t.replacingOccurrences(of: mark, with: "") }
        while let open = t.firstIndex(of: "["), let mid = t[open...].firstIndex(of: "]"), t[mid...].dropFirst().hasPrefix("("), let close = t[mid...].firstIndex(of: ")") {
            let text = t[t.index(after: open)..<mid]
            t.replaceSubrange(open...close, with: text)
        }
        return t
    }
}
