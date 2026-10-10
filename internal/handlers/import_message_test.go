package handlers

import "testing"

func TestHumanizeImportMessage(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		in   *string
		want string
	}{
		{nil, ""},
		{str("  "), ""},
		{str("unrecognized CSV header (neither MotiveWave nor TopstepX orders/trades)"), "Not a MotiveWave or TopstepX export: the header row was not recognised."},
		{str(`parse: missing column "Id"`), `The export is missing the "Id" column. Re-export with the default columns.`},
		{str("parse: read header: EOF"), "The file is empty, or cut off before the header row."},
		{str("parse: row 12: parse price: strconv.ParseFloat: parsing \"abc\": invalid syntax"), "Row 12 could not be read: the price is not a number. Nothing from this file was imported."},
		{str("parse: row 3: quantity must be positive, got 0"), "Row 3 could not be read: the quantity is zero or negative. Nothing from this file was imported."},
		{str("parse: row 7: parse time \"x\": cannot parse"), "Row 7 could not be read: the timestamp is not in the expected format. Nothing from this file was imported."},
		{str("parse: row 9: record on line 9: wrong number of fields"), "Row 9 could not be read: it has a different number of columns than the header. Nothing from this file was imported."},
		{str("ingest: position builder: boom"), "The file parsed, but saving its fills failed: position builder: boom."},
		{str(`ingest: account "X" is already orders-imported; delete it from /accounts and re-import to switch to trades-CSV`), `account "X" is already orders-imported; delete it from /accounts and re-import to switch to trades-CSV`},
		{str("something new"), "something new"},
	}
	for _, c := range cases {
		got := humanizeImportMessage(c.in)
		if got.Summary != c.want {
			t.Errorf("humanize(%v)\n got %q\nwant %q", deref(c.in), got.Summary, c.want)
		}
		if c.in != nil && got.Raw == "" && c.want != "" {
			t.Errorf("humanize(%q): raw text dropped", *c.in)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
