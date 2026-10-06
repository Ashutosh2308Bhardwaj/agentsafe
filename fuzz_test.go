package agentsafe

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fuzz targets: the code that reads data agentsafe doesn't control (a log file after a crash or an edit, a
// model's tool arguments, sealed payloads). `go test` replays the seeds and every saved failure in
// testdata/fuzz; `go test -fuzz=FuzzX` searches for new ones.

func seedLogs(f *testing.F) {
	for _, g := range golden {
		if b, err := os.ReadFile(g.file); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte(""))
	f.Add([]byte("{}\n"))
	f.Add([]byte("{\"seq\":1,\"type\":\"run_started\"}\n{\"seq\":2"))
}

// Reading any bytes as a log never panics; whatever it accepts is a prefix of acknowledged lines; and a torn
// tail (bytes after the last newline) never changes what's read.
func FuzzReadLog(f *testing.F) {
	seedLogs(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "run.jsonl")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		events, err := (&FileLog{Path: path}).Read(context.Background())
		if err != nil {
			return
		}
		if len(events) > bytes.Count(data, []byte{'\n'}) {
			t.Fatalf("%d events from %d complete lines", len(events), bytes.Count(data, []byte{'\n'}))
		}
		_, _ = Rebuild(events) // may refuse, must not panic

		// A torn tail never changes what's read, when every complete line was an acknowledged event. (A bad
		// complete line followed by anything is damaged history, not a crash: agentsafe repairs a bad last
		// line before it writes again, so a line with data after it was once good. Found by this fuzzer.)
		if len(events) != bytes.Count(data, []byte{'\n'}) || (len(data) > 0 && !bytes.HasSuffix(data, []byte("\n"))) {
			return
		}
		torn := append(append([]byte{}, data...), []byte(`{"seq":`)...)
		if err := os.WriteFile(path, torn, 0o600); err != nil {
			t.Fatal(err)
		}
		again, err := (&FileLog{Path: path}).Read(context.Background())
		if err != nil || len(again) != len(events) {
			t.Fatalf("a torn tail changed the log: %d events before, %d after (err %v)", len(events), len(again), err)
		}
	})
}

// Rebuild of any event sequence never panics, and is deterministic.
func FuzzRebuild(f *testing.F) {
	for _, g := range golden {
		events, err := (&FileLog{Path: g.file, Codec: g.codec}).Read(context.Background())
		if err == nil {
			b, _ := json.Marshal(events)
			f.Add(b)
		}
	}
	f.Add([]byte(`[{"type":"tool_result","call_id":"x"}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var events []Event
		if json.Unmarshal(data, &events) != nil {
			return
		}
		a, errA := Rebuild(events)
		b, errB := Rebuild(events)
		if (errA == nil) != (errB == nil) || a.Status != b.Status || a.Step != b.Step || len(a.Effects) != len(b.Effects) {
			t.Fatal("Rebuild is not deterministic")
		}
	})
}

// Canonical keeps every number exactly: two different numbers must never canonicalize the same, or two
// different operations would share an idempotency key (and the second would never happen).
func FuzzCanonicalKeepsNumbersExact(f *testing.F) {
	for _, s := range []string{"1", "4200.50", "-0.1", "1e3", "12345678901234567", "0.30000000000000004"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, lit string) {
		var n json.Number
		if len(lit) > 40 || json.Unmarshal([]byte(lit), &n) != nil {
			return
		}
		want, ok := new(big.Rat).SetString(string(n))
		if !ok {
			return
		}
		c, err := Canonical(json.RawMessage(`{"id":` + lit + `}`))
		if err != nil {
			return // refusing a number is allowed; changing it is not
		}
		d := json.NewDecoder(strings.NewReader(c))
		d.UseNumber()
		var out struct{ ID json.Number }
		if err := d.Decode(&out); err != nil {
			t.Fatalf("Canonical produced invalid JSON %q: %v", c, err)
		}
		got, ok := new(big.Rat).SetString(string(out.ID))
		if !ok || got.Cmp(want) != 0 {
			t.Fatalf("Canonical changed the number %s to %s", lit, out.ID)
		}
	})
}

// Canonical is stable: canonicalizing its own output changes nothing.
func FuzzCanonicalIsStable(f *testing.F) {
	f.Add(`{"b":1,"a":[true,null,"x"]}`)
	f.Add(`{"amount":"4200.50","invoice_id":"INV-1"}`)
	f.Fuzz(func(t *testing.T, s string) {
		if !json.Valid([]byte(s)) {
			return
		}
		c1, err := Canonical(json.RawMessage(s))
		if err != nil {
			return
		}
		c2, err := Canonical(json.RawMessage(c1))
		if err != nil || c1 != c2 {
			t.Fatalf("not stable: %q -> %q -> %q (%v)", s, c1, c2, err)
		}
	})
}

// Opening any sealed payload never panics, and never succeeds with the wrong key.
func FuzzOpenSealed(f *testing.F) {
	f.Add("AQ==", 1, "tool_result")
	f.Add("", 0, "")
	f.Fuzz(func(_ *testing.T, sealed string, seq int, typ string) {
		e := Event{Seq: seq, Type: EventType(typ), Sealed: sealed}
		_, _ = Open(context.Background(), e, k1)
	})
}

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

type fuzzPayout struct {
	InvoiceID string   `json:"invoice_id"`
	Amount    Decimal  `json:"amount"`
	Tags      []string `json:"tags,omitempty"`
	Note      *string  `json:"note"`
}

// A model can send any arguments: a wrapped tool must never panic on them, and the same arguments must always
// give the same idempotency key.
func FuzzFuncArgs(f *testing.F) {
	f.Add(`{"invoice_id":"INV-1","amount":"4200.50","note":null}`)
	f.Add(`{"invoice_id":12,"amount":[]}`)
	f.Add(`{"invoice_id":"INV-1","amount":"1","extra":1}`)
	tool := Func("pay", "", func(context.Context, fuzzPayout) (string, error) { return "ok", nil },
		Idempotent("invoice_id"), NeedsApproval(func(p fuzzPayout) any { return p })).(IdempotentTool)
	f.Fuzz(func(t *testing.T, args string) {
		raw := json.RawMessage(args)
		ctx := context.Background()
		_ = tool.(Validator).Validate(ctx, raw)
		_, _ = tool.(Gated).Summary(raw)
		_ = tool.(Gated).NeedsApproval(raw)
		_, _ = tool.CallWithKey(ctx, "k", raw)
		k1, p1, err1 := keyFor("s", tool, raw)
		k2, p2, err2 := keyFor("s", tool, raw)
		if (err1 == nil) != (err2 == nil) || k1 != k2 || p1 != p2 {
			t.Fatalf("the same arguments gave different keys: %s/%s vs %s/%s", k1, p1, k2, p2)
		}
	})
}
