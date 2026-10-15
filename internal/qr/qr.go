// Package qr encodes short text as a QR code (byte mode, error correction
// level M, versions 1 to 10: up to 213 bytes) and renders it as SVG. It
// exists for the phone pairing card in Settings, where a code holding a
// URL of about sixty characters must show without a script or a package;
// the symbol follows ISO/IEC 18004 and is checked against libqrencode in
// the tests.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Matrix is a finished symbol: Size modules a side, dark modules true.
type Matrix struct {
	Size int
	mods []bool
	fn   []bool // function modules (not data), kept for the mask step
}

// Dark reports the module at column x, row y.
func (m *Matrix) Dark(x, y int) bool { return m.mods[y*m.Size+x] }

func (m *Matrix) set(x, y int, dark bool) { m.mods[y*m.Size+x] = dark }

func (m *Matrix) setFn(x, y int, dark bool) {
	m.mods[y*m.Size+x] = dark
	m.fn[y*m.Size+x] = true
}

// maxVersion bounds the symbol: version 10 holds 213 bytes at level M,
// more than a pairing URL needs.
const maxVersion = 10

// ecBlocks is the level M block structure per version: the error
// correction codewords per block and the blocks as (count, data codewords).
var ecBlocks = [maxVersion + 1]struct {
	ec     int
	groups [][2]int
}{
	{}, // version 0 does not exist
	{10, [][2]int{{1, 16}}},
	{16, [][2]int{{1, 28}}},
	{26, [][2]int{{1, 44}}},
	{18, [][2]int{{2, 32}}},
	{24, [][2]int{{2, 43}}},
	{16, [][2]int{{4, 27}}},
	{18, [][2]int{{4, 31}}},
	{22, [][2]int{{2, 38}, {2, 39}}},
	{22, [][2]int{{3, 36}, {2, 37}}},
	{26, [][2]int{{4, 43}, {1, 44}}},
}

// alignPositions are the alignment pattern centres per version.
var alignPositions = [maxVersion + 1][]int{
	nil, nil, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
}

// dataCapacity is the data codewords a version holds at level M.
func dataCapacity(v int) int {
	n := 0
	for _, g := range ecBlocks[v].groups {
		n += g[0] * g[1]
	}
	return n
}

// Encode builds the smallest symbol that holds text in byte mode.
func Encode(text string) (*Matrix, error) {
	data := []byte(text)
	version := 0
	for v := 1; v <= maxVersion; v++ {
		cc := 8
		if v >= 10 {
			cc = 16
		}
		if 4+cc+8*len(data) <= 8*dataCapacity(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, errors.New("qr: text too long")
	}
	cw := codewords(data, version)
	m := newMatrix(version)
	m.drawFunctionPatterns(version)
	m.placeData(interleave(cw, version))
	best, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		m.applyMask(mask)
		m.drawFormat(mask)
		if p := m.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = mask, p
		}
		m.applyMask(mask) // masks are xor: undo
	}
	m.applyMask(best)
	m.drawFormat(best)
	return m, nil
}

// codewords is the data bit stream as codewords: mode, count, the bytes,
// the terminator, and padding to the version's capacity.
func codewords(data []byte, version int) []byte {
	var bits bitBuf
	bits.add(4, 4) // byte mode
	if version >= 10 {
		bits.add(len(data), 16)
	} else {
		bits.add(len(data), 8)
	}
	for _, b := range data {
		bits.add(int(b), 8)
	}
	capBits := 8 * dataCapacity(version)
	bits.add(0, min(4, capBits-bits.n))
	bits.add(0, (8-bits.n%8)%8)
	for pad := 0xEC; bits.n < capBits; pad ^= 0xEC ^ 0x11 {
		bits.add(pad, 8)
	}
	return bits.bytes()
}

type bitBuf struct {
	b []byte
	n int
}

func (b *bitBuf) add(v, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.b = append(b.b, 0)
		}
		if v>>uint(i)&1 == 1 {
			b.b[b.n/8] |= 0x80 >> uint(b.n%8)
		}
		b.n++
	}
}

func (b *bitBuf) bytes() []byte { return b.b }

// interleave splits the data into the version's blocks, appends each
// block's error correction, and interleaves the blocks codeword by
// codeword, data first then error correction.
func interleave(data []byte, version int) []byte {
	spec := ecBlocks[version]
	gen := rsGenerator(spec.ec)
	var blocks [][]byte
	var ecs [][]byte
	pos := 0
	for _, g := range spec.groups {
		for i := 0; i < g[0]; i++ {
			blk := data[pos : pos+g[1]]
			pos += g[1]
			blocks = append(blocks, blk)
			ecs = append(ecs, rsRemainder(blk, gen))
		}
	}
	var out []byte
	longest := 0
	for _, b := range blocks {
		longest = max(longest, len(b))
	}
	for i := 0; i < longest; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < spec.ec; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out
}

// ---- Reed-Solomon over GF(2^8) with the QR polynomial 0x11D ----

func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x1D)
		z ^= ((y >> uint(i)) & 1) * x
	}
	return z
}

// rsGenerator is the product of (x - a^i) for i in [0, degree).
func rsGenerator(degree int) []byte {
	g := make([]byte, degree)
	g[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range g {
			g[j] = gfMul(g[j], root)
			if j+1 < len(g) {
				g[j] ^= g[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return g
}

// rsRemainder is the error correction codewords of data for the generator.
func rsRemainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i, g := range gen {
			rem[i] ^= gfMul(g, factor)
		}
	}
	return rem
}

// ---- The matrix ----

func newMatrix(version int) *Matrix {
	size := 17 + 4*version
	return &Matrix{Size: size, mods: make([]bool, size*size), fn: make([]bool, size*size)}
}

func (m *Matrix) drawFunctionPatterns(version int) {
	// Timing patterns.
	for i := 0; i < m.Size; i++ {
		m.setFn(6, i, i%2 == 0)
		m.setFn(i, 6, i%2 == 0)
	}
	// Finders with their separators.
	m.drawFinder(3, 3)
	m.drawFinder(m.Size-4, 3)
	m.drawFinder(3, m.Size-4)
	// Alignment patterns, except those over a finder.
	pos := alignPositions[version]
	for i, cx := range pos {
		for j, cy := range pos {
			overFinder := (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0)
			if !overFinder {
				m.drawAlignment(cx, cy)
			}
		}
	}
	// Format areas are reserved now (drawn per mask) and the version info
	// once.
	m.drawFormat(0)
	m.drawVersion(version)
}

func (m *Matrix) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= m.Size || y >= m.Size {
				continue
			}
			d := max(abs(dx), abs(dy))
			m.setFn(x, y, d != 2 && d != 4)
		}
	}
}

func (m *Matrix) drawAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			m.setFn(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

// drawFormat writes the level M + mask format information in both places,
// BCH(15,5) coded and xor-masked as the standard requires.
func (m *Matrix) drawFormat(mask int) {
	data := 0<<3 | mask // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>uint(i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		m.setFn(8, i, bit(i))
	}
	m.setFn(8, 7, bit(6))
	m.setFn(8, 8, bit(7))
	m.setFn(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		m.setFn(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		m.setFn(m.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.setFn(8, m.Size-15+i, bit(i))
	}
	m.setFn(8, m.Size-8, true) // the dark module
}

// drawVersion writes the version information (versions 7 and up).
func (m *Matrix) drawVersion(version int) {
	if version < 7 {
		return
	}
	rem := version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := version<<12 | rem
	for i := 0; i < 18; i++ {
		bit := bits>>uint(i)&1 == 1
		a, b := m.Size-11+i%3, i/3
		m.setFn(a, b, bit)
		m.setFn(b, a, bit)
	}
}

// placeData writes the codewords into the data modules in the zigzag the
// standard prescribes: two-module columns from the right, up then down,
// skipping the vertical timing column.
func (m *Matrix) placeData(cw []byte) {
	i := 0
	total := len(cw) * 8
	for right := m.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < m.Size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = m.Size - 1 - vert
				}
				if !m.fn[y*m.Size+x] && i < total {
					m.set(x, y, cw[i/8]>>uint(7-i%8)&1 == 1)
					i++
				}
			}
		}
	}
}

// applyMask xors the mask pattern over the data modules (a second call
// undoes it).
func (m *Matrix) applyMask(mask int) {
	for y := 0; y < m.Size; y++ {
		for x := 0; x < m.Size; x++ {
			if m.fn[y*m.Size+x] {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (x/3+y/2)%2 == 0
			case 5:
				invert = x*y%2+x*y%3 == 0
			case 6:
				invert = (x*y%2+x*y%3)%2 == 0
			case 7:
				invert = ((x+y)%2+x*y%3)%2 == 0
			}
			if invert {
				m.mods[y*m.Size+x] = !m.mods[y*m.Size+x]
			}
		}
	}
}

// penalty scores a masked symbol the standard way: runs of one colour,
// 2x2 blocks, finder-like patterns, and the dark/light balance.
func (m *Matrix) penalty() int {
	score := 0
	line := func(get func(i int) bool) {
		run := 0
		var prev bool
		history := [7]int{}
		push := func(n int) {
			copy(history[1:], history[:6])
			history[0] = n
		}
		finderLike := func() bool {
			n := history[1]
			return n > 0 && history[2] == n && history[3] == 3*n && history[4] == n && history[5] == n && (history[0] >= 4*n || history[6] >= 4*n)
		}
		for i := 0; i < m.Size; i++ {
			c := get(i)
			if i > 0 && c == prev {
				run++
				if run == 5 {
					score += 3
				} else if run > 5 {
					score++
				}
				continue
			}
			if i > 0 {
				push(run)
				if !prev && finderLike() {
					score += 40
				}
			}
			prev, run = c, 1
		}
		push(run)
		if prev { // a dark run at the end: the light padding counts as 4
			push(0)
		}
		history[0] += 4
		if finderLike() {
			score += 40
		}
	}
	for y := 0; y < m.Size; y++ {
		line(func(x int) bool { return m.Dark(x, y) })
	}
	for x := 0; x < m.Size; x++ {
		line(func(y int) bool { return m.Dark(x, y) })
	}
	for y := 0; y < m.Size-1; y++ {
		for x := 0; x < m.Size-1; x++ {
			c := m.Dark(x, y)
			if c == m.Dark(x+1, y) && c == m.Dark(x, y+1) && c == m.Dark(x+1, y+1) {
				score += 3
			}
		}
	}
	dark := 0
	for _, d := range m.mods {
		if d {
			dark++
		}
	}
	total := m.Size * m.Size
	k := (abs(dark*20-total*10)+total-1)/total - 1
	score += 10 * k
	return score
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// SVG renders the symbol with module pixels a side and quiet modules of
// margin, as a self-contained <svg> that scales with its box.
func (m *Matrix) SVG(module, quiet int) string {
	if module <= 0 {
		module = 4
	}
	size := (m.Size + 2*quiet) * module
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" shape-rendering="crispEdges" role="img" aria-label="QR code">`, size, size, size, size)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="#fff"/>`, size, size)
	sb.WriteString(`<path fill="#000" d="`)
	for y := 0; y < m.Size; y++ {
		for x := 0; x < m.Size; x++ {
			if m.Dark(x, y) {
				fmt.Fprintf(&sb, "M%d %dh%dv%dh-%dz", (x+quiet)*module, (y+quiet)*module, module, module, module)
			}
		}
	}
	sb.WriteString(`"/></svg>`)
	return sb.String()
}
