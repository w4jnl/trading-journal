package topstepx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// RetrieveBars posts the documented request shape and returns the bars in
// chronological order (the API answers newest first); an unsuccessful
// envelope becomes an APIError.
func TestRetrieveBars(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/History/retrieveBars" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("unexpected %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["contractId"] == "CON.F.US.EP.U26" {
			_, _ = w.Write([]byte(`{"success":true,"errorCode":0,"bars":[
			  {"t":"2026-09-07T11:34:00Z","o":7721.5,"h":7722.25,"l":7721.25,"c":7722.0,"v":120},
			  {"t":"2026-09-07T11:33:00Z","o":7720.75,"h":7721.75,"l":7720.5,"c":7721.5,"v":98}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":false,"errorCode":2,"errorMessage":"unknown contract"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	start := time.Date(2026, 9, 7, 8, 34, 0, 0, time.UTC)
	bars, err := c.RetrieveBars(context.Background(), "tok-1", "CON.F.US.EP.U26", UnitMinute, 1, start, start.Add(3*time.Hour), 5, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 || bars[0].Close != 7721.5 || bars[1].Close != 7722.0 || !bars[1].Time.Equal(time.Date(2026, 9, 7, 11, 34, 0, 0, time.UTC)) {
		t.Errorf("bars = %+v", bars)
	}
	if got["unit"] != float64(2) || got["unitNumber"] != float64(1) || got["limit"] != float64(5) || got["includePartialBar"] != true || got["live"] != false || got["startTime"] != "2026-09-07T08:34:00Z" {
		t.Errorf("request body = %v", got)
	}
	if _, err := c.RetrieveBars(context.Background(), "tok-1", "CON.F.US.XX.U26", UnitMinute, 1, start, start.Add(time.Hour), 1, true, false); err == nil {
		t.Errorf("unsuccessful envelope must error")
	}
}

// ContractIDForSymbol is the inverse of SymbolFromContractID for the roots
// the journal trades, choosing the decade that puts the contract near now.
func TestContractIDForSymbol(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	cases := map[[2]string]string{
		{"ESU6", "ES"}:   "CON.F.US.EP.U26",
		{"EPU6", "EP"}:   "CON.F.US.EP.U26",
		{"MESU6", "MES"}: "CON.F.US.MES.U26",
		{"GCZ6", "GC"}:   "CON.F.US.GCE.Z26",
		{"MGCZ6", "MGC"}: "CON.F.US.MGC.Z26",
		{"NQZ6", "NQ"}:   "CON.F.US.ENQ.Z26",
		{"CLX6", "CL"}:   "CON.F.US.CLE.X26",
		{"RTYZ6", "RTY"}: "CON.F.US.RTY.Z26",
		{"esh1", "es"}:   "CON.F.US.EP.H31", // 2031 is within +8y, 2021 is not within -2y
		{"ESZ4", "ES"}:   "CON.F.US.EP.Z24", // 2024 is within -2y
	}
	for in, want := range cases {
		got, ok := ContractIDForSymbol(in[0], in[1], now)
		if !ok || got != want {
			t.Errorf("ContractIDForSymbol(%q,%q) = %q,%v; want %q", in[0], in[1], got, ok, want)
		}
		// Round-trips through the parser (root code differs by design for ES/EP).
		if sym, _, ok := SymbolFromContractID(got); !ok || sym[len(sym)-2:] != want[len(want)-3:len(want)-2]+want[len(want)-1:] {
			t.Errorf("%s does not round-trip: %q %v", got, sym, ok)
		}
	}
	for _, in := range [][2]string{{"ZBZ6", "ZB"}, {"ES", "ES"}, {"ESU", "ES"}, {"ESUX", "ES"}, {"", ""}} {
		if id, ok := ContractIDForSymbol(in[0], in[1], now); ok {
			t.Errorf("ContractIDForSymbol(%q,%q) = %q, want !ok", in[0], in[1], id)
		}
	}
}
