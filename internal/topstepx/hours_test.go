package topstepx

import (
	"testing"
	"time"
)

func TestInGlobexHours(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	ct := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, chi)
	}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"saturday closed", ct(2026, 7, 18, 12, 0), false},
		{"sunday before open", ct(2026, 7, 19, 16, 59), false},
		{"sunday at open", ct(2026, 7, 19, 17, 0), true},
		{"friday before close", ct(2026, 7, 17, 15, 59), true},
		{"friday at close", ct(2026, 7, 17, 16, 0), false},
		{"tuesday maintenance break", ct(2026, 7, 14, 16, 30), false},
		{"tuesday after break", ct(2026, 7, 14, 17, 0), true},
		{"tuesday regular", ct(2026, 7, 14, 9, 30), true},
		{"monday midnight", ct(2026, 7, 13, 0, 30), true},
		// DST transitions: the break stays at 16:00-17:00 Chicago wall clock.
		{"spring-forward day break", ct(2026, 3, 8, 16, 30), false},
		{"fall-back day open", ct(2026, 11, 1, 17, 30), true},
	}
	for _, c := range cases {
		if got := InGlobexHours(c.t, chi); got != c.want {
			t.Errorf("%s: InGlobexHours(%v) = %v, want %v", c.name, c.t, got, c.want)
		}
	}
	// A UTC instant must evaluate identically (conversion inside the func).
	utc := time.Date(2026, 7, 14, 21, 30, 0, 0, time.UTC) // = 16:30 CDT, break
	if InGlobexHours(utc, chi) {
		t.Error("UTC 21:30 during CDT break should be closed")
	}
}

// InRTHWindow: Mon-Fri within the configured minutes-of-day, Chicago.
// Bounds here are the default 07:30-16:00 (RTH 08:30-15:00 +/- 1h).
func TestInRTHWindow(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	const start, end = 7*60 + 30, 16 * 60
	ct := func(d, hh, mm int) time.Time {
		return time.Date(2026, 7, d, hh, mm, 0, 0, chi) // 2026-07-13 = Monday
	}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"monday before window", ct(13, 7, 29), false},
		{"monday window opens", ct(13, 7, 30), true},
		{"monday mid RTH", ct(13, 12, 0), true},
		{"monday last minute", ct(13, 15, 59), true},
		{"monday window closes", ct(13, 16, 0), false},
		{"friday mid RTH", ct(17, 10, 0), true},
		{"saturday off", ct(18, 10, 0), false},
		{"sunday off", ct(19, 10, 0), false},
	}
	for _, c := range cases {
		if got := InRTHWindow(c.t, chi, start, end); got != c.want {
			t.Errorf("%s: InRTHWindow(%v) = %v, want %v", c.name, c.t, got, c.want)
		}
	}
}

// InSyncWindow = Globex session +/- 2h: effectively Sunday 15:00 through
// Friday 18:00 Chicago, Saturday off, with the daily maintenance break
// swallowed by the margin.
func TestInSyncWindow(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	ct := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, chi)
	}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"saturday still off", ct(2026, 7, 18, 12, 0), false},
		{"sunday 2h before open", ct(2026, 7, 19, 15, 0), true},
		{"sunday just before margin", ct(2026, 7, 19, 14, 59), false},
		{"friday 2h after close", ct(2026, 7, 17, 17, 59), true},
		{"friday past margin", ct(2026, 7, 17, 18, 0), false},
		{"friday late evening off", ct(2026, 7, 17, 21, 0), false},
		{"tuesday maintenance break covered", ct(2026, 7, 14, 16, 30), true},
		{"tuesday regular", ct(2026, 7, 14, 9, 30), true},
		{"regular session unchanged", ct(2026, 7, 13, 10, 0), true},
	}
	for _, c := range cases {
		if got := InSyncWindow(c.t, chi); got != c.want {
			t.Errorf("%s: InSyncWindow(%v) = %v, want %v", c.name, c.t, got, c.want)
		}
	}
}
