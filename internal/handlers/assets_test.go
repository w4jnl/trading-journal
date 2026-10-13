package handlers

import (
	"testing"
	"testing/fstest"
)

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
	// A development build keys on the content: a changed file, a new URL.
	a := fstest.MapFS{"css/app.css": {Data: []byte(".a{}")}, "js/x.js": {Data: []byte("1")}}
	b := fstest.MapFS{"css/app.css": {Data: []byte(".a{color:red}")}, "js/x.js": {Data: []byte("1")}}
	ha, hb := StaticHash(a), StaticHash(b)
	if len(ha) != 12 || ha == hb || ha != StaticHash(a) {
		t.Errorf("StaticHash = %q / %q", ha, hb)
	}
	if q := (&Handlers{Version: "abc-dirty", DevAssetHash: ha}).assetQuery(); q != "?v=dev-"+ha {
		t.Errorf("dirty assetQuery = %q", q)
	}
	if q := (&Handlers{Version: "v2.11.2", DevAssetHash: ha}).assetQuery(); q != "?v=v2.11.2" {
		t.Errorf("a release keeps its version: %q", q)
	}
}
