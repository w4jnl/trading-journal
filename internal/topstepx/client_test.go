package topstepx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientLoginKey(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/Auth/loginKey" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errorCode": 0, "token": "tok-1"})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	tok, err := c.LoginKey(context.Background(), "trader", "key-123")
	if err != nil || tok != "tok-1" {
		t.Fatalf("LoginKey = (%q, %v), want (tok-1, nil)", tok, err)
	}
	if gotBody["userName"] != "trader" || gotBody["apiKey"] != "key-123" {
		t.Errorf("body = %v", gotBody)
	}
}

func TestClientLoginKeyErrorCodes(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{3, ErrAuth},          // InvalidCredentials
		{1, ErrAuth},          // UserNotFound
		{9, ErrSubscription},  // ApiSubscriptionNotFound
		{10, ErrSubscription}, // ApiKeyAuthenticationDisabled
	}
	for _, cse := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errorCode": cse.code, "errorMessage": "nope"})
		}))
		c := &Client{BaseURL: srv.URL}
		_, err := c.LoginKey(context.Background(), "u", "k")
		if !errors.Is(err, cse.want) {
			t.Errorf("errorCode %d: err = %v, want %v", cse.code, err, cse.want)
		}
		srv.Close()
	}
}

func TestClientBearerAndTimestamps(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errorCode": 0, "trades": []any{}})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	start := time.Date(2026, 7, 1, 10, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	end := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	if _, err := c.SearchTrades(context.Background(), "tok-9", 42, start, end); err != nil {
		t.Fatalf("SearchTrades: %v", err)
	}
	if gotAuth != "Bearer tok-9" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody["startTimestamp"] != "2026-07-01T08:30:00Z" { // converted to UTC
		t.Errorf("startTimestamp = %v", gotBody["startTimestamp"])
	}
	if gotBody["accountId"] != float64(42) {
		t.Errorf("accountId = %v", gotBody["accountId"])
	}
}

func TestClient401IsErrAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	if _, err := c.SearchAccounts(context.Background(), "stale", true); !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
}

func TestClientValidate(t *testing.T) {
	rolled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rolled {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errorCode": 0, "newToken": "tok-2"})
			return
		}
		// success with empty newToken: current token still good
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errorCode": 0})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	tok, err := c.Validate(context.Background(), "tok-1")
	if err != nil || tok != "tok-1" {
		t.Fatalf("Validate keep = (%q, %v), want (tok-1, nil)", tok, err)
	}
	rolled = true
	tok, err = c.Validate(context.Background(), "tok-1")
	if err != nil || tok != "tok-2" {
		t.Fatalf("Validate roll = (%q, %v), want (tok-2, nil)", tok, err)
	}
}

func TestClientValidateFailureIsErrAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errorCode": 1, "errorMessage": "expired"})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	if _, err := c.Validate(context.Background(), "old"); !errors.Is(err, ErrAuth) {
		t.Errorf("err = %v, want ErrAuth", err)
	}
}

func TestClientSearchTradesDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "errorCode": 0,
			"trades": []map[string]any{{
				"id": 9001, "accountId": 42, "contractId": "CON.F.US.MES.M26",
				"creationTimestamp": "2026-07-01T14:00:00+00:00", "price": 7300.25,
				"profitAndLoss": nil, "fees": 0.74, "commissions": 0.35,
				"side": 1, "size": 2, "voided": false, "orderId": 555,
			}},
		})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	trades, err := c.SearchTrades(context.Background(), "tok", 42, time.Now().Add(-time.Hour), time.Now())
	if err != nil || len(trades) != 1 {
		t.Fatalf("SearchTrades = (%d, %v)", len(trades), err)
	}
	ht := trades[0]
	if ht.ID != 9001 || ht.OrderID != 555 || ht.Side != 1 || ht.Size != 2 ||
		ht.ProfitAndLoss != nil || ht.Commissions == nil || *ht.Commissions != 0.35 {
		t.Errorf("decoded = %+v", ht)
	}
}
