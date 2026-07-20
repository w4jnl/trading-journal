package topstepx

import "testing"

func TestSymbolFromContractID(t *testing.T) {
	cases := []struct {
		in     string
		symbol string
		root   string
		ok     bool
	}{
		{"CON.F.US.MES.M26", "MESM6", "MES", true},
		{"CON.F.US.ES.U25", "ESU5", "ES", true},
		{"CON.F.US.ENQ.H26", "ENQH6", "ENQ", true},
		{"CON.F.US.MGC.Z25", "MGCZ5", "MGC", true},
		{" CON.F.US.MES.M26 ", "MESM6", "MES", true}, // stray whitespace
		{"CON.F.US.MES.A26", "", "", false},          // A is not a futures month code
		{"CON.F.US.MES.M2", "", "", false},           // 2-char month code
		{"CON.S.US.AAPL", "", "", false},             // not a future, wrong arity
		{"MESM6", "", "", false},                     // plain contract code
		{"", "", "", false},
		{"CON.F.US..M26", "", "", false}, // empty root
	}
	for _, c := range cases {
		symbol, root, ok := SymbolFromContractID(c.in)
		if ok != c.ok || symbol != c.symbol || root != c.root {
			t.Errorf("SymbolFromContractID(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, symbol, root, ok, c.symbol, c.root, c.ok)
		}
	}
}
