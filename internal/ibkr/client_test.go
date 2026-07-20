package ibkr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientStatusAndPaths(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/api/iserver/auth/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": true, "connected": true})
		case "/v1/api/tickle", "/v1/api/iserver/auth/reauthenticate":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case "/v1/api/iserver/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []string{"U10000001", "U7654321"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	st, err := c.Status(context.Background())
	if err != nil || !st.Authenticated {
		t.Fatalf("Status = (%+v, %v)", st, err)
	}
	if err := c.Tickle(context.Background()); err != nil {
		t.Fatalf("Tickle: %v", err)
	}
	if err := c.Reauthenticate(context.Background()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	accounts, err := c.Accounts(context.Background())
	if err != nil || len(accounts) != 2 || accounts[0].ID != "U10000001" {
		t.Fatalf("Accounts = (%v, %v)", accounts, err)
	}
	want := []string{
		"POST /v1/api/iserver/auth/status",
		"POST /v1/api/tickle",
		"POST /v1/api/iserver/auth/reauthenticate",
		"GET /v1/api/iserver/accounts",
	}
	for i, w := range want {
		if gotPaths[i] != w {
			t.Errorf("call %d = %q, want %q", i, gotPaths[i], w)
		}
	}
}

func TestClientTradesDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/iserver/account/trades" || r.URL.Query().Get("days") != "7" {
			t.Errorf("unexpected %s %s", r.URL.Path, r.URL.RawQuery)
		}
		// Mixed numeric encodings on purpose: price as string, commission as number.
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"execution_id": "0000e0d5.68762d31.01.01", "symbol": "ES",
			"contract_description_1": "ESM6", "sec_type": "FUT", "conid": 495512563,
			"side": "B", "size": 2, "price": "7222.50", "commission": 4.28,
			"trade_time": "20260504-12:23:31", "trade_time_r": 1777976611000,
			"account": "U10000001", "exchange": "CME",
		}})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	trades, err := c.Trades(context.Background(), 7)
	if err != nil || len(trades) != 1 {
		t.Fatalf("Trades = (%d, %v)", len(trades), err)
	}
	tr := trades[0]
	if tr.ExecutionID != "0000e0d5.68762d31.01.01" || tr.Side != "B" || tr.Size != 2 ||
		float64(tr.Price) != 7222.50 || float64(tr.Commission) != 4.28 ||
		tr.Account != "U10000001" || tr.Conid != 495512563 {
		t.Errorf("decoded = %+v", tr)
	}
}

func TestClient401IsNotAuthenticated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	if _, err := c.Trades(context.Background(), 7); !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("err = %v, want ErrNotAuthenticated", err)
	}
}

func TestClientUnreachable(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"} // nothing listens there
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrGatewayUnreachable) {
		t.Errorf("err = %v, want ErrGatewayUnreachable", err)
	}
}
