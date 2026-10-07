package agentsafe

import (
	"context"
	"path/filepath"
	"testing"
)

// keyRecorder is a gated, idempotent payout that records the key each payment was made with.
type keyRecorder struct{ keys []string }

func (k *keyRecorder) tool() Tool {
	return Func("pay", "pay", func(ctx context.Context, in struct {
		Ref string `json:"ref"`
	}) (string, error) {
		k.keys = append(k.keys, KeyFrom(ctx))
		return "paid " + in.Ref, nil
	}, Idempotent("ref"), NeedsApproval(func(in struct {
		Ref string `json:"ref"`
	}) any {
		return in
	}))
}

// The model proposes the same payout twice: once before the approval, once after it ran.
func keyRun(t *testing.T, path string, k *keyRecorder) *Runner {
	t.Helper()
	plan := []FunctionCall{{Name: "pay", Arguments: `{"ref":"R1"}`}, {Name: "pay", Arguments: `{"ref":"R1"}`}}
	r, err := New(&ScriptedModel{Plan: plan, Final: "done"}, &FileLog{Path: path},
		WithTools(k.tool()), WithMaxSteps(8), WithAnyApprover())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewRunsUse128BitKeys(t *testing.T) {
	k := &keyRecorder{}
	st, err := keyRun(t, filepath.Join(t.TempDir(), "run.jsonl"), k).Start(context.Background(), "sys", "pay R1")
	if err != nil || st.Status != StatusAwaitingApproval {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	if st.KeyBits != KeyBits || len(st.Waiting.Key) != KeyBits/4 {
		t.Fatalf("a new run must use %d-bit keys: key_bits=%d key=%q", KeyBits, st.KeyBits, st.Waiting.Key)
	}
}

// A run paused for approval under v0.2 (log format v3, 64-bit keys) and approved after the upgrade must
// finish on its original keys. With a recomputed longer key, the payout would run under a key the provider
// has never seen, and the repeated proposal would not be found in the log: two payments.
func TestRunStartedBeforeV4KeepsItsKeysAcrossAnUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	k := &keyRecorder{}
	if _, err := keyRun(t, filepath.Join(dir, "new.jsonl"), k).Start(ctx, "sys", "pay R1"); err != nil {
		t.Fatal(err)
	}
	events, err := (&FileLog{Path: filepath.Join(dir, "new.jsonl")}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old := writeAsV3(ctx, t, events, filepath.Join(dir, "old.jsonl"))

	r := keyRun(t, old.Path, k)
	oldEvents, err := old.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := Rebuild(oldEvents)
	if err != nil || before.KeyBits != 64 || len(before.Waiting.Key) != 16 {
		t.Fatalf("a v3 run must read as 64-bit: err=%v key_bits=%d key=%q", err, before.KeyBits, before.Waiting.Key)
	}
	st, err := r.Approve(ctx, before.Waiting.Key, "ops")
	if err != nil || st.Status != StatusFinished {
		t.Fatalf("approving after the upgrade: err=%v status=%s", err, st.Status)
	}
	if len(k.keys) != 1 || k.keys[0] != before.Waiting.Key {
		t.Fatalf("one payment, under the key that was approved (%s); got %v", before.Waiting.Key, k.keys)
	}
	after, err := old.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertKeyLength(t, after, 16)
}

// writeAsV3 writes a history the way v0.2 did: format v3, no key_bits, 16-character keys (a 64-bit key is
// the first 16 characters of the 128-bit one).
func writeAsV3(ctx context.Context, t *testing.T, events []Event, path string) *FileLog {
	t.Helper()
	old := &FileLog{Path: path}
	for _, e := range events {
		e.V, e.KeyBits = 3, 0
		if e.Key != "" {
			e.Key = e.Key[:16]
		}
		if e.PayloadHash != "" {
			e.PayloadHash = e.PayloadHash[:16]
		}
		if err := old.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	return old
}

// assertKeyLength checks that every key in a run's log has the run's length.
func assertKeyLength(t *testing.T, events []Event, chars int) {
	t.Helper()
	for _, e := range events {
		if e.Key != "" && len(e.Key) != chars {
			t.Fatalf("event %d (%s) has a %d-character key in a run of %d-character keys", e.Seq, e.Type, len(e.Key), chars)
		}
	}
}

func TestRunStartedNeedsAKeyLength(t *testing.T) {
	for _, bits := range []int{0, 32, 100, 512} {
		_, err := Rebuild([]Event{{V: FormatVersion, Type: EvRunStarted, Task: "t", MaxSteps: 1, KeyBits: bits}})
		if err == nil {
			t.Errorf("a v%d run_started with key_bits %d must be refused", FormatVersion, bits)
		}
	}
}
