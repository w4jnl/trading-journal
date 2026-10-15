package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/push"
)

// The device routes need a device key; registration, settings, activities
// and removal round-trip; /me reports push off without an APNs key.
func TestAPIDevices(t *testing.T) {
	fx := newPositionFixture(t)
	fx.h.Push = &push.Service{DB: fx.h.DB, Q: fx.q}
	device := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindDevice})
	agent := auth.WithAPIKey(fx.ctx, auth.APIKeyInfo{Kind: auth.KeyKindAgent})
	do := func(ctx context.Context, fn http.HandlerFunc, method, token, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/api/v1/devices", strings.NewReader(body)).WithContext(ctx)
		if token != "" {
			req.SetPathValue("token", token)
		}
		rec := httptest.NewRecorder()
		fn(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	tok := "0123456789abcdef0123456789abcdef"
	if code, _ := do(agent, fx.h.APIDeviceRegister, http.MethodPost, "", `{"token": "`+tok+`"}`); code != http.StatusForbidden {
		t.Errorf("agent register = %d", code)
	}
	code, out := do(device, fx.h.APIDeviceRegister, http.MethodPost, "", `{"token": "`+tok+`", "name": "Phone", "sandbox": true, "prefs": {"kinds": {"nudge": {"sound": "default"}}}}`)
	if code != http.StatusOK || out["push"] != false || out["device"].(map[string]any)["name"] != "Phone" {
		t.Fatalf("register = %d %v", code, out)
	}
	if code, out := do(device, fx.h.APIDeviceRegister, http.MethodPost, "", `{"token": "zz"}`); code != http.StatusBadRequest {
		t.Errorf("bad token = %d %v", code, out)
	}
	code, out = do(device, fx.h.APIDevicePrefs, http.MethodPut, tok, `{"noActivity": true}`)
	if code != http.StatusOK || out["device"].(map[string]any)["prefs"].(map[string]any)["noActivity"] != true {
		t.Errorf("prefs = %d %v", code, out)
	}
	if code, _ := do(device, fx.h.APIDevicePrefs, http.MethodPut, "ffffffffffffffffffffffffffffffff", `{}`); code != http.StatusNotFound {
		t.Errorf("unknown device prefs = %d", code)
	}
	code, out = do(device, fx.h.APIDeviceActivity, http.MethodPut, tok, `{"startToken": "abcdefabcdefabcdefabcdefabcdefab", "activities": [{"id": "A", "token": "abcdefabcdefabcdefabcdefabcdefab"}]}`)
	if code != http.StatusOK || out["device"].(map[string]any)["activityStart"] != true || len(out["device"].(map[string]any)["activities"].([]any)) != 1 {
		t.Errorf("activity = %d %v", code, out)
	}
	code, out = do(device, fx.h.APIDevicesList, http.MethodGet, "", "")
	if code != http.StatusOK || len(out["items"].([]any)) != 1 {
		t.Errorf("list = %d %v", code, out)
	}
	if code, out := apiGet(device, fx.h.APIMe, "/api/v1/me"); code != http.StatusOK || out["server"].(map[string]any)["features"].(map[string]any)["push"] != false {
		t.Errorf("me = %d %v", code, out)
	}
	if code, _ := do(device, fx.h.APIDeviceDelete, http.MethodDelete, tok, ""); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if code, _ := do(device, fx.h.APIDeviceDelete, http.MethodDelete, tok, ""); code != http.StatusNotFound {
		t.Errorf("second delete = %d", code)
	}
}
