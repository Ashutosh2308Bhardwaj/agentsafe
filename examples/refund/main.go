// Refund: a support agent refunds an overcharge through a Stripe-shaped payments API. The refund is checked
// against the real charge before anyone is asked, waits for finance to approve it, and is sent once, even if
// the process is killed right after the money moved.
//
//	go run ./examples/refund                                      the agent proposes; the run waits
//	go run ./examples/refund approve intern@example.com           refused: not on the approver list
//	go run ./examples/refund approve support@example.com          refused: support started this run
//	go run ./examples/refund approve finance@example.com -crash   approved, refunded, killed before logging
//	go run ./examples/refund resume                               finishes: one refund
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// RefundRequest is what the model sends. The ticket and the charge are what make it one operation; the
// amount is its payload, so the same ticket refunded with a different amount is a conflict, not a second
// refund.
type RefundRequest struct {
	TicketID string            `json:"ticket_id"`
	ChargeID string            `json:"charge_id"`
	Amount   agentsafe.Decimal `json:"amount"`
}

func main() { os.Exit(run()) }

// run is the program; it returns the exit code, so deferred cleanup always runs.
func run() int {
	crash := flag.Bool("crash", false, "kill this process right after the API refunded, before the result is logged")
	noKey := flag.Bool("no-key", false, "control: the refund tool doesn't send the idempotency key")
	dir := flag.String("dir", "refund-demo", "where the run's log and the payments API's books live")
	flag.Parse()
	ctx := context.Background()
	must(os.MkdirAll(*dir, 0o750))
	api, err := startPayments(filepath.Join(*dir, "payments.json"))
	must(err)
	defer api.Close()

	lookup := agentsafe.Func("lookup_charge", "Look a charge up: amount, customer, how much is already refunded",
		func(ctx context.Context, in struct {
			ChargeID string `json:"charge_id"`
		}) (*Charge, error) {
			return fetchCharge(ctx, api.url, in.ChargeID)
		})

	refund := agentsafe.Func("create_refund", "Refund part or all of a charge, as a support ticket asks",
		func(ctx context.Context, in RefundRequest) (*Refund, error) {
			key := agentsafe.KeyFrom(ctx)
			if *noKey {
				key = ""
			}
			return createRefund(ctx, api.url, in.ChargeID, string(in.Amount), key)
		},
		agentsafe.Idempotent("ticket_id", "charge_id"), // one refund per ticket and charge
		agentsafe.Check(func(ctx context.Context, in RefundRequest) error {
			return refundable(ctx, api.url, in) // grounded in the real charge, before anyone is asked
		}),
		agentsafe.ApprovalIf(func(in RefundRequest) bool { // a human decides anything over 50.00
			c, _ := cents(string(in.Amount))
			return c > 5000
		}, func(in RefundRequest) any { return in }),
		agentsafe.Timeout(2*time.Second))

	// The model's first attempt slips a decimal place (12000.00); the check refuses it, and it corrects.
	model := &agentsafe.ScriptedModel{Final: "Refunded the 1200.00 overcharge on ch_3QA (ticket T-77).",
		Plan: []agentsafe.FunctionCall{
			{Name: "lookup_charge", Arguments: `{"charge_id":"ch_3QA"}`},
			{Name: "create_refund", Arguments: `{"ticket_id":"T-77","charge_id":"ch_3QA","amount":"12000.00"}`},
			{Name: "create_refund", Arguments: `{"ticket_id":"T-77","charge_id":"ch_3QA","amount":"1200.00"}`}}}

	var hook func(string)
	if *crash {
		hook = func(point string) {
			if point == "after_tool_executed" { // the money moved; the log doesn't know yet
				fmt.Println("killed right after the payments API refunded, before the result was logged")
				kill()
			}
		}
	}
	r, err := agentsafe.New(model, &agentsafe.FileLog{Path: filepath.Join(*dir, "run.jsonl")},
		agentsafe.WithTools(lookup, refund),
		// Finance and the support lead may approve refunds, but nobody approves a run they started (maker-checker).
		agentsafe.WithStartedBy("support@example.com"),
		agentsafe.WithAuthorizer(agentsafe.All(
			agentsafe.AllowList("finance@example.com", "support@example.com"), agentsafe.NotRequester())),
		agentsafe.WithHook(hook),
		agentsafe.WithLogf(func(f string, a ...any) { fmt.Printf(f+"\n", a...) }))
	must(err)

	var st agentsafe.State
	switch flag.Arg(0) {
	case "approve":
		st, err = approve(ctx, r, flag.Arg(1))
	case "resume":
		st, err = r.Continue(ctx) // rebuilt from the log: a refund in doubt is retried with its key
	default:
		st, err = r.Start(ctx, "You handle payment support tickets. Check a charge before refunding it.",
			"Ticket T-77: customer cus_42 was overcharged 1200.00 on charge ch_3QA. Refund the overcharge.")
	}
	if errors.Is(err, agentsafe.ErrNotAuthorized) {
		fmt.Println("refused:", err)
		must(api.report())
		return 1
	}
	must(err)
	if st.Status == agentsafe.StatusAwaitingApproval {
		fmt.Printf("waiting for finance to approve: %s\n", st.Waiting.Summary)
	} else {
		fmt.Println(st.Status, "-", st.Text)
	}
	must(api.report())
	return 0
}

// approve decides the refund the run is waiting on, as who. The run is read from the log: any process can.
func approve(ctx context.Context, r *agentsafe.Runner, who string) (agentsafe.State, error) {
	st, err := r.Continue(ctx)
	if err != nil || st.Waiting == nil {
		return st, err
	}
	return r.Approve(ctx, st.Waiting.Key, who)
}

// refundable is the grounding check: the charge exists and the amount fits in what's left of it.
func refundable(ctx context.Context, baseURL string, in RefundRequest) error {
	c, err := fetchCharge(ctx, baseURL, in.ChargeID)
	if err != nil {
		return err
	}
	amt, ok := cents(string(in.Amount))
	paid, _ := cents(c.Amount)
	done, _ := cents(c.Refunded)
	switch {
	case !ok || amt <= 0:
		return fmt.Errorf("amount %q is not a positive amount", in.Amount)
	case amt > paid-done:
		return fmt.Errorf("a refund of %s exceeds the %s left on %s (charged %s, refunded %s)",
			in.Amount, money(paid-done), c.ID, c.Amount, c.Refunded)
	}
	return nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
