// Server-side PDF rendering for the journal feed export. Replaces the
// fragile browser-print path: instead of relying on each browser's
// print pipeline (Safari blanking under macOS dark mode, etc.) we hand
// the caller the final PDF bytes.
//
// Pure-Go via go-pdf/fpdf for the page primitives. Markdown bodies parse
// through gomarkdown (already a dep); we walk the AST ourselves and
// emit fpdf calls per node so the PDF stays consistent with what the
// web UI renders.

package handlers

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/go-pdf/fpdf"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
)

// pdfHeader is the cover/header metadata. Title is the document title shown
// on the cover; GeneratedAt is a free-form timestamp string; FilterDesc is
// the filter summary line ("Filters — range … ; scope: …").
type pdfHeader struct {
	Title       string
	GeneratedAt string
	FilterDesc  string
}

// pdfEntry is one journal entry as the renderer consumes it.
//   - Title is the date/week heading line.
//   - Scope is "day" | "trade" | "week" | "group" — drives chip colour.
//   - Account / Trade / Tag are optional metadata; empty strings hide.
//   - Body is RAW markdown; the renderer parses it.
//   - Summary + Trades are the related-trades panel rendered above the body
//     so every entry — regardless of scope — shows the trades it was
//     written about.
type pdfEntry struct {
	Title    string
	Scope    string
	Account  string
	Trade    string
	Tag      string
	Strategy string
	Mistakes []string // review labels, display names
	Emotions []string
	Body     string
	Summary  GroupSummary
	Trades   []linkedTrade
}

// page geometry, in mm.
const (
	pageMarginX      = 18.0
	pageMarginTop    = 16.0
	pageMarginBottom = 18.0
	bodyFontSize     = 11.0
	bodyLineHeight   = 5.6 // ~1.5 line-height at 11pt
	smallFontSize    = 9.0
)

// RenderJournalPDF writes a complete PDF document to w. Returns an error
// only on PDF library failures; an empty `entries` slice still produces a
// valid PDF (cover + "no entries" notice).
func RenderJournalPDF(w io.Writer, header pdfHeader, entries []pdfEntry) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(pageMarginX, pageMarginTop, pageMarginX)
	pdf.SetAutoPageBreak(true, pageMarginBottom)
	pdf.AliasNbPages("")

	pageW, pageH := pdf.GetPageSize()
	// CP1252 (Windows Latin-1) is what Helvetica/Courier expect. Translating
	// each string before it hits the PDF turns "·", "—", "–", "•" from
	// UTF-8 mojibake into proper glyphs.
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	r := &pdfRenderer{
		pdf: pdf, header: header,
		pageWidth: pageW, pageHeight: pageH,
		contentWidth: pageW - 2*pageMarginX,
		tr:           tr,
	}

	// Footer "Page X of Y" using the AliasNbPages alias. {nb} is the
	// alias literal — fpdf substitutes it at render time, so do not run it
	// through the translator (the {} chars need to round-trip unchanged).
	pdf.SetFooterFunc(func() {
		pdf.SetY(-12)
		pdf.SetFont("Helvetica", "", 8)
		pdf.SetTextColor(120, 120, 130)
		pdf.CellFormat(0, 8, fmt.Sprintf("Page %d of {nb}", pdf.PageNo()),
			"", 0, "C", false, 0, "")
	})

	// Header strip on every page *after* the cover. The first page renders
	// a richer cover block instead (drawn in r.renderCover).
	pdf.SetHeaderFunc(func() {
		if pdf.PageNo() == 1 {
			return
		}
		pdf.SetY(8)
		pdf.SetFont("Helvetica", "B", 9)
		pdf.SetTextColor(80, 80, 90)
		pdf.CellFormat(0, 6, tr(header.Title), "", 0, "L", false, 0, "")
		pdf.Ln(6)
		pdf.SetDrawColor(228, 228, 231)
		pdf.SetLineWidth(0.2)
		y := pdf.GetY()
		pdf.Line(pageMarginX, y, pageW-pageMarginX, y)
		pdf.SetY(pageMarginTop)
	})

	pdf.AddPage()
	r.renderCover()

	if len(entries) == 0 {
		pdf.SetFont("Helvetica", "I", smallFontSize)
		pdf.SetTextColor(120, 120, 130)
		pdf.CellFormat(0, 8, tr("No entries match the active filters."), "", 0, "L", false, 0, "")
	}
	for i, e := range entries {
		if i > 0 {
			r.entrySeparator()
		}
		r.renderEntry(e)
	}

	return pdf.Output(w)
}

// pdfRenderer holds the fpdf handle plus rendering state. Methods operate
// directly on `r.pdf` and share the layout constants.
type pdfRenderer struct {
	pdf          *fpdf.Fpdf
	header       pdfHeader
	pageWidth    float64             // mm; cached from GetPageSize
	pageHeight   float64             // mm
	contentWidth float64             // page width minus left+right margins
	tr           func(string) string // UTF-8 → CP1252; called before any text reaches the PDF
}

func (r *pdfRenderer) renderCover() {
	pdf := r.pdf
	pdf.SetY(pageMarginTop)
	pdf.SetFont("Helvetica", "B", 20)
	pdf.SetTextColor(24, 24, 27)
	pdf.CellFormat(0, 9, r.tr(r.header.Title), "", 0, "L", false, 0, "")
	pdf.Ln(10)

	pdf.SetFont("Helvetica", "", smallFontSize)
	pdf.SetTextColor(113, 113, 122)
	meta := "Generated " + r.header.GeneratedAt
	if r.header.FilterDesc != "" {
		meta += "  ·  " + r.header.FilterDesc
	}
	pdf.MultiCell(r.contentWidth, 4.5, r.tr(meta), "", "L", false)
	pdf.Ln(2)

	// Hairline under the cover block.
	pdf.SetDrawColor(228, 228, 231)
	pdf.SetLineWidth(0.2)
	y := pdf.GetY()
	pdf.Line(pageMarginX, y, r.pageWidth-pageMarginX, y)
	pdf.Ln(6)
}

// entrySeparator draws a faint divider between entries.
func (r *pdfRenderer) entrySeparator() {
	pdf := r.pdf
	pdf.Ln(2)
	pdf.SetDrawColor(228, 228, 231)
	pdf.SetLineWidth(0.15)
	y := pdf.GetY()
	pdf.Line(pageMarginX, y, r.pageWidth-pageMarginX, y)
	pdf.Ln(4)
}

func (r *pdfRenderer) renderEntry(e pdfEntry) {
	pdf := r.pdf
	// Title row: date heading + scope chip + tag chip.
	pdf.SetFont("Helvetica", "B", 13)
	pdf.SetTextColor(24, 24, 27)
	title := r.tr(e.Title)
	titleW := pdf.GetStringWidth(title)
	pdf.CellFormat(titleW, 7, title, "", 0, "L", false, 0, "")

	pdf.SetX(pdf.GetX() + 3)
	r.drawChip(strings.ToUpper(e.Scope), scopeColor(e.Scope))
	if e.Tag != "" {
		pdf.SetX(pdf.GetX() + 2)
		r.drawTagChip(e.Tag)
	}
	if e.Strategy != "" {
		pdf.SetX(pdf.GetX() + 2)
		// Teal strategy chip, matching the on-screen feed.
		r.drawChip(e.Strategy, chipColor{224, 242, 242, 15, 118, 110})
	}
	pdf.Ln(8)

	// Meta line: "Account: X · Trade: Y …", then the review labels on a
	// line of their own ("Mistakes: … · Emotions: …").
	if e.Account != "" || e.Trade != "" {
		pdf.SetFont("Helvetica", "", smallFontSize)
		pdf.SetTextColor(82, 82, 91)
		var meta string
		if e.Account != "" {
			meta = "Account: " + e.Account
		}
		if e.Trade != "" {
			if meta != "" {
				meta += "  ·  "
			}
			meta += "Trade: " + e.Trade
		}
		pdf.MultiCell(r.contentWidth, 5, r.tr(meta), "", "L", false)
	}
	// Review labels as pills, like on screen: amber mistakes, blue emotions.
	if len(e.Mistakes) > 0 || len(e.Emotions) > 0 {
		var chips []pdfChip
		for _, m := range e.Mistakes {
			chips = append(chips, pdfChip{m, mistakeChipColor})
		}
		for _, m := range e.Emotions {
			chips = append(chips, pdfChip{m, emotionChipColor})
		}
		r.drawChipRow(chips)
	}

	// Trades block — same look for day / week / group / trade entries.
	r.renderTradesBlock(e)

	pdf.Ln(2)
	r.renderMarkdownBody(e.Body)
}

// renderTradesBlock prints the per-entry trades summary + bullet list.
// Suppressed entirely when the entry has no associated trades, so day
// entries with zero closed trades on that date stay clean. The bullet
// list itself is capped at exportTradeListCap; above that, only the
// summary line is shown so a 70-fill day doesn't blow the page count.
func (r *pdfRenderer) renderTradesBlock(e pdfEntry) {
	if e.Summary.N == 0 {
		return
	}
	pdf := r.pdf
	pdf.Ln(2)

	pdf.SetFont("Helvetica", "B", smallFontSize)
	pdf.SetTextColor(24, 24, 27)
	summary := fmt.Sprintf("Trades · %d · Net %s · Gross %s · Comm %.2f · %dW/%dL",
		e.Summary.N,
		signedMoney(e.Summary.Net),
		signedMoney(e.Summary.Gross),
		e.Summary.Commission,
		e.Summary.Wins, e.Summary.Losses)
	pdf.MultiCell(r.contentWidth, 5, r.tr(summary), "", "L", false)

	pdf.SetFont("Helvetica", "", smallFontSize)
	pdf.SetTextColor(82, 82, 91)
	if len(e.Trades) > exportTradeListCap {
		note := fmt.Sprintf("  %d trades not listed individually — see counts above.", len(e.Trades))
		pdf.MultiCell(r.contentWidth, 4.6, r.tr(note), "", "L", false)
		return
	}
	for _, lt := range e.Trades {
		net := "—"
		if lt.NetPnl != nil {
			net = signedMoney(*lt.NetPnl)
		}
		line := fmt.Sprintf("  •  %s · %s %s · qty %s · %s",
			lt.AccountName, lt.Symbol, lt.Direction, formatQty(lt.Qty), net)
		pdf.MultiCell(r.contentWidth, 4.6, r.tr(line), "", "L", false)
	}
}

// renderMarkdownBody parses the body as markdown and walks the AST,
// rendering each supported block node. Anything unsupported (tables,
// images, raw HTML, horizontal rules) prints as plain text from its
// flattened literal so users don't get silent drop-outs.
func (r *pdfRenderer) renderMarkdownBody(body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	p := parser.NewWithExtensions(parser.CommonExtensions)
	doc := p.Parse([]byte(body))
	for _, child := range doc.GetChildren() {
		r.renderBlock(child)
	}
}

func (r *pdfRenderer) renderBlock(node ast.Node) {
	switch n := node.(type) {
	case *ast.Heading:
		r.renderHeading(n)
	case *ast.Paragraph:
		r.renderParagraph(n)
	case *ast.List:
		r.renderList(n, 0)
	case *ast.CodeBlock:
		r.renderCodeBlock(n)
	case *ast.BlockQuote:
		r.renderBlockQuote(n)
	default:
		// Unsupported block type — fall back to a plain paragraph of the
		// concatenated text so we don't drop content silently.
		text := plainText(node)
		if strings.TrimSpace(text) != "" {
			r.drawRuns([]styledRun{{Text: text}}, 0)
			r.pdf.Ln(2)
		}
	}
}

func (r *pdfRenderer) renderHeading(h *ast.Heading) {
	pdf := r.pdf
	pdf.Ln(2)
	size := 14.0
	switch h.Level {
	case 1:
		size = 15.0
	case 2:
		size = 13.0
	case 3:
		size = 12.0
	default:
		size = 11.5
	}
	pdf.SetFont("Helvetica", "B", size)
	pdf.SetTextColor(24, 24, 27)
	r.drawRuns(collectRuns(h), 0)
	pdf.Ln(1)
	pdf.SetFont("Helvetica", "", bodyFontSize)
}

func (r *pdfRenderer) renderParagraph(p *ast.Paragraph) {
	pdf := r.pdf
	pdf.SetFont("Helvetica", "", bodyFontSize)
	pdf.SetTextColor(24, 24, 27)
	r.drawRuns(collectRuns(p), 0)
	pdf.Ln(2)
}

func (r *pdfRenderer) renderList(l *ast.List, depth int) {
	pdf := r.pdf
	ordered := l.ListFlags&ast.ListTypeOrdered != 0
	for i, child := range l.GetChildren() {
		item, ok := child.(*ast.ListItem)
		if !ok {
			continue
		}
		var marker string
		if ordered {
			marker = fmt.Sprintf("%d.", i+1)
		} else {
			marker = "•"
		}
		r.renderListItem(item, marker, depth)
	}
	if depth == 0 {
		pdf.Ln(1)
	}
}

func (r *pdfRenderer) renderListItem(item *ast.ListItem, marker string, depth int) {
	pdf := r.pdf
	pdf.SetFont("Helvetica", "", bodyFontSize)
	pdf.SetTextColor(24, 24, 27)
	indent := 6.0 * float64(depth+1)
	// Draw the bullet/number, then a hanging-indent paragraph for the runs.
	pdf.SetX(pageMarginX + indent - 5)
	pdf.CellFormat(5, bodyLineHeight, r.tr(marker), "", 0, "L", false, 0, "")

	// Build runs from the item's inline children. ListItem can contain
	// Paragraph blocks (loose lists) or run children directly (tight lists);
	// flatten both.
	runs := collectListItemRuns(item)
	r.drawRuns(runs, indent)

	// Nested lists inside this item.
	for _, ch := range item.GetChildren() {
		if sub, ok := ch.(*ast.List); ok {
			r.renderList(sub, depth+1)
		}
	}
}

func (r *pdfRenderer) renderCodeBlock(c *ast.CodeBlock) {
	pdf := r.pdf
	pdf.SetFont("Courier", "", 9)
	pdf.SetTextColor(40, 40, 50)
	src := strings.TrimRight(string(c.Literal), "\n")
	lines := strings.Split(src, "\n")
	// Measure block height to draw the background rect first.
	const padX, padY = 3.0, 2.5
	lineH := 4.4
	blockH := float64(len(lines))*lineH + 2*padY
	r.ensureSpace(blockH + 2)
	xStart := pageMarginX
	yStart := pdf.GetY()
	pdf.SetFillColor(244, 244, 245)
	pdf.SetDrawColor(228, 228, 231)
	pdf.SetLineWidth(0.15)
	pdf.Rect(xStart, yStart, r.contentWidth, blockH, "FD")
	pdf.SetY(yStart + padY)
	for _, ln := range lines {
		pdf.SetX(xStart + padX)
		pdf.CellFormat(r.contentWidth-2*padX, lineH, r.tr(ln), "", 0, "L", false, 0, "")
		pdf.Ln(lineH)
	}
	pdf.SetY(yStart + blockH + 2)
	pdf.SetFont("Helvetica", "", bodyFontSize)
}

func (r *pdfRenderer) renderBlockQuote(b *ast.BlockQuote) {
	pdf := r.pdf
	pdf.SetFont("Helvetica", "I", bodyFontSize)
	pdf.SetTextColor(82, 82, 91)
	startY := pdf.GetY()
	for _, child := range b.GetChildren() {
		r.renderBlock(child)
	}
	endY := pdf.GetY()
	// Draw a 2pt left border across the quote's vertical span.
	pdf.SetDrawColor(180, 180, 186)
	pdf.SetLineWidth(0.7)
	pdf.Line(pageMarginX+1.5, startY, pageMarginX+1.5, endY-1)
	pdf.SetFont("Helvetica", "", bodyFontSize)
	pdf.SetTextColor(24, 24, 27)
}

// ----- inline rendering --------------------------------------------------

// styledRun is one stylable inline fragment. Bold/Italic/Code toggle font
// flags; Link adds a clickable href.
type styledRun struct {
	Text   string
	Bold   bool
	Italic bool
	Code   bool
	Link   string // href; empty = not a link
}

func collectRuns(n ast.Node) []styledRun {
	var out []styledRun
	walkInline(n, styledRun{}, &out)
	return out
}

// collectListItemRuns flattens a ListItem's inline content, handling both
// tight (direct text/inline children) and loose (single Paragraph child)
// layouts. Nested lists are NOT included here — the caller recurses for
// those after the runs are drawn.
func collectListItemRuns(item *ast.ListItem) []styledRun {
	var out []styledRun
	for _, child := range item.GetChildren() {
		if _, isList := child.(*ast.List); isList {
			continue
		}
		walkInline(child, styledRun{}, &out)
	}
	return out
}

func walkInline(node ast.Node, base styledRun, out *[]styledRun) {
	switch n := node.(type) {
	case *ast.Text:
		base.Text = string(n.Literal)
		*out = append(*out, base)
	case *ast.Strong:
		next := base
		next.Bold = true
		for _, ch := range n.GetChildren() {
			walkInline(ch, next, out)
		}
	case *ast.Emph:
		next := base
		next.Italic = true
		for _, ch := range n.GetChildren() {
			walkInline(ch, next, out)
		}
	case *ast.Code:
		next := base
		next.Code = true
		next.Text = string(n.Literal)
		*out = append(*out, next)
	case *ast.Link:
		next := base
		next.Link = string(n.Destination)
		for _, ch := range n.GetChildren() {
			walkInline(ch, next, out)
		}
	case *ast.Softbreak, *ast.Hardbreak:
		base.Text = " "
		*out = append(*out, base)
	default:
		// Container with inline children — recurse.
		for _, ch := range node.GetChildren() {
			walkInline(ch, base, out)
		}
	}
}

// drawRuns lays out a sequence of styled inline runs with word-wrap inside
// a column starting at `pageMarginX + leftIndent` and ending at the right
// margin. Handles its own page-break for long content.
func (r *pdfRenderer) drawRuns(runs []styledRun, leftIndent float64) {
	pdf := r.pdf
	xStart := pageMarginX + leftIndent
	xEnd := r.pageWidth - pageMarginX
	maxW := xEnd - xStart

	pdf.SetX(xStart)
	cur := xStart

	moveToNextLine := func() {
		pdf.Ln(bodyLineHeight)
		r.ensureSpace(bodyLineHeight)
		pdf.SetX(xStart)
		cur = xStart
	}

	for _, run := range runs {
		applyRunFont(pdf, run)
		// Split run into tokens that preserve word boundaries. Tokens are
		// non-space sequences; runs of whitespace collapse to single spaces.
		words := tokenize(run.Text)
		for _, w := range words {
			if w == " " {
				spaceW := pdf.GetStringWidth(" ")
				if cur+spaceW > xEnd {
					moveToNextLine()
					continue // skip leading space on new line
				}
				if cur == xStart {
					continue // don't lead a line with a space
				}
				pdf.CellFormat(spaceW, bodyLineHeight, " ", "", 0, "L", false, 0, "")
				cur += spaceW
				continue
			}
			tw := r.tr(w)
			wordW := pdf.GetStringWidth(tw)
			if wordW > maxW {
				// Word longer than the column — print as-is, will overflow
				// slightly. Rare in practice; avoids an infinite loop.
				if cur != xStart {
					moveToNextLine()
				}
				if run.Link != "" {
					pdf.CellFormat(wordW, bodyLineHeight, tw, "", 0, "L", false, 0, run.Link)
				} else {
					pdf.CellFormat(wordW, bodyLineHeight, tw, "", 0, "L", false, 0, "")
				}
				cur += wordW
				continue
			}
			if cur+wordW > xEnd {
				moveToNextLine()
			}
			if run.Link != "" {
				pdf.SetTextColor(67, 56, 202) // indigo
				pdf.CellFormat(wordW, bodyLineHeight, tw, "", 0, "L", false, 0, run.Link)
				pdf.SetTextColor(24, 24, 27)
			} else {
				pdf.CellFormat(wordW, bodyLineHeight, tw, "", 0, "L", false, 0, "")
			}
			cur += wordW
		}
	}
	pdf.Ln(bodyLineHeight)
}

func applyRunFont(pdf *fpdf.Fpdf, run styledRun) {
	family := "Helvetica"
	style := ""
	if run.Code {
		family = "Courier"
	}
	if run.Bold {
		style += "B"
	}
	if run.Italic {
		style += "I"
	}
	pdf.SetFont(family, style, bodyFontSize)
}

// tokenize splits s into a slice that preserves word boundaries by
// alternating non-space tokens and single-space separators. The renderer
// uses ' ' as the only whitespace token; newlines collapse to spaces.
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, ch := range s {
		switch ch {
		case ' ', '\t', '\n':
			flush()
			if len(out) == 0 || out[len(out)-1] != " " {
				out = append(out, " ")
			}
		default:
			cur.WriteRune(ch)
		}
	}
	flush()
	return out
}

// ensureSpace forces a page break if the requested vertical space won't
// fit on the current page. Used before drawing block elements that the
// auto-break logic can't pre-measure (code blocks, multi-line chips).
func (r *pdfRenderer) ensureSpace(need float64) {
	pdf := r.pdf
	if pdf.GetY()+need > r.pageHeight-pageMarginBottom {
		pdf.AddPage()
	}
}

// ----- chips -------------------------------------------------------------

type chipColor struct {
	bgR, bgG, bgB int
	fgR, fgG, fgB int
}

func scopeColor(scope string) chipColor {
	switch scope {
	case "trade":
		return chipColor{238, 242, 255, 67, 56, 202} // indigo soft / indigo
	case "week":
		return chipColor{254, 243, 199, 146, 64, 14} // amber soft / amber
	case "group":
		return chipColor{245, 230, 255, 126, 34, 206} // purple soft / purple
	default: // "day"
		return chipColor{244, 244, 245, 82, 82, 91}
	}
}

func tagColor(tag string) chipColor {
	switch tag {
	case "good":
		return chipColor{240, 253, 244, 22, 163, 74}
	case "bad":
		return chipColor{254, 242, 242, 220, 38, 38}
	case "neutral":
		return chipColor{238, 242, 255, 79, 70, 229}
	default:
		return chipColor{244, 244, 245, 82, 82, 91}
	}
}

// drawChip emits a small filled-rect pill at the current x/y. Advances X
// by the chip width + a small gap.
func (r *pdfRenderer) drawChip(label string, c chipColor) {
	pdf := r.pdf
	pdf.SetFont("Helvetica", "B", 7)
	t := r.tr(label)
	w := pdf.GetStringWidth(t) + 4
	h := 4.0
	x := pdf.GetX()
	y := pdf.GetY() + 1.5 // shift down to align baseline with title
	pdf.SetFillColor(c.bgR, c.bgG, c.bgB)
	pdf.SetTextColor(c.fgR, c.fgG, c.fgB)
	pdf.RoundedRect(x, y, w, h, 1.2, "1234", "F")
	pdf.SetXY(x, y-0.3)
	pdf.CellFormat(w, h, t, "", 0, "C", false, 0, "")
	pdf.SetXY(x+w, y-1.5)
	pdf.SetTextColor(24, 24, 27)
	pdf.SetFont("Helvetica", "", bodyFontSize)
}

func (r *pdfRenderer) drawTagChip(tag string) {
	r.drawChip(strings.ToUpper(tag), tagColor(tag))
}

// pdfChip is one pill in a wrapping row.
type pdfChip struct {
	Label string
	Color chipColor
}

// Review-label pills, the on-screen amber / blue (app.css label-pill).
var (
	mistakeChipColor = chipColor{255, 251, 235, 154, 52, 18}
	emotionChipColor = chipColor{239, 246, 255, 30, 64, 175}
)

// drawChipRow lays pills left to right from the margin, wrapping to a new
// line when the next one would cross the content width, and ends the row.
func (r *pdfRenderer) drawChipRow(chips []pdfChip) {
	pdf := r.pdf
	if len(chips) == 0 {
		return
	}
	const gap, lineStep = 1.5, 6.0
	x0 := pageMarginX
	pdf.SetX(x0)
	for _, c := range chips {
		pdf.SetFont("Helvetica", "B", 7)
		w := pdf.GetStringWidth(r.tr(c.Label)) + 4
		if pdf.GetX() > x0 && pdf.GetX()+w > x0+r.contentWidth {
			pdf.Ln(lineStep)
			pdf.SetX(x0)
		}
		r.drawChip(c.Label, c.Color)
		pdf.SetX(pdf.GetX() + gap)
	}
	pdf.Ln(lineStep)
}

// ----- plain-text fallback for unsupported node types --------------------

func plainText(n ast.Node) string {
	var b bytes.Buffer
	collectText(n, &b)
	return b.String()
}

func collectText(n ast.Node, b *bytes.Buffer) {
	switch v := n.(type) {
	case *ast.Text:
		b.Write(v.Literal)
	case *ast.Code:
		b.Write(v.Literal)
	case *ast.CodeBlock:
		b.Write(v.Literal)
	}
	for _, ch := range n.GetChildren() {
		collectText(ch, b)
	}
}
