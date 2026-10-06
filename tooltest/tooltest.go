// Package tooltest checks your own tools against the guarantees agentsafe relies on. Run it against a sandbox
// of the real system (a test gateway, a scratch table), not a mock: the point is to catch the real system's
// races.
//
//	func TestPayoutIsIdempotent(t *testing.T) {
//		tool := agentsafe.Func("send_payout", "...", sendPayout, agentsafe.Idempotent("invoice_id"))
//		tooltest.SameKey(t, tool.(agentsafe.IdempotentTool), json.RawMessage(`{"invoice_id":"INV-1",...}`),
//			func() int { return sandbox.PaymentCount() })
//	}
package tooltest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Concurrency is how many simultaneous calls SameKey makes with one key.
const Concurrency = 20

// SameKey fails t unless the tool performs its effect at most once per key, however the calls arrive: one
// after another (a retry after a crash) or at the same moment (a retry after a timeout while the first call
// is still running), and every caller gets the same outcome.
//
// effects counts the real effects in the system the tool acts on (payments made, rows written). args must be
// valid arguments for one operation.
func SameKey(t testing.TB, tool agentsafe.IdempotentTool, args json.RawMessage, effects func() int) {
	t.Helper()
	if err := CheckSameKey(context.Background(), tool, args, effects); err != nil {
		t.Fatal(err)
	}
}

// CheckSameKey is SameKey returning an error instead of failing a test.
func CheckSameKey(ctx context.Context, tool agentsafe.IdempotentTool, args json.RawMessage, effects func() int) error {
	name := tool.Spec().Name
	// 1. The counter must see effects at all, or "one effect per key" would pass by counting nothing.
	before := effects()
	if _, err := tool.CallWithKey(ctx, newKey(), args); err != nil {
		return fmt.Errorf("%s: a first call failed, so nothing can be checked: %w", name, err)
	}
	if effects() != before+1 {
		return fmt.Errorf("%s: one call with a new key made %d effects (want 1): is effects() counting the right thing?",
			name, effects()-before)
	}

	// 2. Simultaneous calls with one key: one effect, one outcome.
	key := newKey()
	before = effects()
	results := make([]string, Concurrency)
	errs := make([]error, Concurrency)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together
			out, err := tool.CallWithKey(ctx, key, args)
			errs[i] = err
			if err == nil {
				results[i], errs[i] = agentsafe.Canonical(out)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := effects() - before; n != 1 {
		return fmt.Errorf("%s: %d simultaneous calls with ONE key made %d effects (want 1): the check for the key "+
			"and the effect aren't atomic (check-then-act). Use a unique constraint on the key, or a lock around both",
			name, Concurrency, n)
	}
	for i := range Concurrency {
		if errs[i] != nil {
			return fmt.Errorf("%s: call %d of %d with one key failed while another succeeded (%v): a caller that gets "+
				"an error records a failure for an operation that happened", name, i+1, Concurrency, errs[i])
		}
		if results[i] != results[0] {
			return fmt.Errorf("%s: calls with one key returned different outcomes:\n  %s\n  %s", name, results[0], results[i])
		}
	}

	// 3. A later retry with the same key: still one effect, the same outcome.
	out, err := tool.CallWithKey(ctx, key, args)
	if err != nil {
		return fmt.Errorf("%s: a retry with a used key failed: %w", name, err)
	}
	if n := effects() - before; n != 1 {
		return fmt.Errorf("%s: a retry with a used key made another effect (%d in total)", name, n)
	}
	if c, _ := agentsafe.Canonical(out); c != results[0] {
		return fmt.Errorf("%s: a retry returned a different outcome than the original:\n  %s\n  %s", name, results[0], c)
	}
	return nil
}

func newKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "tooltest-" + hex.EncodeToString(b)
}
