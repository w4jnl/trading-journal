// Package apns sends alerts through the Apple Push Notification service over HTTP/2 with a
// provider token (ES256 JWT from the .p8 key of your developer account). No dependencies: the
// standard library speaks HTTP/2 and signs P-256.
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Config of the provider.
type Config struct {
	Key        []byte // the .p8 file (PKCS#8 PEM) from the developer account
	KeyID      string // its key id (10 characters)
	TeamID     string // the team id
	Topic      string // the app's bundle id
	URL        string // overrides the production service URL (tests)
	SandboxURL string // overrides the sandbox service URL (tests); "" = URL when that is set
	HTTP       *http.Client
}

// Hosts of the service. One key serves both: which one a device needs depends on how its app
// was built, so every Device says it.
const (
	ProductionHost = "https://api.push.apple.com"
	SandboxHost    = "https://api.sandbox.push.apple.com"
)

// Device is where an alert goes: the app's push token and the service that token belongs to.
// A development build installed from Xcode gets its tokens from the sandbox; TestFlight and the
// App Store from production. A token sent to the other service is refused as BadDeviceToken.
type Device struct {
	Token   string
	Sandbox bool
}

// Notification is one alert.
type Notification struct {
	Title         string
	Subtitle      string
	Body          string
	Category      string // the app's notification category (actions)
	ThreadID      string // groups alerts in Notification Center
	CollapseID    string // a later alert with the same id replaces the earlier one
	Badge         *int
	Sound         string // "default", or "" for none
	TimeSensitive bool   // breaks through Focus modes the user allowed it for
	Mutable       bool   // the app's notification service extension may rework it (mutable-content)
	Background    bool   // a silent push that wakes the app (content-available) and shows nothing
	Live          *Live  // a Live Activity push instead of an alert (the device token is the activity's)
	Data          map[string]any
}

// Live is a Live Activity push (apns-push-type liveactivity): start one (to a push-to-start
// token), update or end one (to the activity's own token). Title and Body of the Notification
// are the alert a start needs.
type Live struct {
	Event          string    // start | update | end
	ContentState   any       // the activity's ContentState, as the app's Codable reads it
	AttributesType string    // start: the app's ActivityAttributes type name
	Attributes     any       // start: its attributes
	Stale          time.Time // shown as out of date after; zero = never
	Dismissal      time.Time // end: off the Lock Screen at; zero = the system's default
	Urgent         bool      // priority 10, which iOS budgets; routine changes go at 5
}

// ErrUnregistered is returned for a device token the service no longer knows (the app was
// removed): drop the token.
var ErrUnregistered = errors.New("device token is not registered any more")

// Error is a refusal by the service.
type Error struct {
	Status int
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("apns: %d %s", e.Status, e.Reason) }

// Client sends to one app.
type Client struct {
	cfg     Config
	key     *ecdsa.PrivateKey
	base    string // production
	sandbox string
	http    *http.Client

	mu       sync.Mutex
	token    string
	tokenAt  time.Time
	tokenTTL time.Duration
}

// New parses the key; nil, nil when cfg has no key (push off).
func New(cfg Config) (*Client, error) {
	if len(cfg.Key) == 0 {
		return nil, nil
	}
	if cfg.KeyID == "" || cfg.TeamID == "" || cfg.Topic == "" {
		return nil, errors.New("apns: key_id, team_id and topic are all needed")
	}
	block, _ := pem.Decode(cfg.Key)
	if block == nil {
		return nil, errors.New("apns: the key is not PEM (expected the .p8 file)")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: the key is not an EC (P-256) key")
	}
	base, sandbox := ProductionHost, SandboxHost
	if cfg.URL != "" {
		base, sandbox = cfg.URL, cfg.URL
	}
	if cfg.SandboxURL != "" {
		sandbox = cfg.SandboxURL
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true, Proxy: http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 15 * time.Second, IdleConnTimeout: 10 * time.Minute}}
	}
	return &Client{cfg: cfg, key: ec, base: base, sandbox: sandbox, http: hc, tokenTTL: 50 * time.Minute}, nil
}

// Topic is the app's bundle id.
func (c *Client) Topic() string { return c.cfg.Topic }

// Send delivers one alert to a device, through the service its token belongs to.
func (c *Client) Send(ctx context.Context, d Device, n Notification) error {
	tok, err := c.jwt(time.Now())
	if err != nil {
		return err
	}
	alert := map[string]string{"title": n.Title, "body": n.Body}
	if n.Subtitle != "" {
		alert["subtitle"] = n.Subtitle
	}
	aps := map[string]any{}
	if n.Title != "" || n.Body != "" || n.Subtitle != "" { // none: a badge-only push, which shows nothing
		aps["alert"] = alert
	}
	if n.Background { // Apple: content-available and nothing that shows or sounds
		aps = map[string]any{"content-available": 1}
	}
	sound := n.Sound
	if l := n.Live; l != nil {
		aps = map[string]any{"timestamp": time.Now().Unix(), "event": l.Event, "content-state": l.ContentState}
		if l.Event == "start" {
			aps["attributes-type"], aps["attributes"] = l.AttributesType, l.Attributes
			aps["input-push-token"] = 1 // the new activity's update token, to the app at once (iOS 18)
		}
		if n.Title != "" || n.Body != "" { // an alert lights the activity up; its sound goes inside it
			if sound != "" {
				alert["sound"] = sound
			}
			aps["alert"], sound = alert, ""
		}
		if !l.Stale.IsZero() {
			aps["stale-date"] = l.Stale.Unix()
		}
		if !l.Dismissal.IsZero() {
			aps["dismissal-date"] = l.Dismissal.Unix()
		}
	}
	if sound != "" {
		aps["sound"] = sound
	}
	if n.Badge != nil {
		aps["badge"] = *n.Badge
	}
	if n.Category != "" {
		aps["category"] = n.Category
	}
	if n.ThreadID != "" {
		aps["thread-id"] = n.ThreadID
	}
	if n.TimeSensitive {
		aps["interruption-level"] = "time-sensitive"
	}
	if n.Mutable {
		aps["mutable-content"] = 1
	}
	payload := map[string]any{"aps": aps}
	for k, v := range n.Data {
		if k != "aps" {
			payload[k] = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	base := c.base
	if d.Sandbox {
		base = c.sandbox
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/3/device/"+d.Token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+tok)
	req.Header.Set("apns-topic", c.cfg.Topic)
	switch {
	case n.Live != nil:
		req.Header.Set("apns-push-type", "liveactivity")
		req.Header.Set("apns-topic", c.cfg.Topic+".push-type.liveactivity")
		req.Header.Set("apns-priority", map[bool]string{true: "10", false: "5"}[n.Live.Urgent])
	case n.Background:
		req.Header.Set("apns-push-type", "background")
		req.Header.Set("apns-priority", "5") // background pushes must be 5
	default:
		req.Header.Set("apns-push-type", "alert")
		req.Header.Set("apns-priority", "10")
	}
	req.Header.Set("apns-expiration", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	if n.CollapseID != "" {
		req.Header.Set("apns-collapse-id", trunc(n.CollapseID, 64))
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var r struct {
		Reason string `json:"reason"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(data, &r)
	if resp.StatusCode == http.StatusGone || r.Reason == "Unregistered" || r.Reason == "BadDeviceToken" {
		return ErrUnregistered
	}
	if r.Reason == "" {
		r.Reason = string(bytes.TrimSpace(data))
	}
	return &Error{Status: resp.StatusCode, Reason: r.Reason}
}

// jwt is the provider token, reused for 50 minutes (Apple wants one younger than an hour and
// no more than one refresh per 20 minutes).
func (c *Client) jwt(now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && now.Sub(c.tokenAt) < c.tokenTTL {
		return c.token, nil
	}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(map[string]string{"alg": "ES256", "kid": c.cfg.KeyID}) + "." + enc(map[string]any{"iss": c.cfg.TeamID, "iat": now.Unix()})
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // r || s, each left-padded to 32 bytes (JWS, not ASN.1)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	c.token, c.tokenAt = signing+"."+base64.RawURLEncoding.EncodeToString(sig), now
	return c.token, nil
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
