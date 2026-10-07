// Quickstart: an agent that can pay an invoice at most once, and only after a human approves.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Payout is what the model sends to pay one invoice.
type Payout struct {
	InvoiceID string            `json:"invoice_id"`
	Amount    agentsafe.Decimal `json:"amount"`
}

// sendPayout is your code. KeyFrom(ctx) is the operation's idempotency key: give it to your payment
// provider, and a retry after a crash or a timeout can't pay twice.
func sendPayout(ctx context.Context, p Payout) (string, error) {
	_ = agentsafe.KeyFrom(ctx)
	fmt.Println("paying", p.InvoiceID, p.Amount)
	return "paid", nil
}

func main() {
	ctx := context.Background()
	pay := agentsafe.Func("send_payout", "Pay an approved invoice", sendPayout,
		agentsafe.Idempotent("invoice_id"),                       // once per invoice, ever
		agentsafe.NeedsApproval(func(p Payout) any { return p })) // a human decides first

	// An offline stand-in for the model. For Claude: anthropic.New(sdk.NewClient()).
	model := &agentsafe.ScriptedModel{Final: "INV-1 paid.", Plan: []agentsafe.FunctionCall{
		{Name: "send_payout", Arguments: `{"invoice_id":"INV-1","amount":"4200.50"}`}}}

	r, err := agentsafe.New(model, &agentsafe.FileLog{Path: "payout-run.jsonl"},
		agentsafe.WithTools(pay), agentsafe.WithAuthorizer(agentsafe.AllowList("you@example.com")))
	must(err)

	if len(os.Args) > 1 && os.Args[1] == "approve" {
		st, err := r.Continue(ctx) // rebuilt from the log: a new process knows what's waiting
		must(err)
		if st.Waiting == nil {
			fmt.Println("nothing to approve; the run is", st.Status)
			return
		}
		st, err = r.Approve(ctx, st.Waiting.Key, "you@example.com")
		must(err)
		fmt.Println(st.Status, "-", st.Text)
		return
	}
	st, err := r.Start(ctx, "You pay approved invoices.", "Pay invoice INV-1.")
	must(err)
	fmt.Printf("%s: %s\napprove it, now or after a reboot: go run . approve\n", st.Status, st.Waiting.Summary)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
