package agentsafe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Benchmarks answer "what does the safety cost?". Every consequential transition is one log append: encode,
// hash-chain, optionally seal, then one durable write. The in-memory store isolates the CPU part; the file
// store adds the fsync, which dominates. docs/PERFORMANCE.md has the numbers and how to reproduce them.

// memLines is a LineStore in memory: the CPU cost of a Journal without the cost of durability.
type memLines struct{ lines [][]byte }

func (m *memLines) ReadLines(context.Context) ([][]byte, error) { return m.lines, nil }
func (m *memLines) AppendLine(_ context.Context, seq int, line []byte) error {
	if seq != len(m.lines)+1 {
		return ErrConflict
	}
	m.lines = append(m.lines, bytes.Clone(line))
	return nil
}

// benchEvents is a legal history of n tool calls: run_started, then (model_decided, tool_started,
// tool_result) per call, then the answer and run_finished. About 3n+3 events.
func benchEvents(n int) []Event {
	args := `{"invoice_id":"INV-0001","amount":"4200.50","payee":"Imran Khan","account":"9876543210"}`
	ev := []Event{{Type: EvRunStarted, System: "You pay approved invoices.", Task: "Pay the open invoices.",
		MaxSteps: n + 1, KeyBits: KeyBits, By: "scheduler"}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("call_%d", i)
		ev = append(ev,
			Event{Type: EvModelDecided, Step: i, FinishReason: "tool_calls", Message: &Message{Role: RoleAssistant,
				ToolCalls: []ToolCall{{ID: id, Type: "function", Function: FunctionCall{Name: "pay", Arguments: args}}}}},
			Event{Type: EvToolStarted, CallID: id, Tool: "pay", Args: args, Key: fmt.Sprintf("%032x", i), PayloadHash: "p"},
			Event{Type: EvToolResult, CallID: id, Key: fmt.Sprintf("%032x", i), PayloadHash: "p",
				Result: `{"status":"paid","payment_id":"pay_001"}`})
	}
	return append(ev,
		Event{Type: EvModelDecided, Step: n + 1, FinishReason: "stop", Message: &Message{Role: RoleAssistant, Content: Str("done")}},
		Event{Type: EvRunFinished, Stop: "stop"})
}

func benchCodec() Codec {
	return AESGCM{Keys: map[string][]byte{"k": bytes.Repeat([]byte{7}, 32)}, Current: "k"}
}

func BenchmarkAppend(b *testing.B) {
	ctx := context.Background()
	ev := benchEvents(1)[2] // a tool_started: the write-ahead record before every effect
	for _, c := range []struct {
		name string
		log  func(b *testing.B) Log
	}{
		{"memory", func(*testing.B) Log { return &Journal{Store: &memLines{}} }},
		{"memory+hmac", func(*testing.B) Log { return &Journal{Store: &memLines{}, Key: []byte("k")} }},
		{"memory+sealed", func(*testing.B) Log { return &Journal{Store: &memLines{}, Codec: benchCodec()} }},
		{"file+fsync", func(b *testing.B) Log { return &FileLog{Path: filepath.Join(b.TempDir(), "run.jsonl")} }},
	} {
		b.Run(c.name, func(b *testing.B) {
			log := c.log(b)
			b.ReportAllocs()
			for range b.N {
				if err := log.Append(ctx, ev); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// writeBenchLog writes a chained log of n tool calls to a file, without paying n fsyncs to build it.
func writeBenchLog(b *testing.B, n int, codec Codec) string {
	b.Helper()
	ctx := context.Background()
	store := &memLines{}
	j := &Journal{Store: store, Codec: codec}
	for _, e := range benchEvents(n) {
		e.V = FormatVersion
		if err := j.Append(ctx, e); err != nil {
			b.Fatal(err)
		}
	}
	var buf strings.Builder
	for _, l := range store.lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	path := filepath.Join(b.TempDir(), "run.jsonl")
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkResume is what a process pays to pick up a run: read and verify the log, then rebuild the state.
func BenchmarkResume(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{33, 333, 3333} { // ≈ 100, 1k, 10k events
		for _, sealed := range []bool{false, true} {
			var codec Codec
			name := fmt.Sprintf("events=%d", 3*n+3)
			if sealed {
				codec, name = benchCodec(), name+"/sealed"
			}
			path := writeBenchLog(b, n, codec)
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					events, err := (&FileLog{Path: path, Codec: codec}).Read(ctx)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := Rebuild(events); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkRebuild(b *testing.B) {
	for _, n := range []int{33, 333, 3333} {
		events := benchEvents(n)
		for i := range events {
			events[i].Seq, events[i].V = i+1, FormatVersion
		}
		b.Run(fmt.Sprintf("events=%d", len(events)), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if _, err := Rebuild(events); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCanonical(b *testing.B) {
	v := map[string]any{"invoice_id": "INV-0001", "amount": "4200.50", "payee": map[string]any{"name": "Imran Khan",
		"account": "9876543210", "ifsc": "HDFC0000001"}, "lines": []any{1, 2.5, "x", true, nil}}
	b.ReportAllocs()
	for range b.N {
		if _, err := Canonical(v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunnerStep is the whole loop for one idempotent tool call: model decision (scripted, so free),
// key derivation, three appends (model_decided, tool_started, tool_result) and the state transitions, on an
// in-memory store. Add the durable-write cost from BenchmarkAppend/file+fsync for each append.
func BenchmarkRunnerStep(b *testing.B) {
	ctx := context.Background()
	tool := Func("pay", "pay", func(_ context.Context, in struct {
		InvoiceID string  `json:"invoice_id"`
		Amount    Decimal `json:"amount"`
	}) (string, error) {
		return "paid " + in.InvoiceID, nil
	}, Idempotent("invoice_id"))
	const calls = 50
	plan := make([]FunctionCall, calls)
	for i := range plan {
		plan[i] = FunctionCall{Name: "pay", Arguments: fmt.Sprintf(`{"invoice_id":"INV-%04d","amount":"4200.50"}`, i)}
	}
	b.ReportAllocs()
	for range b.N {
		r, err := New(&ScriptedModel{Plan: plan, Final: "done"}, &Journal{Store: &memLines{}},
			WithTools(tool), WithMaxSteps(calls+1), WithoutLease())
		if err != nil {
			b.Fatal(err)
		}
		if st, err := r.Start(ctx, "sys", "task"); err != nil || st.Status != StatusFinished {
			b.Fatalf("err=%v status=%s", err, st.Status)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*calls)/1000, "µs/call")
}
