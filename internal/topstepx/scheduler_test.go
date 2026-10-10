package topstepx

import (
	"testing"
	"time"
)

// scheduledInterval picks the cadence tier by wall clock: RTH cadence inside
// the RTH window, session cadence through the rest of the Globex window,
// zero when closed or a tier is disabled.
func TestScheduledInterval(t *testing.T) {
	chi, _ := time.LoadLocation("America/Chicago")
	// The queue copies its options at construction, so each tier mix is
	// its own service.
	mk := func(session, rth time.Duration) *SyncService {
		return NewSyncService(nil, nil, nil, nil, SyncOptions{SessionEvery: session, RTHEvery: rth, RTHStartMin: 7*60 + 30, RTHEndMin: 16 * 60}, nil)
	}
	svc := mk(time.Hour, 15*time.Minute)
	ct := func(d, hh, mm int) time.Time {
		return time.Date(2026, 7, d, hh, mm, 0, 0, chi) // 2026-07-13 = Monday
	}
	cases := []struct {
		name string
		t    time.Time
		want time.Duration
	}{
		{"monday RTH -> fast", ct(13, 10, 0), 15 * time.Minute},
		{"monday 1h pre-RTH -> fast", ct(13, 7, 45), 15 * time.Minute},
		{"monday overnight -> hourly", ct(13, 2, 0), time.Hour},
		{"monday evening -> hourly", ct(13, 20, 0), time.Hour},
		{"monday maintenance break -> off", ct(13, 16, 30), 0},
		{"sunday before open -> off", ct(19, 15, 30), 0},
		{"sunday after open -> hourly", ct(19, 17, 30), time.Hour},
		{"saturday -> off", ct(18, 12, 0), 0},
		{"friday after close -> off", ct(17, 17, 0), 0},
	}
	for _, c := range cases {
		if got := svc.scheduledInterval(c.t); got != c.want {
			t.Errorf("%s: scheduledInterval(%v) = %v, want %v", c.name, c.t, got, c.want)
		}
	}

	// Disabling a tier zeroes it out without affecting the other.
	svc = mk(time.Hour, 0)
	if got := svc.scheduledInterval(ct(13, 10, 0)); got != time.Hour {
		t.Errorf("RTH disabled: interval = %v, want fallback to hourly", got)
	}
	svc = mk(0, 0)
	if got := svc.scheduledInterval(ct(13, 2, 0)); got != 0 {
		t.Errorf("both disabled: interval = %v, want 0", got)
	}
}
