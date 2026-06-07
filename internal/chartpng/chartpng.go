// Package chartpng renders a trade-replay candlestick chart to a PNG, server
// side, with no DB/HTTP/network dependency. Input is a plain data struct
// (candles + entry/exit markers), so it's trivially unit-testable. The API's
// chart.png endpoint maps a handler chartPanel into a Panel and calls Render.
package chartpng

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sort"
	"strconv"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// OHLC is one candle; Time is unix seconds.
type OHLC struct {
	Time                           int64
	Open, High, Low, Close, Volume float64
}

// Marker is an entry/exit fill. Side is buy|sell, Role is entry|exit.
type Marker struct {
	Time  int64
	Price float64
	Side  string
	Role  string
}

// Panel is the renderer input: one contract's bars + markers + average lines.
type Panel struct {
	Title    string // e.g. "MESM6  5m"
	Bars     []OHLC // ascending by Time
	Markers  []Marker
	EntryAvg float64 // 0 = omit
	ExitAvg  float64 // 0 = omit
	Loc      *time.Location
}

// Options controls size + theme. Zero values default to 1000x500 dark.
type Options struct {
	Width, Height int
	Dark          bool
}

type theme struct{ bg, grid, text, up, down, entry, exit, avg color.RGBA }

func themeFor(dark bool) theme {
	if dark {
		return theme{
			bg: color.RGBA{0x12, 0x15, 0x1a, 0xff}, grid: color.RGBA{0x2a, 0x2e, 0x39, 0xff},
			text: color.RGBA{0xc8, 0xcc, 0xd4, 0xff}, up: color.RGBA{0x26, 0xa6, 0x9a, 0xff},
			down: color.RGBA{0xef, 0x53, 0x50, 0xff}, entry: color.RGBA{0x41, 0x8d, 0xff, 0xff},
			exit: color.RGBA{0xff, 0x9d, 0x42, 0xff}, avg: color.RGBA{0x8a, 0x8f, 0x9a, 0xff},
		}
	}
	return theme{
		bg: color.RGBA{0xff, 0xff, 0xff, 0xff}, grid: color.RGBA{0xe4, 0xe4, 0xe7, 0xff},
		text: color.RGBA{0x3f, 0x3f, 0x46, 0xff}, up: color.RGBA{0x16, 0xa3, 0x4a, 0xff},
		down: color.RGBA{0xdc, 0x26, 0x26, 0xff}, entry: color.RGBA{0x25, 0x63, 0xeb, 0xff},
		exit: color.RGBA{0xea, 0x58, 0x0c, 0xff}, avg: color.RGBA{0x71, 0x71, 0x7a, 0xff},
	}
}

// Render draws the panel and returns PNG bytes. Bars spaced by index (collapses
// session gaps, like the UI). Pure function: no I/O beyond the returned bytes.
func Render(p Panel, opt Options) ([]byte, error) {
	if opt.Width <= 0 {
		opt.Width = 1000
	}
	if opt.Height <= 0 {
		opt.Height = 500
	}
	th := themeFor(opt.Dark)
	W, H := opt.Width, opt.Height
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), &image.Uniform{th.bg}, image.Point{}, draw.Src)

	left, right, top, bottom := 64, 12, 22, 26
	plot := image.Rect(left, top, W-right, H-bottom)
	pw, ph := plot.Dx(), plot.Dy()

	drawString(img, th.text, p.Title, left, 15)

	if len(p.Bars) == 0 {
		drawString(img, th.text, "no data", left+8, top+24)
		return encode(img)
	}

	lo, hi := p.Bars[0].Low, p.Bars[0].High
	for _, b := range p.Bars {
		lo, hi = min2(lo, b.Low), max2(hi, b.High)
	}
	for _, m := range p.Markers {
		lo, hi = min2(lo, m.Price), max2(hi, m.Price)
	}
	for _, v := range []float64{p.EntryAvg, p.ExitAvg} {
		if v != 0 {
			lo, hi = min2(lo, v), max2(hi, v)
		}
	}
	if hi <= lo {
		hi = lo + 1
	}
	pad := (hi - lo) * 0.05
	lo, hi = lo-pad, hi+pad
	rng := hi - lo

	yOf := func(price float64) int { return plot.Min.Y + int(float64(ph)*(hi-price)/rng) }
	n := len(p.Bars)
	step := float64(pw) / float64(n)
	xOf := func(i int) int { return plot.Min.X + int((float64(i)+0.5)*step) }
	bodyW := int(step * 0.7)
	if bodyW < 1 {
		bodyW = 1
	}

	// horizontal grid + price labels
	const ticks = 5
	for i := 0; i <= ticks; i++ {
		price := hi - rng*float64(i)/float64(ticks)
		y := plot.Min.Y + ph*i/ticks
		fillRect(img, th.grid, plot.Min.X, y, plot.Max.X, y)
		drawString(img, th.text, fmtPrice(price), 4, y+4)
	}
	// time labels
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	for i := 0; i <= ticks; i++ {
		bi := i * (n - 1) / ticks
		x := xOf(bi)
		lbl := time.Unix(p.Bars[bi].Time, 0).In(loc).Format("15:04")
		drawString(img, th.text, lbl, x-14, plot.Max.Y+15)
	}

	// candles
	for i, b := range p.Bars {
		x := xOf(i)
		c := th.up
		if b.Close < b.Open {
			c = th.down
		}
		fillRect(img, c, x, yOf(b.High), x, yOf(b.Low)) // wick
		yo, yc := yOf(b.Open), yOf(b.Close)
		if yo > yc {
			yo, yc = yc, yo
		}
		if yc-yo < 1 {
			yc = yo + 1
		}
		fillRect(img, c, x-bodyW/2, yo, x+bodyW/2, yc)
	}

	// average entry/exit lines (dashed)
	if p.EntryAvg != 0 {
		dashedHLine(img, th.avg, plot.Min.X, plot.Max.X, yOf(p.EntryAvg))
	}
	if p.ExitAvg != 0 {
		dashedHLine(img, th.avg, plot.Min.X, plot.Max.X, yOf(p.ExitAvg))
	}

	// markers: triangle at the bar nearest the fill time, at the fill price
	times := make([]int64, n)
	for i, b := range p.Bars {
		times[i] = b.Time
	}
	for _, m := range p.Markers {
		bi := nearestIdx(times, m.Time)
		col := th.entry
		if m.Role == "exit" {
			col = th.exit
		}
		triangle(img, col, xOf(bi), yOf(m.Price), m.Side == "buy")
	}

	// legend
	drawString(img, th.entry, "entry", W-right-86, 15)
	drawString(img, th.exit, "exit", W-right-34, 15)

	return encode(img)
}

func encode(img *image.RGBA) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawString(img *image.RGBA, col color.RGBA, s string, x, y int) {
	d := &font.Drawer{Dst: img, Src: image.NewUniform(col), Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// fillRect fills the inclusive rectangle [x0,x1]x[y0,y1] (also used for 1px
// horizontal/vertical lines by passing equal coords).
func fillRect(img *image.RGBA, col color.RGBA, x0, y0, x1, y1 int) {
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	draw.Draw(img, image.Rect(x0, y0, x1+1, y1+1), &image.Uniform{col}, image.Point{}, draw.Src)
}

func dashedHLine(img *image.RGBA, col color.RGBA, x0, x1, y int) {
	for x := x0; x <= x1; x += 10 {
		end := x + 6
		if end > x1 {
			end = x1
		}
		fillRect(img, col, x, y, end, y)
	}
}

// triangle draws a small solid triangle centered at (cx,cy), apex up if up.
func triangle(img *image.RGBA, col color.RGBA, cx, cy int, up bool) {
	const size = 9
	for dy := 0; dy < size; dy++ {
		half := dy / 2
		if !up {
			half = (size - 1 - dy) / 2
		}
		y := cy - size/2 + dy
		fillRect(img, col, cx-half, y, cx+half, y)
	}
}

func fmtPrice(p float64) string { return strconv.FormatFloat(p, 'f', 2, 64) }

func nearestIdx(times []int64, t int64) int {
	i := sort.Search(len(times), func(i int) bool { return times[i] >= t })
	if i <= 0 {
		return 0
	}
	if i >= len(times) {
		return len(times) - 1
	}
	if t-times[i-1] <= times[i]-t {
		return i - 1
	}
	return i
}

func min2(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func max2(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
