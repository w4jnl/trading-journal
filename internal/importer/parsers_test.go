package importer

import (
	"os"
	"testing"
)

const (
	motivewaveFixture = "../../exportData/MotivewaveExport.csv"
	topstepxFixture   = "../../exportData/TopstepxExport.csv"
)

func TestParseMotiveWaveFixture(t *testing.T) {
	f, err := os.Open(motivewaveFixture)
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	defer f.Close()

	fills, err := ParseMotiveWave(f)
	if err != nil {
		t.Fatalf("ParseMotiveWave: %v", err)
	}

	if len(fills) != 990 {
		t.Errorf("len(fills) = %d, want 990", len(fills))
	}

	roots := distinct(fills, func(f Fill) string { return f.SymbolRoot })
	wantRoots := []string{"ES", "MES"} // EP must be canonicalized to ES
	if !sameSet(roots, wantRoots) {
		t.Errorf("symbol roots after canonicalization = %v, want %v", roots, wantRoots)
	}

	accounts := distinct(fills, func(f Fill) string { return f.AccountRef })
	if len(accounts) != 17 {
		t.Errorf("distinct accounts = %d, want 17 (10 Apex + 5 Lucid + 1 IBKR + 1 AMP)", len(accounts))
	}

	for i, f := range fills {
		if f.Side != "buy" && f.Side != "sell" {
			t.Errorf("row %d: invalid side %q", i, f.Side)
			break
		}
		if f.Qty <= 0 {
			t.Errorf("row %d: qty %v not positive", i, f.Qty)
			break
		}
		if f.ExecTime.Location().String() != "UTC" {
			t.Errorf("row %d: time not UTC: %v", i, f.ExecTime)
			break
		}
		if f.BrokerExecID == "" {
			t.Errorf("row %d: empty BrokerExecID", i)
			break
		}
	}
}

func TestParseTopstepXFixture(t *testing.T) {
	f, err := os.Open(topstepxFixture)
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	defer f.Close()

	fills, err := ParseTopstepX(f)
	if err != nil {
		t.Fatalf("ParseTopstepX: %v", err)
	}

	if len(fills) != 49 {
		t.Errorf("len(fills) = %d, want 49 (Status=Filled rows only)", len(fills))
	}

	accounts := distinct(fills, func(f Fill) string { return f.AccountRef })
	if len(accounts) != 1 {
		t.Errorf("distinct accounts = %d, want 1", len(accounts))
	}

	roots := distinct(fills, func(f Fill) string { return f.SymbolRoot })
	wantRoots := []string{"MES", "MNQ"}
	if !sameSet(roots, wantRoots) {
		t.Errorf("symbol roots = %v, want %v", roots, wantRoots)
	}

	for i, f := range fills {
		if f.Side != "buy" && f.Side != "sell" {
			t.Errorf("row %d: invalid side %q", i, f.Side)
			break
		}
		if f.Qty <= 0 {
			t.Errorf("row %d: qty %v not positive", i, f.Qty)
			break
		}
		if f.BrokerExecID == "" {
			t.Errorf("row %d: empty BrokerExecID", i)
			break
		}
	}

	// Pin the side mapping: BrokerExecID 2917364597 is a Limit|Ask|Opening fill
	// on MESM6 @ 7273, which the trades CSV (Id 2539451779 on the same date /
	// price / contract) records as a Short entry. So Side="Ask" must produce
	// a SELL execution.
	var pinned *Fill
	for i := range fills {
		if fills[i].BrokerExecID == "2917364597" {
			pinned = &fills[i]
			break
		}
	}
	if pinned == nil {
		t.Fatalf("fixture missing reference fill 2917364597")
	}
	if pinned.Side != "sell" {
		t.Errorf("BrokerExecID 2917364597 (Ask|Opening, short entry): Side = %q, want %q",
			pinned.Side, "sell")
	}
}

func TestTopstepxSide(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"Ask", "sell", true},
		{"ask", "sell", true},
		{" ASK ", "sell", true},
		{"Bid", "buy", true},
		{"bid", "buy", true},
		{"Mid", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, err := topstepxSide(c.in)
		if c.wantOK {
			if err != nil {
				t.Errorf("topstepxSide(%q) err = %v, want nil", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("topstepxSide(%q) = %q, want %q", c.in, got, c.want)
			}
		} else if err == nil {
			t.Errorf("topstepxSide(%q) = %q, want error", c.in, got)
		}
	}
}

func distinct[T any](s []T, key func(T) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		k := key(v)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, v := range a {
		m[v] = true
	}
	for _, v := range b {
		if !m[v] {
			return false
		}
	}
	return true
}
