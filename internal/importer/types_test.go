package importer

import "testing"

func TestDeriveSymbolRoot(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ESM6", "ES"},
		{"MESM6", "MES"},
		{"NQM6", "NQ"},
		{"MNQM6", "MNQ"},
		{"ESH26", "ES"},
		{"MESH26", "MES"},
		{"GCZ24", "GC"},
		{"CLM6", "CL"},
		{"MCLM6", "MCL"},
		{"EPM26", "EP"}, // canonicalization is the parser's job, not this function's
		{"AB", "AB"},    // too short to peel a suffix
		{"", ""},
	}
	for _, c := range cases {
		got := DeriveSymbolRoot(c.in)
		if got != c.want {
			t.Errorf("DeriveSymbolRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGuessFirm(t *testing.T) {
	cases := []struct {
		format Format
		ref    string
		source string
		want   string
	}{
		{FormatTopstepX, "EXPRESS-V2-100000-42718837", "", "topstep"},
		{FormatMotiveWave, "APEX-100001-28", "Rithmic", "apex"},
		{FormatMotiveWave, "LFE050-EXAMPLE1-TEST018", "Rithmic WS", "lucid"},
		{FormatMotiveWave, "LFF050-EXAMPLE2-PRO002", "Rithmic WS", "lucid"},
		{FormatMotiveWave, "U10000001", "Rithmic", "ibkr"},
		{FormatMotiveWave, "120042", "AMP Global Clearing - CQG", "amp"},
		{FormatMotiveWave, "unknown-account", "Rithmic", ""},
		{FormatUnknown, "anything", "", ""},
	}
	for _, c := range cases {
		got := GuessFirm(c.format, c.ref, c.source)
		if got != c.want {
			t.Errorf("GuessFirm(%v, %q, %q) = %q, want %q", c.format, c.ref, c.source, got, c.want)
		}
	}
}

func TestCanonicalizeSymbolRoot(t *testing.T) {
	cases := []struct {
		root, source string
		want         string
	}{
		{"EP", "AMP Global Clearing - CQG", "ES"},
		{"EP", "Rithmic", "EP"}, // not CQG-sourced; leave alone
		{"ES", "Rithmic", "ES"},
		{"MES", "AMP Global Clearing - CQG", "MES"},
	}
	for _, c := range cases {
		got := CanonicalizeSymbolRoot(c.root, c.source)
		if got != c.want {
			t.Errorf("CanonicalizeSymbolRoot(%q, %q) = %q, want %q", c.root, c.source, got, c.want)
		}
	}
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		content string
		want    Format
	}{
		{"Account,Ticket,Underlying,Symbol,Exchange,Time,Action,Quantity,Price,Source\nrow", FormatMotiveWave},
		{"Id,AccountName,ContractName,Status\nrow", FormatTopstepX},
		{"Foo,Bar,Baz", FormatUnknown},
		{"", FormatUnknown},
	}
	for _, c := range cases {
		got := DetectFormat([]byte(c.content))
		if got != c.want {
			t.Errorf("DetectFormat(%q) = %q, want %q", c.content, got, c.want)
		}
	}
}
