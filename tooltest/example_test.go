package tooltest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/tooltest"
)

type Refund struct {
	OrderID string `json:"order_id"`
}

// sandbox is your real system's test instance; here, a map guarded so "seen this key?" and "record it" are
// one step (a unique constraint on the key, in a database).
type sandbox struct {
	mu      sync.Mutex
	refunds map[string]bool
}

func (s *sandbox) refund(ctx context.Context, _ Refund) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refunds[agentsafe.KeyFrom(ctx)] = true
	return "refunded", nil
}

func (s *sandbox) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refunds)
}

// In a test, call tooltest.SameKey(t, ...); CheckSameKey returns the error instead.
func ExampleCheckSameKey() {
	sb := &sandbox{refunds: map[string]bool{}}
	tool := agentsafe.Func("refund", "Refund an order", sb.refund, agentsafe.Idempotent("order_id"))
	err := tooltest.CheckSameKey(context.Background(), tool.(agentsafe.IdempotentTool),
		json.RawMessage(`{"order_id":"ORD-9"}`), sb.count)
	fmt.Println("safe under concurrent retries:", err == nil)
	// Output:
	// safe under concurrent retries: true
}
