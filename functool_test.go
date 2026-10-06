package agentsafe_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/tooltest"
)

type Payout struct {
	InvoiceID string            `json:"invoice_id" desc:"the invoice being paid"`
	Payee     string            `json:"payee"`
	Amount    agentsafe.Decimal `json:"amount" desc:"in rupees"`
	Method    string            `json:"method" enum:"neft,imps,upi"`
	Note      string            `json:"note,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	DueBy     *time.Time        `json:"due_by"`
}

type Receipt struct {
	PaymentID string `json:"payment_id"`
	Key       string `json:"key"`
}

// bank is a sandbox payment system with idempotency keys: the check and the record are one step (a mutex
// here; a unique constraint on the key in a real one).
type bank struct {
	mu     sync.Mutex
	byKey  map[string]Receipt
	racy   bool // the bug tooltest must catch: check, then act, without holding anything between
	ledger map[string]string
}

func newBank() *bank {
	return &bank{byKey: map[string]Receipt{}, ledger: map[string]string{"INV-1": "4200.50"}}
}

func (b *bank) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.byKey)
}

func (b *bank) pay(ctx context.Context, _ Payout) (Receipt, error) {
	key := agentsafe.KeyFrom(ctx)
	if b.racy {
		b.mu.Lock()
		r, seen := b.byKey[key]
		b.mu.Unlock()
		if seen {
			return r, nil
		}
		time.Sleep(time.Millisecond) // the gap between check and act
		b.mu.Lock()
		defer b.mu.Unlock()
		r = Receipt{PaymentID: fmt.Sprintf("pay_%d", len(b.byKey)+1), Key: key}
		b.byKey[key+fmt.Sprint(len(b.byKey))] = r // a second payment for the same key
		return r, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, seen := b.byKey[key]; seen {
		return r, nil
	}
	r := Receipt{PaymentID: fmt.Sprintf("pay_%d", len(b.byKey)+1), Key: key}
	b.byKey[key] = r
	return r, nil
}

func (b *bank) check(_ context.Context, p Payout) error {
	if want, ok := b.ledger[p.InvoiceID]; !ok || agentsafe.Decimal(want) != p.Amount {
		return fmt.Errorf("the ledger says %s is %s, not %s", p.InvoiceID, want, p.Amount)
	}
	return nil
}

const goodArgs = `{"invoice_id":"INV-1","payee":"Ramesh","amount":"4200.50","method":"neft","due_by":null}`

func TestFuncSchemaIsGeneratedFromTheInputStruct(t *testing.T) {
	tool := agentsafe.Func("send_payout", "Pay an invoice", newBank().pay)
	var s struct {
		Type                 string                    `json:"type"`
		Properties           map[string]map[string]any `json:"properties"`
		Required             []string                  `json:"required"`
		AdditionalProperties bool                      `json:"additionalProperties"`
	}
	if err := json.Unmarshal(tool.Spec().Parameters, &s); err != nil {
		t.Fatal(err)
	}
	if s.Type != "object" || s.AdditionalProperties || strings.Join(s.Required, ",") != "invoice_id,payee,amount,method" {
		t.Fatalf("required = non-pointer fields without omitempty; no extra properties: %+v", s)
	}
	for field, want := range map[string]string{
		"invoice_id": `{"description":"the invoice being paid","type":"string"}`,
		"amount":     `{"description":"in rupees","pattern":"^-?[0-9]+(\\.[0-9]+)?$","type":"string"}`,
		"method":     `{"enum":["neft","imps","upi"],"type":"string"}`,
		"tags":       `{"items":{"type":"string"},"type":"array"}`,
		"due_by":     `{"format":"date-time","type":"string"}`,
	} {
		got, _ := json.Marshal(s.Properties[field])
		if string(got) != want {
			t.Errorf("%s: got %s, want %s", field, got, want)
		}
	}
}

func run(t *testing.T, tool agentsafe.Tool, opts []agentsafe.Option, calls ...string) *agentsafe.Runner {
	t.Helper()
	var plan []agentsafe.FunctionCall
	for _, c := range calls {
		plan = append(plan, agentsafe.FunctionCall{Name: tool.Spec().Name, Arguments: c})
	}
	r, err := agentsafe.New(&agentsafe.ScriptedModel{Plan: plan, Final: "done"},
		&agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "run.jsonl")},
		append([]agentsafe.Option{agentsafe.WithTools(tool), agentsafe.WithMaxSteps(8)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func results(t *testing.T, r *agentsafe.Runner) []agentsafe.Event {
	t.Helper()
	events, err := r.Log.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []agentsafe.Event
	for _, e := range events {
		if e.Type == agentsafe.EvToolResult || e.Type == agentsafe.EvToolRefused {
			out = append(out, e)
		}
	}
	return out
}

func TestFuncIdempotentPaysOncePerInvoiceAndPassesTheKey(t *testing.T) {
	b := newBank()
	pay := agentsafe.Func("send_payout", "Pay an invoice", b.pay, agentsafe.Idempotent("invoice_id"))
	again := strings.Replace(goodArgs, `"neft"`, `"neft","note":"retry"`, 1) // same invoice, different payload
	r := run(t, pay, nil, goodArgs, goodArgs, again)
	if _, err := r.Start(context.Background(), "sys", "pay INV-1"); err != nil {
		t.Fatal(err)
	}
	res := results(t, r)
	if b.count() != 1 || len(res) != 3 {
		t.Fatalf("three proposals of one invoice must pay once: payments=%d results=%d", b.count(), len(res))
	}
	var rc Receipt
	if err := json.Unmarshal([]byte(res[0].Result), &rc); err != nil || rc.Key != res[0].Key || rc.Key == "" {
		t.Fatalf("the function must get the logged key via KeyFrom: receipt %+v, logged key %q", rc, res[0].Key)
	}
	if !res[1].Replayed || !strings.Contains(res[2].Result, "conflict") {
		t.Fatalf("a repeat is replayed; the same invoice with different values is a conflict: %s / %s", res[1].Result, res[2].Result)
	}
}

func TestFuncRefusesUnknownFieldsBeforeRunning(t *testing.T) {
	b := newBank()
	pay := agentsafe.Func("send_payout", "Pay an invoice", b.pay, agentsafe.Idempotent("invoice_id"))
	r := run(t, pay, nil, strings.Replace(goodArgs, `"payee"`, `"payee_account":"999","payee"`, 1))
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	res := results(t, r)
	if b.count() != 0 || res[0].Type != agentsafe.EvToolRefused || !strings.Contains(res[0].Result, `unknown field \"payee_account\"`) {
		t.Fatalf("an argument the schema doesn't have must be refused, nothing paid: %+v", res)
	}
}

func TestFuncApprovalAndCheckEndToEnd(t *testing.T) {
	b := newBank()
	pay := agentsafe.Func("send_payout", "Pay an invoice", b.pay, agentsafe.Idempotent("invoice_id"),
		agentsafe.Check(b.check),
		agentsafe.NeedsApproval(func(p Payout) any { return map[string]any{"payee": p.Payee, "amount": p.Amount} }),
		agentsafe.Timeout(time.Second))
	wrong := strings.Replace(goodArgs, "4200.50", "4300", 1)
	r := run(t, pay, []agentsafe.Option{agentsafe.WithAuthorizer(agentsafe.AllowList("ops"))}, wrong, goodArgs)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != agentsafe.StatusAwaitingApproval {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	if res := results(t, r); len(res) != 1 || !strings.Contains(res[0].Result, "the ledger says INV-1 is 4200.50") {
		t.Fatalf("the wrong amount must be refused by Check before anyone is asked: %+v", res)
	}
	if st.Waiting.Summary != `{"amount":"4200.50","payee":"Ramesh"}` || b.count() != 0 {
		t.Fatalf("the approver sees the checked values; nothing paid yet: %s", st.Waiting.Summary)
	}
	if st, err = r.Approve(context.Background(), st.Waiting.Key, "ops"); err != nil || st.Status != agentsafe.StatusFinished || b.count() != 1 {
		t.Fatalf("err=%v status=%s payments=%d", err, st.Status, b.count())
	}
}

func TestFuncApprovalIfOnlyAboveAThreshold(t *testing.T) {
	b := newBank()
	b.ledger["INV-2"] = "90"
	pay := agentsafe.Func("send_payout", "Pay", b.pay, agentsafe.Idempotent("invoice_id"),
		agentsafe.ApprovalIf(func(p Payout) bool { return p.Amount != "90" }, func(p Payout) any { return p }))
	small := strings.Replace(strings.Replace(goodArgs, "INV-1", "INV-2", 1), "4200.50", "90", 1)
	r := run(t, pay, []agentsafe.Option{agentsafe.WithAnyApprover()}, small, goodArgs)
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != agentsafe.StatusAwaitingApproval || b.count() != 1 {
		t.Fatalf("the small payout runs, the large one waits: err=%v status=%s payments=%d", err, st.Status, b.count())
	}
}

func TestFuncConfigurationMistakesStopNew(t *testing.T) {
	b := newBank()
	type Other struct{ X int }
	type Loop struct{ Next *Loop }
	_, err := agentsafe.New(&agentsafe.ScriptedModel{}, &agentsafe.FileLog{Path: filepath.Join(t.TempDir(), "r")}, agentsafe.WithAnyApprover(),
		agentsafe.WithTools(
			agentsafe.Func("a", "", b.pay, agentsafe.NeedsApproval(func(p Payout) any { return p })),
			agentsafe.Func("b", "", b.pay, agentsafe.Idempotent("invoice")),
			agentsafe.Func("c", "", b.pay, agentsafe.Check(func(context.Context, Other) error { return nil })),
			agentsafe.Func("d", "", func(context.Context, string) (int, error) { return 0, nil }),
			agentsafe.Func("e", "", func(context.Context, Loop) (int, error) { return 0, nil }),
			agentsafe.Func("f", "", b.pay, agentsafe.Timeout(0)),
		))
	if !errors.Is(err, agentsafe.ErrConfig) {
		t.Fatalf("want ErrConfig, got %v", err)
	}
	for _, want := range []string{"tool a: NeedsApproval requires Idempotent", `tool b: Idempotent: input has no field "invoice"`,
		"tool c: option Check is for input type agentsafe_test.Other", "must be a struct", "recursive type", "tool f: Timeout must be positive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestFuncTimeoutApplies(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	slow := agentsafe.Func("export", "", func(context.Context, struct{}) (string, error) { <-stuck; return "", nil },
		agentsafe.Timeout(20*time.Millisecond))
	r := run(t, slow, nil, `{}`)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	if res := results(t, r); !strings.Contains(res[0].Result, "did not return within 20ms") {
		t.Fatalf("got %s", res[0].Result)
	}
}

func TestToolTestPassesASafeToolAndCatchesARacyOne(t *testing.T) {
	good := newBank()
	tooltest.SameKey(t, agentsafe.Func("send_payout", "", good.pay, agentsafe.Idempotent("invoice_id")).(agentsafe.IdempotentTool),
		json.RawMessage(goodArgs), good.count)

	racy := newBank()
	racy.racy = true
	err := tooltest.CheckSameKey(context.Background(),
		agentsafe.Func("send_payout", "", racy.pay, agentsafe.Idempotent("invoice_id")).(agentsafe.IdempotentTool),
		json.RawMessage(goodArgs), racy.count)
	if err == nil || !strings.Contains(err.Error(), "check-then-act") {
		t.Fatalf("a check-then-act tool must be caught, got %v", err)
	}

	stuck := agentsafe.Func("send_payout", "", func(context.Context, Payout) (int, error) { return 1, nil },
		agentsafe.Idempotent("invoice_id")).(agentsafe.IdempotentTool)
	if err := tooltest.CheckSameKey(context.Background(), stuck, json.RawMessage(goodArgs), func() int { return 0 }); err == nil ||
		!strings.Contains(err.Error(), "counting the right thing") {
		t.Fatalf("an effects counter that never moves must be flagged, not pass trivially: %v", err)
	}
}

func TestFuncWithoutItsOwnTimeoutGetsTheRunnersDefault(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	slow := agentsafe.Func("export", "", func(context.Context, struct{}) (string, error) { <-stuck; return "", nil })
	r := run(t, slow, []agentsafe.Option{agentsafe.WithToolTimeout(20 * time.Millisecond)}, `{}`)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	if res := results(t, r); !strings.Contains(res[0].Result, "did not return within 20ms") {
		t.Fatalf("a Func with no Timeout option must use the Runner's ToolTimeout, got %s", res[0].Result)
	}
}
