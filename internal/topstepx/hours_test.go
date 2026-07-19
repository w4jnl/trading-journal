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
