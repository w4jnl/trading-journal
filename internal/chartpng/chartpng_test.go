package chartpng

import (
	"bytes"
	"image/png"
	"testing"
	"time"
)

func synthBars(n int) []OHLC {
	var bars []OHLC
	base := int64(1700000000)
	price := 5000.0
	for i := 0; i < n; i++ {
		o := price
		c := price + float64((i%5)-2)
		bars = append(bars, OHLC{
			Time: base + int64(i)*300, Open: o, High: max2(o, c) + 1, Low: min2(o, c) - 1,
			Close: c, Volume: 100,
		})
		price = c
	}
	return bars
}

func TestRender(t *testing.T) {
	bars := synthBars(50)
	p := Panel{
		Title: "MESM6  5m",
		Bars:  bars,
		Markers: []Marker{
			{Time: bars[5].Time, Price: bars[5].Close, Side: "buy", Role: "entry"},
			{Time: bars[40].Time, Price: bars[40].Close, Side: "sell", Role: "exit"},
		},
		EntryAvg: bars[5].Close,
		ExitAvg:  bars[40].Close,
		Loc:      time.UTC,
	}
	out, err := Render(p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("empty PNG")
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 1000 || b.Dy() != 500 {
		t.Errorf("bounds = %v, want 1000x500", b)
	}
}

func TestRenderDegenerate(t *testing.T) {
	// empty panel must not panic and must produce a valid PNG of the given size
	out, err := Render(Panel{Title: "x"}, Options{Width: 300, Height: 200})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() != 300 {
		t.Fatalf("empty render bad: err=%v bounds=%v", err, img.Bounds())
	}
	// single flat bar
	one := []OHLC{{Time: 1700000000, Open: 100, High: 100, Low: 100, Close: 100}}
	if _, err := Render(Panel{Title: "x", Bars: one}, Options{}); err != nil {
		t.Fatal(err)
	}
}
