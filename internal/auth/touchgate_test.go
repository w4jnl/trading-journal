package auth

import (
	"testing"
	"time"
)

func TestTouchGate(t *testing.T) {
	var g touchGate
	now := time.Now()
	if !g.allow(1, now) {
		t.Fatal("first touch refused")
	}
	if g.allow(1, now.Add(time.Minute)) {
		t.Fatal("second touch inside the window allowed")
	}
	if !g.allow(2, now) {
		t.Fatal("another id refused")
	}
	if !g.allow(1, now.Add(touchEvery)) {
		t.Fatal("touch after the window refused")
	}
}
