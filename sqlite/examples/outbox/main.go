// Outbox: an agent issues store credit by writing to your own database. The credit note, its idempotency key
// and the email announcing it commit in one transaction, so a crash at any point leaves either all of them or
// none, and a retry finds the key instead of writing a second credit. A worker delivers the email from the
// outbox, retrying until it's sent. A reconciliation checks the result against the source data.
//
//	go run ./examples/outbox -crash        the credit commits; the process is killed before the log knows
//	go run ./examples/outbox resume        the retry finds the key: still one credit note
//	go run ./examples/outbox worker -crash the email is accepted; the worker is killed before marking it sent
//	go run ./examples/outbox worker        re-sent with the same key: still one email
//	go run ./examples/outbox check         reconcile the database against the source data and the log
//
// Run it from the sqlite module (cd sqlite). -no-key is the control: the same crashes write two credits
// and send two emails.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/reconcile"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/sqlite"
)

// IssueCredit is what the model sends. One credit per late order: the order is the operation's identity.
type IssueCredit struct {
	OrderID  string            `json:"order_id"`
	Customer string            `json:"customer"`
	Amount   agentsafe.Decimal `json:"amount"`
}

// The source data: order 1001 (cus_42, 850.00) arrived 9 days late, and policy credits 10% of a late order.
var lateOrders = []IssueCredit{{OrderID: "1001", Customer: "cus_42", Amount: "85.00"}}

func main() { os.Exit(run()) }

func run() int {
	crash := flag.Bool("crash", false, "kill the process right after it commits, before the log or the outbox moves on")
	noKey := flag.Bool("no-key", false, "control: write without the idempotency key, and send without one")
	dir := flag.String("dir", "outbox-demo", "where the database and the email API's records live")
	flag.Parse()
	ctx := context.Background()
	must(os.MkdirAll(*dir, 0o750))
	db, err := openDB(filepath.Join(*dir, "shop.db"))
	must(err)
	defer func() { _ = db.Close() }()
	mail, err := startNotifier(filepath.Join(*dir, "emails.json"))
	must(err)
	defer mail.Close()
	logs, err := sqlite.New(db) // the agent's log lives in the same database as the business tables
	must(err)
	// A lease is a row with an expiry, renewed while its holder runs. A killed process can't release it, so
	// the run is blocked until it expires: that's what stops a process that only *seemed* dead (paused, cut
	// off) from coming back and acting twice. 2s for the demo; the default is 30s.
	logs.LeaseTTL = leaseTTL
	log := &agentsafe.Journal{Store: logs.Run("late-order-credits")}

	var exit int
	switch flag.Arg(0) {
	case "worker":
		must(deliver(ctx, db, mail.url, *noKey, *crash))
	case "check":
		exit = check(ctx, db, log)
	default:
		must(agent(ctx, db, log, flag.Arg(0) == "resume", *noKey, *crash))
	}
	must(report(ctx, db, mail))
	return exit
}

// agent runs (or resumes) the run that issues the credit.
func agent(ctx context.Context, db *sql.DB, log agentsafe.Log, resume, noKey, crash bool) error {
	issue := agentsafe.Func("issue_credit", "Issue store credit to a customer for a late order",
		func(ctx context.Context, in IssueCredit) (CreditNote, error) {
			key := agentsafe.KeyFrom(ctx)
			if noKey {
				key = ""
			}
			c, replayed, err := issueCredit(ctx, db, key, in.OrderID, in.Customer, string(in.Amount))
			if replayed {
				fmt.Printf("    the database already has this operation's key: credit note %d, nothing written\n", c.ID)
			}
			return c, err
		},
		agentsafe.Idempotent("order_id")) // one credit per order

	model := &agentsafe.ScriptedModel{Final: "Issued 85.00 store credit to cus_42 for late order 1001.",
		Plan: []agentsafe.FunctionCall{{Name: "issue_credit", Arguments: `{"order_id":"1001","customer":"cus_42","amount":"85.00"}`}}}
	r, err := agentsafe.New(model, log, agentsafe.WithTools(issue),
		agentsafe.WithHook(killAt("after_tool_executed", crash, "killed right after the credit committed, before the log recorded it")),
		agentsafe.WithLogf(func(f string, a ...any) { fmt.Printf(f+"\n", a...) }))
	if err != nil {
		return err
	}
	var st agentsafe.State
	if resume {
		st, err = continueWhenFree(ctx, r)
	} else {
		st, err = r.Start(ctx, "You handle late-delivery complaints by policy.", "Order 1001 for cus_42 (850.00) arrived 9 days late. Apply the policy: 10% store credit.")
	}
	if err != nil {
		return err
	}
	fmt.Println(st.Status, "-", st.Text)
	return nil
}

// continueWhenFree resumes the run, waiting out the lease of a process that died holding it.
func continueWhenFree(ctx context.Context, r *agentsafe.Runner) (agentsafe.State, error) {
	deadline := time.Now().Add(2 * leaseTTL)
	for {
		st, err := r.Continue(ctx)
		if !errors.Is(err, agentsafe.ErrRunLocked) || time.Now().After(deadline) {
			return st, err
		}
		fmt.Println("    the run is still leased by the killed process; waiting for the lease to expire")
		time.Sleep(leaseTTL / 4)
	}
}

const leaseTTL = 2 * time.Second

// deliver is the outbox worker: it sends every pending message, then marks it sent. A crash between the two
// means the message goes again on the next run, with the same key, so the email API sends it once.
func deliver(ctx context.Context, db *sql.DB, url string, noKey, crash bool) error {
	pending, err := pendingOutbox(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range pending {
		key := outboxKey(m.ID)
		if noKey {
			key = ""
		}
		if err := sendMessage(ctx, url, m.Payload, key); err != nil {
			return err // left pending: the next run retries it
		}
		killAt("sent", crash, "killed right after the email API accepted the message, before it was marked sent")("sent")
		if err := markSent(ctx, db, m.ID); err != nil {
			return err
		}
	}
	fmt.Printf("worker: %d message(s) delivered\n", len(pending))
	return nil
}

// check reconciles the database against the source data and the agent's log, independently of the agent.
func check(ctx context.Context, db *sql.DB, log agentsafe.Log) int {
	expected := make([]reconcile.Effect, len(lateOrders))
	for i, o := range lateOrders {
		expected[i] = reconcile.Effect{ID: "credit:" + o.OrderID, Kind: "credit",
			Fields: map[string]any{"customer": o.Customer, "amount": o.Amount}}
	}
	notes, keys, err := creditNotes(ctx, db)
	must(err)
	actual := make([]reconcile.Effect, len(notes))
	for i, c := range notes {
		actual[i] = reconcile.Effect{ID: "credit:" + c.OrderID, Kind: "credit", Key: keys[i],
			Fields: map[string]any{"customer": c.Customer, "amount": agentsafe.Decimal(c.Amount)}}
	}
	events, err := log.Read(ctx)
	must(err)
	rep := reconcile.Audit(expected, actual, events, nil)
	fmt.Print(rep.String())
	if !rep.Pass() {
		return 1
	}
	return 0
}

// report prints what the systems of record say: the database and the email API.
func report(ctx context.Context, db *sql.DB, mail *notifier) error {
	notes, _, err := creditNotes(ctx, db)
	if err != nil {
		return err
	}
	sent, err := mail.count()
	if err != nil {
		return err
	}
	fmt.Printf("database: %d credit note(s); email API: %d email(s)\n", len(notes), sent)
	for _, c := range notes {
		fmt.Printf("  credit note %d  order %s  %s  %s\n", c.ID, c.OrderID, c.Customer, c.Amount)
	}
	return nil
}

// killAt returns a hook that kills the process at point, if enabled.
func killAt(point string, enabled bool, why string) func(string) {
	return func(p string) {
		if enabled && p == point {
			fmt.Println(why)
			kill()
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
