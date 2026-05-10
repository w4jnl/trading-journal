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
