package agentsafe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

var k1 = AESGCM{Keys: map[string][]byte{"k1": testKey(1)}, Current: "k1"}

// The account number appears in the model's tool call; the payee and amount in the approval summary.
const sealedArgs = `{"ref":"T1007","amount":11000,"acct":"9876543210"}`

func sealedPayRun(t *testing.T, c Codec) (*Runner, *payTool) {
	t.Helper()
	r, tool := payRun(t, sealedArgs)
	r.Log.(*FileLog).Codec = c
	return r, tool
}

func TestSealedRunNeverWritesContentInTheClear(t *testing.T) {
	r, tool := sealedPayRun(t, k1)
	st, err := r.Start(context.Background(), "you pay people", "pay T1007")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Approve(context.Background(), st.Waiting.Key, "ops@test"); err != nil || tool.paid != 1 {
		t.Fatalf("a sealed run must work like any other: err=%v paid=%d", err, tool.paid)
	}
	raw, err := os.ReadFile(r.Log.(*FileLog).Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"9876543210", "11000", "T1007", "you pay people", "paid"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%q is in the log file in the clear", secret)
		}
	}
	// The audit skeleton stays readable without the key.
	for _, plain := range []string{`"type":"approval_decided"`, `"by":"ops@test"`, `"decision":"approved"`, `"key":"` + st.Waiting.Key} {
		if !bytes.Contains(raw, []byte(plain)) {
			t.Fatalf("%s should stay readable for audit", plain)
		}
	}
	// With the key, the log reads back exactly as the run saw it.
	events, err := r.Log.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Rebuild(events)
	if err != nil || got.Status != StatusFinished {
		t.Fatalf("opened log must rebuild: err=%v status=%s", err, got.Status)
	}
	var seen bool
	for _, m := range got.Messages {
		for _, c := range m.ToolCalls {
			seen = seen || strings.Contains(c.Function.Arguments, "9876543210")
		}
	}
	if !seen {
		t.Fatal("the opened history must hold the real arguments, not a masked copy")
	}
}

func TestSealedRunResumesAfterACrashAndPaysOnce(t *testing.T) {
	r, tool := sealedPayRun(t, k1)
	st, _ := r.Start(context.Background(), "sys", "task")
	r.Hook = func(p string) {
		if p == "after_tool_executed" {
			r.Hook = nil
			panic(crash{})
		}
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = r.Approve(context.Background(), st.Waiting.Key, "ops@test")
	}()
	// A new process: new FileLog, same path, same keys.
	r2 := &Runner{Model: r.Model, Tools: r.Tools, Authorizer: r.Authorizer, Log: &FileLog{Path: r.Log.(*FileLog).Path, Codec: k1}}
	st, err := r2.Continue(context.Background())
	if err != nil || tool.paid != 1 || st.Status != StatusFinished {
		t.Fatalf("resume from a sealed log must not pay twice: err=%v paid=%d status=%s", err, tool.paid, st.Status)
	}
}

func TestSealedLogWithoutTheKeyIsRefusedNotReadAsEmpty(t *testing.T) {
	r, _ := sealedPayRun(t, k1)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	path := r.Log.(*FileLog).Path

	if _, err := (&FileLog{Path: path}).Read(context.Background()); !errors.Is(err, ErrSealed) {
		t.Fatalf("no Codec: want ErrSealed, got %v", err)
	}
	erased := AESGCM{Keys: map[string][]byte{}, Current: "k2"} // k1 deleted: crypto-shredding
	if _, err := (&FileLog{Path: path, Codec: erased}).Read(context.Background()); !errors.Is(err, ErrCannotOpen) {
		t.Fatalf("erased key: want ErrCannotOpen, got %v", err)
	}
	wrong := AESGCM{Keys: map[string][]byte{"k1": testKey(9)}, Current: "k1"}
	if _, err := (&FileLog{Path: path, Codec: wrong}).Read(context.Background()); !errors.Is(err, ErrCannotOpen) {
		t.Fatalf("wrong key: want ErrCannotOpen, got %v", err)
	}
	// Events that were never opened can't be rebuilt, by any route.
	raw, err := (&FileLog{Path: path}).load()
	if err != nil {
		t.Fatalf("the chain must verify without the key: %v", err)
	}
	if _, err := Rebuild(raw); !errors.Is(err, ErrSealed) {
		t.Fatalf("Rebuild of sealed events: want ErrSealed, got %v", err)
	}
}

func TestTamperingWithSealedContentIsDetectedWithoutTheKey(t *testing.T) {
	r, _ := sealedPayRun(t, k1)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	path := r.Log.(*FileLog).Path
	raw, _ := os.ReadFile(path)
	i := bytes.Index(raw, []byte(`"sealed":"`)) + len(`"sealed":"`) + 10
	flipped := append([]byte{}, raw...)
	flipped[i] ^= 1 // still valid base64 in most cases, still valid JSON
	if bytes.Equal(flipped, raw) {
		t.Fatal("setup: the attack didn't change the file")
	}
	if err := os.WriteFile(path, flipped, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileLog{Path: path}).load(); !errors.Is(err, ErrTampered) {
		t.Fatalf("an auditor without the key must still detect the edit, got %v", err)
	}
}

func TestSealedPayloadCantBeMovedToAnotherEvent(t *testing.T) {
	e := Event{Seq: 4, Type: EvToolResult, CallID: "a", Result: `{"paid":true}`}
	s, err := Seal(context.Background(), e, k1)
	if err != nil {
		t.Fatal(err)
	}
	for _, moved := range []Event{{Seq: 5, Type: EvToolResult, CallID: "a"}, {Seq: 4, Type: EvToolResult, CallID: "b"},
		{Seq: 4, Type: EvToolStarted, CallID: "a"}} {
		moved.Sealed = s.Sealed
		if _, err := Open(context.Background(), moved, k1); !errors.Is(err, ErrCannotOpen) {
			t.Fatalf("a sealed payload replayed onto %+v must not open, got %v", moved, err)
		}
	}
}

func TestSealOpenRoundTripsEveryContentField(t *testing.T) {
	e := Event{V: FormatVersion, Seq: 7, Type: EvModelDecided, CallID: "c", Tool: "pay", Key: "k", By: "ops",
		System: "s", Task: "t", Message: &Message{Role: RoleAssistant, Content: Str("hi"),
			ToolCalls: []ToolCall{{ID: "c", Type: "function", Function: FunctionCall{Name: "pay", Arguments: sealedArgs}}}},
		Args: "a", Result: "r", Summary: "{}", Text: "x", Reason: "why", Decision: "approved"}
	s, err := Seal(context.Background(), e, k1)
	if err != nil {
		t.Fatal(err)
	}
	if s.System != "" || s.Message != nil || s.Args != "" || s.Reason != "" || s.Sealed == "" {
		t.Fatalf("content left in the clear: %+v", s)
	}
	if s.Tool != "pay" || s.Key != "k" || s.By != "ops" || s.Decision != "approved" {
		t.Fatal("the audit fields must stay plain")
	}
	o, err := Open(context.Background(), s, k1)
	if err != nil || !reflect.DeepEqual(o, e) {
		t.Fatalf("round trip changed the event:\n got %+v\nwant %+v (err %v)", o, e, err)
	}
	if _, err := Seal(context.Background(), s, k1); err == nil {
		t.Fatal("sealing twice must fail")
	}
	if n, err := Seal(context.Background(), Event{Seq: 1, Type: EvToolStarted, CallID: "c"}, k1); err != nil || n.Sealed != "" {
		t.Fatal("an event with no content needs no seal")
	}
}

func TestKeyRotation(t *testing.T) {
	r, tool := sealedPayRun(t, k1)
	st, _ := r.Start(context.Background(), "sys", "task") // sealed with k1
	path := r.Log.(*FileLog).Path
	rotated := AESGCM{Keys: map[string][]byte{"k1": testKey(1), "k2": testKey(2)}, Current: "k2"}
	r2 := &Runner{Model: r.Model, Tools: r.Tools, Authorizer: r.Authorizer, Log: &FileLog{Path: path, Codec: rotated}}
	if _, err := r2.Approve(context.Background(), st.Waiting.Key, "ops@test"); err != nil || tool.paid != 1 {
		t.Fatalf("a run must continue across a key rotation: err=%v paid=%d", err, tool.paid)
	}
	// Retiring k1 makes only the events it sealed unreadable.
	retired := AESGCM{Keys: map[string][]byte{"k2": testKey(2)}, Current: "k2"}
	if _, err := (&FileLog{Path: path, Codec: retired}).Read(context.Background()); !errors.Is(err, ErrCannotOpen) {
		t.Fatalf("events sealed with a removed key must not open, got %v", err)
	}
}

func TestAESGCMRefusesBadConfiguration(t *testing.T) {
	for name, c := range map[string]AESGCM{
		"no current":  {Keys: map[string][]byte{"k": testKey(1)}},
		"unknown key": {Keys: map[string][]byte{"k": testKey(1)}, Current: "x"},
		"short key":   {Keys: map[string][]byte{"k": testKey(1)[:16]}, Current: "k"},
	} {
		if _, err := c.Seal(context.Background(), []byte("x"), nil); err == nil {
			t.Fatalf("%s: must refuse", name)
		}
	}
	for _, ct := range [][]byte{nil, {5, 'k'}, {1, 'k', 1, 2}} {
		if _, err := k1.Open(context.Background(), ct, nil); err == nil {
			t.Fatalf("truncated ciphertext %v must not open", ct)
		}
	}
}
