package handlers

import "testing"

func TestAssetVersion(t *testing.T) {
	cases := map[string]string{
		"v2.11.2":                    "v2.11.2",
		"19b2562 | 2026-09-22":       "19b2562-2026-09-22",
		"  v2.11.2 \n":               "v2.11.2",
		"a\"b/c d":                   "a-b-c-d",
		"dev":                        "",
		"":                           "",
		"19b2562-dirty | 2026-09-22": "",
	}
	for in, want := range cases {
		if got := AssetVersion(in); got != want {
			t.Errorf("AssetVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if q := (&Handlers{Version: "v2.11.2"}).assetQuery(); q != "?v=v2.11.2" {
		t.Errorf("assetQuery = %q", q)
	}
	if q := (&Handlers{Version: "dev"}).assetQuery(); q != "" {
		t.Errorf("dev assetQuery = %q, want empty", q)
	}
}
