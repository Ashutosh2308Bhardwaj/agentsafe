package heartbeat

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRenewsUntilStopped(t *testing.T) {
	var n atomic.Int32
	h := Start(30*time.Millisecond, func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("each renew must have a deadline")
		}
		n.Add(1)
	})
	time.Sleep(120 * time.Millisecond) // a tick every 10ms
	h.Stop()
	after := n.Load()
	if after < 3 {
		t.Fatalf("want several renewals, got %d", after)
	}
	time.Sleep(50 * time.Millisecond)
	if n.Load() != after {
		t.Fatal("no renew may run after Stop returns")
	}
	h.Stop() // idempotent
}

func TestHoldersAreUnique(t *testing.T) {
	a, err := Holder()
	b, _ := Holder()
	if err != nil || a == b || len(a) != 32 {
		t.Fatalf("got %q %q %v", a, b, err)
	}
}
