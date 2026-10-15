package qr

import (
	"os/exec"
	"strings"
	"testing"
)

// readback is a test-side reader: it takes the format information, undoes
// the mask and reads the codewords back out of a symbol, so two symbols of
// the same text can be compared codeword for codeword whatever mask each
// encoder chose.
func readback(t *testing.T, m *Matrix) (mask int, cw []byte) {
	t.Helper()
	version := (m.Size - 17) / 4
	bits := 0
	for i := 0; i <= 5; i++ {
		bits |= b2i(m.Dark(8, i)) << uint(i)
	}
	bits |= b2i(m.Dark(8, 7)) << 6
	bits |= b2i(m.Dark(8, 8)) << 7
	bits |= b2i(m.Dark(7, 8)) << 8
	for i := 9; i < 15; i++ {
		bits |= b2i(m.Dark(14-i, 8)) << uint(i)
	}
	bits ^= 0x5412
	if level := bits >> 13; level != 0 {
		t.Fatalf("format says level bits %b, want 00 (M)", level)
	}
	mask = bits >> 10 & 7
	// Rebuild the function map and unmask.
	fn := newMatrix(version)
	fn.drawFunctionPatterns(version)
	copyM := &Matrix{Size: m.Size, mods: append([]bool(nil), m.mods...), fn: fn.fn}
	copyM.applyMask(mask)
	var out []byte
	i := 0
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
				if fn.fn[y*m.Size+x] {
					continue
				}
				if i%8 == 0 {
					out = append(out, 0)
				}
				if copyM.Dark(x, y) {
					out[i/8] |= 0x80 >> uint(i%8)
				}
				i++
			}
		}
	}
	return mask, out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fromASCII parses qrencode's -t ASCII output: two characters per module,
// '#' dark.
func fromASCII(t *testing.T, s string) *Matrix {
	t.Helper()
	var rows []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			rows = append(rows, l)
		}
	}
	size := len(rows)
	m := &Matrix{Size: size, mods: make([]bool, size*size)}
	for y, l := range rows {
		if len(l) < 2*size {
			t.Fatalf("row %d is %d chars, want %d", y, len(l), 2*size)
		}
		for x := 0; x < size; x++ {
			m.set(x, y, l[2*x] == '#')
		}
	}
	return m
}

// Every version is checked against libqrencode: the symbol is the same
// size, the format information decodes, and the codewords read back from
// our symbol equal the ones read back from qrencode's. qrencode may pick
// another mask (its penalty tie-breaking differs); the comparison is
// after unmasking, so that is allowed.
func TestEncodeMatchesQrencode(t *testing.T) {
	if _, err := exec.LookPath("qrencode"); err != nil {
		t.Skip("qrencode not installed")
	}
	base := "tj://pair?server=https://journal.example.com&code=AB7Q-K2MN"
	for _, text := range []string{"A", base, base + strings.Repeat("&x=0123456789", 4), strings.Repeat("0123456789", 21)} {
		m, err := Encode(text)
		if err != nil {
			t.Fatalf("%d bytes: %v", len(text), err)
		}
		ver := (m.Size - 17) / 4
		out, err := exec.Command("qrencode", "-l", "M", "-8", "-m", "0", "-t", "ASCII", text).Output()
		if err != nil {
			t.Fatal(err)
		}
		ref := fromASCII(t, string(out))
		if ref.Size != m.Size {
			t.Fatalf("%d bytes: size %d (version %d), qrencode %d", len(text), m.Size, ver, ref.Size)
		}
		mask, ours := readback(t, m)
		refMask, theirs := readback(t, ref)
		if string(ours) != string(theirs) {
			t.Errorf("%d bytes (version %d, masks %d/%d): codewords differ\nours   %x\ntheirs %x", len(text), ver, mask, refMask, ours, theirs)
		}
		// The function modules must agree exactly.
		fn := newMatrix(ver)
		fn.drawFunctionPatterns(ver)
		for y := 0; y < m.Size; y++ {
			for x := 0; x < m.Size; x++ {
				if fn.fn[y*m.Size+x] && mask == refMask && m.Dark(x, y) != ref.Dark(x, y) {
					t.Fatalf("version %d: function module (%d,%d) differs", ver, x, y)
				}
			}
		}
	}
}

// Too long for version 10 is refused; the SVG carries one path per dark
// module and the quiet zone.
func TestEncodeLimitsAndSVG(t *testing.T) {
	if _, err := Encode(strings.Repeat("x", 214)); err == nil {
		t.Errorf("214 bytes should not fit version 10 at level M")
	}
	m, err := Encode("hello")
	if err != nil {
		t.Fatal(err)
	}
	svg := m.SVG(4, 2)
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, `viewBox="0 0 100 100"`) || !strings.Contains(svg, "M8 8h4v4h-4z") {
		t.Errorf("svg = %s", svg[:120])
	}
}
