package livedata

import (
	"testing"
	"time"
)

// A subscriber hears its user's bumps and global ones, bursts collapse
// into one signal, other users' bumps are silent, and cancel stops it.
func TestSubscribe(t *testing.T) {
	n := NewNotifier()
	ch, cancel := n.Subscribe(7)
	n.Bump(7)
	n.Bump(7)
	n.Bump(7)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no signal after a bump")
	}
	select {
	case <-ch:
		t.Fatal("a burst must collapse into one signal")
	default:
	}
	n.Bump(8)
	select {
	case <-ch:
		t.Fatal("another user's bump woke this subscriber")
	default:
	}
	n.BumpAll()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no signal after a global bump")
	}
	cancel()
	n.Bump(7)
	select {
	case <-ch:
		t.Fatal("signal after cancel")
	default:
	}
	var none *Notifier
	if c, stop := none.Subscribe(1); c != nil {
		t.Fatal("nil notifier")
	} else {
		stop()
	}
}
