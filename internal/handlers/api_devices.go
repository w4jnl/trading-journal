package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/push"
)

// ---- Phones registered for push -----------------------------------------
//
// The paired phone registers its APNs token with its notification settings
// (push.Prefs), updates the settings whole whenever they change, reports
// its push-to-start token and running Live Activities, and forgets itself
// on sign-out. All device-key routes; /me's features.push says whether the
// server can send at all (the registration is kept either way).

func (h *Handlers) pushError(w http.ResponseWriter, err error) {
	var bad push.ErrBadInput
	switch {
	case errors.As(err, &bad):
		writeJSONError(w, http.StatusBadRequest, bad.Msg)
	case errors.Is(err, push.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "device not found")
	default:
		h.logger().Error("api devices", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not update the device")
	}
}

// APIDevicesList: GET /api/v1/devices
func (h *Handlers) APIDevicesList(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	list, err := h.Push.List(r.Context(), userID(r.Context()))
	if err != nil {
		h.pushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "push": h.Push.Enabled()})
}

// APIDeviceRegister: POST /api/v1/devices {token, name, sandbox, bundleId, prefs}
func (h *Handlers) APIDeviceRegister(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	if h.Push == nil {
		writeJSONError(w, http.StatusConflict, "push is not configured on this server")
		return
	}
	var in push.Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d, err := h.Push.Register(r.Context(), userID(r.Context()), in)
	if err != nil {
		h.pushError(w, err)
		return
	}
	h.logger().Info("phone registered for push", "user", userID(r.Context()), "device", d.Name, "sandbox", d.Sandbox)
	writeJSON(w, http.StatusOK, map[string]any{"device": d, "push": h.Push.Enabled()})
}

// APIDevicePrefs: PUT /api/v1/devices/{token}/prefs (the whole Prefs)
func (h *Handlers) APIDevicePrefs(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	var p push.Prefs
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&p); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d, err := h.Push.SetPrefs(r.Context(), userID(r.Context()), r.PathValue("token"), &p)
	if err != nil {
		h.pushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": d})
}

// APIDeviceActivity: PUT /api/v1/devices/{token}/activity {startToken?, activities?}
func (h *Handlers) APIDeviceActivity(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	var in struct {
		StartToken *string         `json:"startToken"`
		Activities []push.Activity `json:"activities"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d, err := h.Push.SetActivities(r.Context(), userID(r.Context()), r.PathValue("token"), in.StartToken, in.Activities)
	if err != nil {
		h.pushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": d})
}

// APIDeviceDelete: DELETE /api/v1/devices/{token}
func (h *Handlers) APIDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	ok, err := h.Push.Remove(r.Context(), userID(r.Context()), r.PathValue("token"))
	if err != nil {
		h.pushError(w, err)
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "device not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
