package reconcile

import (
	"encoding/json"
	"testing"
)

// Same is reflexive and symmetric on anything decoded from JSON.
func FuzzSame(f *testing.F) {
	f.Add(`4200.5`, `"4200.50"`)
	f.Add(`{"a":[1,2]}`, `{"a":[1,2.0]}`)
	f.Fuzz(func(t *testing.T, a, b string) {
		var x, y any
		if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
			return
		}
		if eq, why := Same(x, x); !eq {
			t.Fatalf("Same(x, x) is false for %s: %s", a, why)
		}
		if e1, _ := Same(x, y); e1 != func() bool { e2, _ := Same(y, x); return e2 }() {
			t.Fatalf("Same isn't symmetric for %s, %s", a, b)
		}
	})
}
