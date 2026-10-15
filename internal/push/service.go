package push

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/push/apns"
)

// Service keeps the registered phones and sends to them. Nil-safe: a nil
// service (push not configured) registers nothing and sends nothing, and
// the API says so in /me's features.
type Service struct {
	DB   *sql.DB
	Q    *models.Queries
	APNs *apns.Client // nil: devices register, nothing is sent
	Log  *slog.Logger
	Now  func() time.Time
	// Live is the change signal the triggers watch; TZ the trading
	// timezone; Chicago the exchange's (nil loads America/Chicago); Every
	// the tick (DefaultEvery when 0).
	Live    *livedata.Notifier
	TZ      *time.Location
	Chicago *time.Location
	Every   time.Duration
	// State gives the Session activity's content for a user (the handlers,
	// which price positions); nil leaves the activity off.
	State StateFunc

	mu     sync.Mutex
	states map[int64]*tickState
}

func (s *Service) log() *slog.Logger {
	if s != nil && s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Enabled reports whether pushes can be sent at all.
func (s *Service) Enabled() bool { return s != nil && s.APNs != nil }

// Device is a registered phone as the API shows it.
type Device struct {
	Token         string     `json:"token"`
	Name          string     `json:"name"`
	Sandbox       bool       `json:"sandbox"`
	BundleID      string     `json:"bundleId"`
	Prefs         *Prefs     `json:"prefs"`
	ActivityStart bool       `json:"activityStart"` // it gave a push-to-start token
	Activities    []Activity `json:"activities"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastSeenAt    time.Time  `json:"lastSeenAt"`
}

// Activity is one running Live Activity of a phone.
type Activity struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Kind  string `json:"kind"`
}

// Registration is what the app sends when it registers.
type Registration struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Sandbox  bool   `json:"sandbox"`
	BundleID string `json:"bundleId"`
	Prefs    *Prefs `json:"prefs"`
}

// ErrBadInput is a registration the service refuses.
type ErrBadInput struct{ Msg string }

func (e ErrBadInput) Error() string { return e.Msg }

// ErrNotFound: no such device for the user.
var ErrNotFound = errors.New("device not found")

func validToken(tok string) bool {
	if len(tok) < 16 || len(tok) > 512 {
		return false
	}
	for _, c := range tok {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func encodePrefs(p *Prefs) string {
	if p == nil {
		return "{}"
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func decodePrefs(s string) *Prefs {
	var p Prefs
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return &Prefs{}
	}
	return &p
}

// Register stores a phone (or updates its row) and returns it.
func (s *Service) Register(ctx context.Context, userID int64, in Registration) (Device, error) {
	if s == nil {
		return Device{}, errors.New("push is not configured")
	}
	tok := strings.ToLower(strings.TrimSpace(in.Token))
	if !validToken(tok) {
		return Device{}, ErrBadInput{"token must be the APNs device token in hex"}
	}
	if err := in.Prefs.Validate(); err != nil {
		return Device{}, ErrBadInput{err.Error()}
	}
	name := strings.TrimSpace(in.Name)
	if len(name) > 64 {
		name = name[:64]
	}
	sandbox := int64(0)
	if in.Sandbox {
		sandbox = 1
	}
	row, err := s.Q.UpsertDevice(ctx, models.UpsertDeviceParams{UserID: userID, Token: tok, Name: name, Sandbox: sandbox, BundleID: strings.TrimSpace(in.BundleID), Prefs: encodePrefs(in.Prefs)})
	if err != nil {
		return Device{}, fmt.Errorf("register device: %w", err)
	}
	return s.device(ctx, row)
}

func (s *Service) device(ctx context.Context, row models.Device) (Device, error) {
	d := Device{Token: row.Token, Name: row.Name, Sandbox: row.Sandbox != 0, BundleID: row.BundleID, Prefs: decodePrefs(row.Prefs),
		ActivityStart: row.ActivityStartToken != nil && *row.ActivityStartToken != "", Activities: []Activity{}, CreatedAt: row.CreatedAt, LastSeenAt: row.LastSeenAt}
	acts, err := s.Q.ListDeviceActivities(ctx, row.ID)
	if err != nil {
		return d, fmt.Errorf("list activities: %w", err)
	}
	for _, a := range acts {
		d.Activities = append(d.Activities, Activity{ID: a.ActivityID, Token: a.Token, Kind: a.Kind})
	}
	return d, nil
}

// List is the user's phones.
func (s *Service) List(ctx context.Context, userID int64) ([]Device, error) {
	if s == nil {
		return []Device{}, nil
	}
	rows, err := s.Q.ListDevicesForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	out := make([]Device, 0, len(rows))
	for _, r := range rows {
		d, err := s.device(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// SetPrefs replaces a phone's settings.
func (s *Service) SetPrefs(ctx context.Context, userID int64, token string, p *Prefs) (Device, error) {
	if s == nil {
		return Device{}, ErrNotFound
	}
	if err := p.Validate(); err != nil {
		return Device{}, ErrBadInput{err.Error()}
	}
	tok := strings.ToLower(strings.TrimSpace(token))
	n, err := s.Q.UpdateDevicePrefs(ctx, models.UpdateDevicePrefsParams{Prefs: encodePrefs(p), Token: tok, UserID: userID})
	if err != nil {
		return Device{}, fmt.Errorf("update prefs: %w", err)
	}
	if n == 0 {
		return Device{}, ErrNotFound
	}
	row, err := s.Q.GetDeviceByToken(ctx, models.GetDeviceByTokenParams{Token: tok, UserID: userID})
	if err != nil {
		return Device{}, ErrNotFound
	}
	return s.device(ctx, row)
}

// SetActivities replaces a phone's push-to-start token and its running
// activities (the app reports them whole after every change).
func (s *Service) SetActivities(ctx context.Context, userID int64, token string, startToken *string, acts []Activity) (Device, error) {
	if s == nil {
		return Device{}, ErrNotFound
	}
	tok := strings.ToLower(strings.TrimSpace(token))
	row, err := s.Q.GetDeviceByToken(ctx, models.GetDeviceByTokenParams{Token: tok, UserID: userID})
	if err != nil {
		return Device{}, ErrNotFound
	}
	if startToken != nil {
		st := strings.ToLower(strings.TrimSpace(*startToken))
		if st != "" && !validToken(st) {
			return Device{}, ErrBadInput{"startToken must be a push-to-start token in hex"}
		}
		if _, err := s.Q.SetDeviceActivityStart(ctx, models.SetDeviceActivityStartParams{ActivityStartToken: &st, Token: tok, UserID: userID}); err != nil {
			return Device{}, fmt.Errorf("set start token: %w", err)
		}
	}
	if acts != nil {
		if len(acts) > 8 {
			return Device{}, ErrBadInput{"at most 8 activities"}
		}
		if err := s.Q.DeleteDeviceActivities(ctx, row.ID); err != nil {
			return Device{}, fmt.Errorf("clear activities: %w", err)
		}
		for _, a := range acts {
			id, atok := strings.TrimSpace(a.ID), strings.ToLower(strings.TrimSpace(a.Token))
			if id == "" || !validToken(atok) {
				return Device{}, ErrBadInput{"each activity needs an id and its update token in hex"}
			}
			kind := a.Kind
			if kind == "" {
				kind = "session"
			}
			if err := s.Q.InsertDeviceActivity(ctx, models.InsertDeviceActivityParams{DeviceID: row.ID, ActivityID: id, Token: atok, Kind: kind}); err != nil {
				return Device{}, fmt.Errorf("record activity: %w", err)
			}
		}
	}
	_ = s.Q.TouchDevice(ctx, row.ID)
	row, err = s.Q.GetDeviceByToken(ctx, models.GetDeviceByTokenParams{Token: tok, UserID: userID})
	if err != nil {
		return Device{}, ErrNotFound
	}
	return s.device(ctx, row)
}

// Remove forgets a phone; false when it was not the user's.
func (s *Service) Remove(ctx context.Context, userID int64, token string) (bool, error) {
	if s == nil {
		return false, nil
	}
	n, err := s.Q.DeleteDevice(ctx, models.DeleteDeviceParams{Token: strings.ToLower(strings.TrimSpace(token)), UserID: userID})
	if err != nil {
		return false, fmt.Errorf("delete device: %w", err)
	}
	return n > 0, nil
}

// target is a registered phone as the senders see it.
type target struct {
	row   models.Device
	prefs *Prefs
}

func (t target) apns() apns.Device {
	return apns.Device{Token: t.row.Token, Sandbox: t.row.Sandbox != 0}
}

// targets are the user's phones.
func (s *Service) targets(ctx context.Context, userID int64) ([]target, error) {
	rows, err := s.Q.ListDevicesForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]target, 0, len(rows))
	for _, r := range rows {
		out = append(out, target{row: r, prefs: decodePrefs(r.Prefs)})
	}
	return out, nil
}

// send delivers one push to a phone's device token, forgetting the phone
// when Apple no longer knows the token.
func (s *Service) send(ctx context.Context, t target, n apns.Notification, what string) error {
	if !s.Enabled() {
		return nil
	}
	err := s.APNs.Send(ctx, t.apns(), n)
	switch {
	case err == nil:
		s.log().Debug("push sent", "what", what, "device", t.row.Name, "user", t.row.UserID)
	case errors.Is(err, apns.ErrUnregistered):
		s.log().Info("push: device gone, forgetting it", "device", t.row.Name, "user", t.row.UserID)
		_ = s.Q.DeleteDeviceByID(ctx, t.row.ID)
	default:
		s.log().Warn("push failed", "what", what, "device", t.row.Name, "err", err)
	}
	return err
}

// sendActivity delivers a Live Activity push to an activity's own token (or
// a push-to-start token), dropping a refused activity token alone.
func (s *Service) sendActivity(ctx context.Context, t target, token string, n apns.Notification, what string) error {
	if !s.Enabled() {
		return nil
	}
	err := s.APNs.Send(ctx, apns.Device{Token: token, Sandbox: t.row.Sandbox != 0}, n)
	switch {
	case err == nil:
		s.log().Debug("activity push sent", "what", what, "device", t.row.Name)
	case errors.Is(err, apns.ErrUnregistered):
		s.log().Info("activity push: token gone, dropping it", "what", what, "device", t.row.Name)
		_ = s.Q.DeleteDeviceActivityByToken(ctx, token)
		if t.row.ActivityStartToken != nil && *t.row.ActivityStartToken == token {
			empty := ""
			_, _ = s.Q.SetDeviceActivityStart(ctx, models.SetDeviceActivityStartParams{ActivityStartToken: &empty, Token: t.row.Token, UserID: t.row.UserID})
		}
	default:
		s.log().Warn("activity push failed", "what", what, "device", t.row.Name, "err", err)
	}
	return err
}
