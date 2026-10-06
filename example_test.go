package agentsafe_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// The examples run offline: ScriptedModel stands in for a real model (agentsafe/anthropic, agentsafe/gemini,
// or agentsafe/openai), proposing the tool calls a model would.

type Invoice struct {
	InvoiceID string            `json:"invoice_id" desc:"the invoice being paid"`
	Payee     string            `json:"payee"`
	Amount    agentsafe.Decimal `json:"amount" desc:"in rupees, e.g. 4200.50"`
}

// gateway is a stand-in payment system: one payment per idempotency key.
type gateway struct{ payments map[string]string }

func (g *gateway) pay(ctx context.Context, in Invoice) (string, error) {
	key := agentsafe.KeyFrom(ctx) // send this as the provider's Idempotency-Key
	if _, done := g.payments[key]; !done {
		g.payments[key] = fmt.Sprintf("%s %s", in.Payee, in.Amount)
	}
	return "paid", nil
}

func tempLog() (*agentsafe.FileLog, func()) {
	dir, err := os.MkdirTemp("", "agentsafe-example")
	if err != nil {
		panic(err)
	}
	return &agentsafe.FileLog{Path: filepath.Join(dir, "run.jsonl")}, func() { _ = os.RemoveAll(dir) }
}

const payINV1 = `{"invoice_id":"INV-1","payee":"Ramesh","amount":"4200.50"}`

// A payout agent: the payment is idempotent (one per invoice, ever) and waits for a human approval, durably.
func Example() {
	log, cleanup := tempLog()
	defer cleanup()
	gw := &gateway{payments: map[string]string{}}

	pay := agentsafe.Func("send_payout", "Pay an approved invoice", gw.pay,
		agentsafe.Idempotent("invoice_id"),
		agentsafe.NeedsApproval(func(in Invoice) any { return in }))
	model := &agentsafe.ScriptedModel{ // the model proposes the same payout twice
		Plan:  []agentsafe.FunctionCall{{Name: "send_payout", Arguments: payINV1}, {Name: "send_payout", Arguments: payINV1}},
		Final: "INV-1 is paid.",
	}
	r, err := agentsafe.New(model, log, agentsafe.WithTools(pay),
		agentsafe.WithAuthorizer(agentsafe.AllowList("ops@example.com")))
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	st, _ := r.Start(ctx, "You pay approved invoices.", "Pay INV-1.")
	fmt.Println(st.Status, "| approver sees:", st.Waiting.Summary)

	// Hours later, possibly in another process: the decision is addressed by the operation's key.
	st, _ = r.Approve(ctx, st.Waiting.Key, "ops@example.com")
	fmt.Println(st.Status, "|", st.Text, "| payments made:", len(gw.payments))
	// Output:
	// awaiting_approval | approver sees: {"invoice_id":"INV-1","payee":"Ramesh","amount":"4200.50"}
	// finished | INV-1 is paid. | payments made: 1
}

// New checks the whole configuration before any money can move, and reports every problem at once.
func ExampleNew() {
	gw := &gateway{payments: map[string]string{}}
	unsafe := agentsafe.Func("send_payout", "Pay", gw.pay,
		agentsafe.NeedsApproval(func(in Invoice) any { return in })) // approval without Idempotent
	_, err := agentsafe.New(&agentsafe.ScriptedModel{}, &agentsafe.FileLog{Path: "unused.jsonl"}, agentsafe.WithTools(unsafe))
	fmt.Println(errors.Is(err, agentsafe.ErrConfig))
	fmt.Println(strings.Contains(err.Error(), "NeedsApproval requires Idempotent"))
	fmt.Println(strings.Contains(err.Error(), "nobody may give it")) // no Authorizer either
	// Output:
	// true
	// true
	// true
}

// Func derives the JSON schema the model sees from the input struct.
func ExampleFunc() {
	gw := &gateway{payments: map[string]string{}}
	tool := agentsafe.Func("send_payout", "Pay an invoice", gw.pay)
	fmt.Println(string(tool.Spec().Parameters))
	// Output:
	// {"additionalProperties":false,"properties":{"amount":{"description":"in rupees, e.g. 4200.50","pattern":"^-?[0-9]+(\\.[0-9]+)?$","type":"string"},"invoice_id":{"description":"the invoice being paid","type":"string"},"payee":{"type":"string"}},"required":["invoice_id","payee","amount"],"type":"object"}
}

// A run that crashes after the payment but before its result was logged pays once on resume: the same key
// goes back to the gateway, which returns the original payment.
func ExampleRunner_Continue() {
	log, cleanup := tempLog()
	defer cleanup()
	gw := &gateway{payments: map[string]string{}}
	pay := agentsafe.Func("send_payout", "Pay", gw.pay, agentsafe.Idempotent("invoice_id"))
	model := &agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "send_payout", Arguments: payINV1}}, Final: "done"}

	crashing, _ := agentsafe.New(model, log, agentsafe.WithTools(pay), agentsafe.WithHook(func(point string) {
		if point == "after_tool_executed" {
			panic("kill -9") // the payment happened; its result was never logged
		}
	}))
	func() {
		defer func() { _ = recover() }()
		_, _ = crashing.Start(context.Background(), "sys", "Pay INV-1.")
	}()

	// A new process, the same log.
	resumed, _ := agentsafe.New(model, &agentsafe.FileLog{Path: log.Path}, agentsafe.WithTools(pay))
	st, err := resumed.Continue(context.Background())
	fmt.Println(st.Status, err, "| payments made:", len(gw.payments))
	// Output:
	// finished <nil> | payments made: 1
}

// Approvals are authorized: who may decide, and maker-checker.
func ExampleRunner_Approve() {
	log, cleanup := tempLog()
	defer cleanup()
	gw := &gateway{payments: map[string]string{}}
	pay := agentsafe.Func("send_payout", "Pay", gw.pay, agentsafe.Idempotent("invoice_id"),
		agentsafe.NeedsApproval(func(in Invoice) any { return in }))
	r, _ := agentsafe.New(&agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "send_payout", Arguments: payINV1}}, Final: "done"},
		log, agentsafe.WithTools(pay), agentsafe.WithStartedBy("ops@example.com"),
		agentsafe.WithAuthorizer(agentsafe.All(agentsafe.AllowList("ops@example.com", "cfo@example.com"), agentsafe.NotRequester())))

	ctx := context.Background()
	st, _ := r.Start(ctx, "sys", "Pay INV-1.")
	_, err := r.Approve(ctx, st.Waiting.Key, "intern@example.com")
	fmt.Println(errors.Is(err, agentsafe.ErrNotAuthorized), "- not on the list")
	_, err = r.Approve(ctx, st.Waiting.Key, "ops@example.com")
	fmt.Println(errors.Is(err, agentsafe.ErrNotAuthorized), "- started the run, can't approve it")
	st, _ = r.Approve(ctx, st.Waiting.Key, "cfo@example.com")
	fmt.Println(st.Status, "| payments made:", len(gw.payments), "| refused attempts on record:", st.Denials)
	// Output:
	// true - not on the list
	// true - started the run, can't approve it
	// finished | payments made: 1 | refused attempts on record: 2
}

// A sealed log keeps arguments and results encrypted at rest; the audit trail (who approved what) stays
// readable, and resume works with the key.
func ExampleAESGCM() {
	key := bytes.Repeat([]byte{7}, 32) // in production: from a KMS, never next to the log
	log, cleanup := tempLog()
	defer cleanup()
	log.Codec = agentsafe.AESGCM{Keys: map[string][]byte{"2026-10": key}, Current: "2026-10"}

	gw := &gateway{payments: map[string]string{}}
	r, _ := agentsafe.New(&agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "send_payout", Arguments: payINV1}}, Final: "done"},
		log, agentsafe.WithTools(agentsafe.Func("send_payout", "Pay", gw.pay, agentsafe.Idempotent("invoice_id"))))
	_, _ = r.Start(context.Background(), "sys", "Pay INV-1.")

	raw, _ := os.ReadFile(log.Path)
	fmt.Println("amount on disk:", bytes.Contains(raw, []byte("4200.50")))
	_, err := (&agentsafe.FileLog{Path: log.Path}).Read(context.Background())
	fmt.Println("readable without the key:", !errors.Is(err, agentsafe.ErrSealed))
	events, _ := log.Read(context.Background())
	fmt.Println("events with the key:", len(events))
	// Output:
	// amount on disk: false
	// readable without the key: false
	// events with the key: 6
}

// Reconcile checks what the agent did against the systems of record, independently of what it claimed.
func ExampleReconcile() {
	expected := []agentsafe.Effect{{ID: "payment:INV-1", Kind: "payment", Gated: true,
		Fields: map[string]any{"amount": agentsafe.Decimal("4200.50")}}}
	actual := []agentsafe.Effect{ // what the gateway holds: the same invoice paid twice
		{ID: "payment:INV-1", Kind: "payment", Gated: true, Key: "k1", Fields: map[string]any{"amount": 4200.5}},
		{ID: "payment:INV-1", Kind: "payment", Gated: true, Key: "k1", Fields: map[string]any{"amount": 4200.5}},
	}
	events := []agentsafe.Event{
		{Type: agentsafe.EvApprovalDecided, Key: "k1", Decision: "approved", By: "ops"},
		{Type: agentsafe.EvToolResult, Key: "k1", Result: `"paid"`},
	}
	rep := agentsafe.Reconcile(expected, actual, events, nil)
	for _, f := range rep.Findings {
		fmt.Println(f.Severity, f.Check, "-", f.Detail)
	}
	// Output:
	// CRITICAL exactly-once - payment:INV-1 exists 2 times
	// CRITICAL once-per-approval - approved operation k1 took effect 2 times
}

// Same compares field values by type and exact value, as Reconcile does.
func ExampleSame() {
	for _, pair := range [][2]any{
		{agentsafe.Decimal("4200.50"), 4200.5},
		{4200, "4200"},
		{0.3, 0.1 + float64(len("xx"))/10},
	} {
		if eq, why := agentsafe.Same(pair[0], pair[1]); eq {
			fmt.Println("equal")
		} else {
			fmt.Println("different:", why)
		}
	}
	// Output:
	// equal
	// different: "4200" (text) is text, should be number 4200
	// different: 0.30000000000000004, should be 0.3
}

// Redactors mask what leaves the process (console output, exported traces); the log keeps real values.
func ExampleRedactors() {
	r := agentsafe.Redactors(agentsafe.RedactFields("payee", "account"),
		agentsafe.RedactPattern(regexp.MustCompile(`\b\d{9,18}\b`)))
	fmt.Println(r(`{"payee":"Ramesh","amount":"4200.50"}`))
	fmt.Println(r(`account 9876543210 is frozen`))
	// Output:
	// {"amount":"4200.50","payee":"[REDACTED]"}
	// account [REDACTED] is frozen
}

// BuildTrace turns a run's log into OpenTelemetry spans (GenAI semantic conventions).
func ExampleBuildTrace() {
	log, cleanup := tempLog()
	defer cleanup()
	gw := &gateway{payments: map[string]string{}}
	r, _ := agentsafe.New(&agentsafe.ScriptedModel{Plan: []agentsafe.FunctionCall{{Name: "send_payout", Arguments: payINV1}}, Final: "done"},
		log, agentsafe.WithTools(agentsafe.Func("send_payout", "Pay", gw.pay, agentsafe.Idempotent("invoice_id"))))
	_, _ = r.Start(context.Background(), "sys", "Pay INV-1.")

	events, _ := log.Read(context.Background())
	trace, _ := agentsafe.BuildTrace(events, "payouts")
	for _, s := range trace.Spans {
		fmt.Println(s.Name)
	}
	// Output:
	// invoke_agent payouts
	// chat scripted
	// execute_tool send_payout
	// chat scripted
}
