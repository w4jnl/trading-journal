package positions

import (
	"math"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

func fill(side string, qty, price float64) models.Execution {
	return models.Execution{Side: side, Qty: qty, Price: price, ExecTime: time.Now()}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The open average must follow the same FIFO the builder uses: partial closes
// retire the OLDEST lots, flips restart the position the other way.
func TestOpenFromFills(t *testing.T) {
	cases := []struct {
		name  string
		fills []models.Execution
		qty   float64
		avg   float64
	}{
		{"flat", nil, 0, 0},
		{"single long", []models.Execution{fill("buy", 2, 100)}, 2, 100},
		{"scale-in long", []models.Execution{fill("buy", 2, 100), fill("buy", 1, 110)}, 3, 110.0/3 + 200.0/3},
		{"partial close retires oldest lot", []models.Execution{fill("buy", 2, 100), fill("buy", 1, 110), fill("sell", 2, 105)}, 1, 110},
		{"partial close within a lot", []models.Execution{fill("buy", 3, 100), fill("buy", 2, 120), fill("sell", 1, 130)}, 4, (2*100 + 2*120) / 4.0},
		{"closed flat", []models.Execution{fill("buy", 2, 100), fill("sell", 2, 101)}, 0, 0},
		{"flip long to short", []models.Execution{fill("buy", 2, 100), fill("sell", 5, 90)}, -3, 90},
		{"short first", []models.Execution{fill("sell", 2, 50), fill("sell", 2, 40)}, -4, 45},
		{"short partial cover", []models.Execution{fill("sell", 2, 50), fill("sell", 2, 40), fill("buy", 3, 45)}, -1, 40},
		{"reopen after flat uses new price", []models.Execution{fill("buy", 1, 100), fill("sell", 1, 101), fill("buy", 2, 200)}, 2, 200},
		{"zero-qty fill ignored", []models.Execution{fill("buy", 0, 999), fill("buy", 1, 100)}, 1, 100},
	}
	for _, c := range cases {
		got := OpenFromFills(c.fills)
		if !approx(got.NetQty, c.qty) || !approx(got.AvgPrice, c.avg) {
			t.Errorf("%s: got net=%g avg=%g, want net=%g avg=%g", c.name, got.NetQty, got.AvgPrice, c.qty, c.avg)
		}
	}
}
