// Subscription: an agent adds seats to a customer's subscription through an HTTP API, and every addition is
// billed. The request times out after the seats were added (-slow), or the process is killed right after
// (-crash): either way the call is retried with the same idempotency key, and the customer is billed once.
// -no-key is the control: the same faults, with the key not sent, add the seats and bill them twice.
//
//	go run ./examples/subscription -slow          timeout after the change → retried → one invoice
//	go run ./examples/subscription -crash          killed after the change...
//	go run ./examples/subscription resume          ...a new process finishes the run → one invoice
//	go run ./examples/subscription -no-key -slow   the control: two invoices, 10 seats too many
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// SeatChange is what the model sends. The support ticket is what makes it one operation: "add 5 seats"
// asked for in two tickets is two operations, and a retry of one ticket is not.
type SeatChange struct {
	TicketID   string `json:"ticket_id"`
	CustomerID string `json:"customer_id"`
	Add        int    `json:"add"`
}

func main() {
	slow := flag.Bool("slow", false, "the billing API makes the change but answers after the client has given up (once)")
	crash := flag.Bool("crash", false, "kill this process right after the API made the change, before the result is logged")
	noKey := flag.Bool("no-key", false, "control: the tool doesn't send the idempotency key")
	dir := flag.String("dir", "subscription-demo", "where the run's log and the billing API's books live")
	flag.Parse()
	ctx := context.Background()
	must(os.MkdirAll(*dir, 0o750))
	api, err := startBilling(filepath.Join(*dir, "billing.json"), *slow)
	must(err)
	defer api.Close()

	// Your tool: an HTTP call. KeyFrom(ctx) is the operation's idempotency key; send it, and a retry after a
	// timeout or a crash gets the first response back instead of making a second change.
	addSeats := agentsafe.Func("add_seats", "Add seats to a customer's subscription, as a support ticket asks",
		func(ctx context.Context, c SeatChange) (map[string]any, error) {
			key := agentsafe.KeyFrom(ctx)
			if *noKey {
				key = ""
			}
			return addSeatsRequest(ctx, api.url, c.CustomerID, c.Add, key)
		},
		agentsafe.Idempotent("ticket_id"),       // one operation per ticket
		agentsafe.Timeout(500*time.Millisecond)) // no answer by then: the outcome is unknown, not failed

	model := &agentsafe.ScriptedModel{Final: "Added 5 seats for cus_42 (ticket T-1042).", Plan: []agentsafe.FunctionCall{
		{Name: "add_seats", Arguments: `{"ticket_id":"T-1042","customer_id":"cus_42","add":5}`}}}

	var hook func(string)
	if *crash {
		hook = func(point string) {
			if point == "after_tool_executed" { // the API made the change; the log doesn't know yet
				fmt.Println("killed right after the billing API made the change, before the result was logged")
				kill()
			}
		}
	}
	r, err := agentsafe.New(model, &agentsafe.FileLog{Path: filepath.Join(*dir, "run.jsonl")},
		agentsafe.WithTools(addSeats),
		agentsafe.WithToolRetries(3, 100*time.Millisecond), // an unknown outcome is retried, with the same key
		agentsafe.WithHook(hook),
		agentsafe.WithLogf(func(f string, a ...any) { fmt.Printf(f+"\n", a...) }))
	must(err)

	var st agentsafe.State
	if flag.Arg(0) == "resume" {
		st, err = r.Continue(ctx) // rebuilt from the log: the change in doubt is retried with its key
	} else {
		st, err = r.Start(ctx, "You handle billing support tickets.",
			"Ticket T-1042: customer cus_42 asks for 5 more seats.")
	}
	must(err)
	fmt.Println(st.Status, "-", st.Text)
	must(api.report())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
