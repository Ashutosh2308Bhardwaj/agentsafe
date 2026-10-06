// Package heartbeat keeps a database lease alive: the part the sqlite and postgres backends share. Each
// backend keeps what is its own (the SQL that takes, renews and releases a lease row).
//
// It is internal: importable by the modules in this repository (sqlite, postgres), not by anyone else.
package heartbeat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Holder returns a random identity for one lease acquisition. Renewing and releasing touch only rows that
// carry it, so a holder that lost its lease can't extend or free the new holder's.
func Holder() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Heartbeat calls renew every ttl/3 until Stop.
type Heartbeat struct {
	stop chan struct{}
	done sync.WaitGroup
	once sync.Once
}

// Start begins renewing. Each renew gets a context that expires after ttl/3, so a hung database can't stall
// the heartbeat past the next tick. Renewal lives as long as the lease (until Stop), not as long as the call
// that took it: tied to that call's context, the lease would silently stop renewing when the call returned.
func Start(ttl time.Duration, renew func(ctx context.Context)) *Heartbeat {
	h := &Heartbeat{stop: make(chan struct{})}
	h.done.Add(1)
	go func() {
		defer h.done.Done()
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), ttl/3)
				renew(ctx)
				cancel()
			}
		}
	}()
	return h
}

// Stop ends renewal and waits until no renew is running. It doesn't release the lease (a crash doesn't
// either); the backend does that. Calling it again is a no-op.
func (h *Heartbeat) Stop() {
	h.once.Do(func() { close(h.stop) })
	h.done.Wait()
}
