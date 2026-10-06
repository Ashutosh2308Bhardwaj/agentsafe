// Command reconcile is the week-1 reconciliation agent, ported to agentsafe: reconcile ledger.csv (payouts
// we sent) against bank.csv (the bank's settlement file), recording each discrepancy once.
//
//	go run ./examples/reconcile                 one run on Groq (GROQ_API_KEY)
//	go run ./examples/reconcile -mock           the scripted model: free, deterministic
//	go run ./examples/reconcile -run R -resume  continue run R from its log after a crash
//	go run ./examples/reconcile -run R -extend 3  give a PAUSED run 3 more steps (logged, with who)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

const system = "You reconcile our payout ledger against the bank's settlement file.\n" +
	"Use the tools to read both files and compare them. For every discrepancy you find, call " +
	"record_discrepancy exactly once. Then, for every payout that is in our ledger but missing from the bank " +
	"file, re-send it with send_payout (a human approves it before money moves). When you're done, reply with a " +
	"short report whose final line is exactly: TOTAL_DISCREPANCIES: <n>"

var kinds = []string{"amount_mismatch", "missing_in_bank", "missing_in_ledger", "duplicate_in_bank", "duplicate_in_ledger"}

func main() {
	mock := flag.Bool("mock", false, "use the scripted model (free)")
	runID := flag.String("run", "", "run id (default: timestamp)")
	resume := flag.Bool("resume", false, "continue an existing run from its log")
	extend := flag.Int("extend", 0, "give a paused run this many more steps")
	maxSteps := flag.Int("steps", 12, "budget for a new run")
	approve := flag.String("approve", "", "approve the pending operation with this key")
	reject := flag.String("reject", "", "reject the pending operation with this key")
	reason := flag.String("reason", "", "why (for -reject)")
	checkOnly := flag.Bool("check", false, "only reconcile an existing run against the systems of record")
	flag.Parse()

	here := "examples/reconcile"
	if *runID == "" {
		*runID = "g" + time.Now().Format("150405")
	}
	out := filepath.Join(here, "out")
	must(os.MkdirAll(out, 0o755))
	ledger := &Ledger{Path: filepath.Join(out, *runID+"-discrepancies.jsonl")}

	var model agentsafe.Model
	if *mock || *checkOnly { // -check reads records only; it never calls a model
		model = scripted(here)
	} else {
		key := os.Getenv("GROQ_API_KEY")
		if key == "" {
			die(errors.New("GROQ_API_KEY not set (or use -mock)"))
		}
		model = &agentsafe.OpenAICompatible{BaseURL: "https://api.groq.com/openai/v1", APIKey: key,
			Model: "openai/gpt-oss-120b", Logf: logf}
	}

	gw := gatewayAt(out, *runID)
	if n, err := strconv.Atoi(os.Getenv("GATEWAY_LOSE_RESPONSES")); err == nil {
		gw.LoseResponses = n // the gateway charges, then the response is lost (F13 at the money layer)
	}
	gw.OnCharged = func() { killHookFn("gateway_charged") }
	// Silent faults (week 4 S3): each makes a system of record wrong while every report says success.
	fault := os.Getenv("SILENT_FAULT")
	gw.WrongAmount = fault == "gateway_wrong_amount"
	gw.IgnoreKey = fault == "gateway_ignores_key"
	ledger.DropNext = fault == "ledger_drops_write" && !*resume && *approve == "" && !*checkOnly
	var write agentsafe.Tool = &RecordDiscrepancy{Ledger: ledger, Dir: filepath.Join(here, "data")}
	if os.Getenv("NO_IDEMPOTENCY") == "1" { // control experiment: the same write, without the key
		write = plainTool{write}
	}
	who := identity()
	data := filepath.Join(here, "data")
	r, err := agentsafe.New(model,
		&agentsafe.FileLog{Path: filepath.Join(out, *runID+"-log.jsonl"), Codec: logCodec()}, // AGENTSAFE_LOG_KEY: sealed at rest
		agentsafe.WithTools(&ReadCSV{Dir: data}, &CompareRows{Dir: data}, write, &SendPayout{Dir: data, Gateway: gw}),
		agentsafe.WithMaxSteps(*maxSteps),
		agentsafe.WithScope(batch(data)), // same input files = same operations, across runs
		agentsafe.WithLogf(logf),
		agentsafe.WithRedactor(agentsafe.RedactFields("payee")), // payee names never reach the console
		agentsafe.WithHook(killHookFn),                          // KILL_AT=<point> KILL_NTH=<k>: SIGKILL the k-th time the point is reached
		// Who decides, and who may. The identity comes from the OS account (uid), not $USER, which anyone can
		// set; the policy comes from separate config. Never derive both from the same input: if the allowlist
		// were "cli:"+$USER, then USER=anyone would pass by definition. In production, identity comes from your
		// SSO/API auth and the policy from config the approver can't edit.
		agentsafe.WithStartedBy("scheduler"), // runs are started by the system; a human approves (maker-checker)
		agentsafe.WithAuthorizer(agentsafe.All(agentsafe.AllowList(approvers(who)...), agentsafe.NotRequester())),
	)
	must(err)
	var st agentsafe.State
	if *checkOnly {
		events, err := r.Log.Read(context.Background())
		must(err)
		st, err = agentsafe.Rebuild(events)
		must(err)
		fmt.Printf("[%s] CHECK (status %s)\n", *runID, st.Status)
		printReconciliation(filepath.Join(here, "data"), ledger, gw, r.Log, st)
		return
	}
	switch {
	case *approve != "":
		fmt.Printf("[%s] APPROVE %s\n", *runID, *approve)
		st, err = r.Approve(context.Background(), *approve, who)
	case *reject != "":
		fmt.Printf("[%s] REJECT %s\n", *runID, *reject)
		st, err = r.Reject(context.Background(), *reject, who, *reason)
	case *extend > 0:
		fmt.Printf("[%s] EXTEND by %d\n", *runID, *extend)
		st, err = r.Extend(context.Background(), *extend, who)
	case *resume:
		fmt.Printf("[%s] RESUME\n", *runID)
		st, err = r.Continue(context.Background())
	default:
		fmt.Printf("[%s] START\n", *runID)
		st, err = r.Start(context.Background(), system, "Reconcile the two files.")
	}
	if err != nil {
		die(err)
	}
	if fault == "outside_payment" && st.Status == agentsafe.StatusFinished {
		// Someone, not the agent, pays from the same account: a manual payout, another job, an attacker.
		_, err := gw.Pay("manual-ops-1", "Suresh Iyer", 8000, "T1002")
		must(err)
	}
	score(here, ledger, st)
	payments, err := gw.Payments()
	must(err)
	fmt.Printf("gateway payments: %d", len(payments))
	for _, p := range payments {
		fmt.Printf("  [%s %s ₹%v ref=%s]", p.PaymentID, p.Payee, p.Amount, p.Ref)
	}
	fmt.Println("   (truth: exactly 1, Imran Khan ₹11000 ref=T1007, once approved)")
	printReconciliation(filepath.Join(here, "data"), ledger, gw, r.Log, st)
	if st.Status == agentsafe.StatusAwaitingApproval {
		// The approval screen, for the approver: the full summary, payee included (you can't approve a payment
		// without seeing whom it pays). Logf output is masked because logs ship to aggregators and vendors.
		fmt.Printf("\n⏸  WAITING FOR APPROVAL: %s %s\n\n   to approve, run:\n   go run ./examples/reconcile -run %s -approve %s\n\n   to reject, run:\n   go run ./examples/reconcile -run %s -reject %s -reason \"why\"\n",
			st.Waiting.Tool, st.Waiting.Summary, *runID, st.Waiting.Key, *runID, st.Waiting.Key)
	}
}

func score(here string, ledger *Ledger, st agentsafe.State) {
	var truth struct {
		Discrepancies []Row `json:"discrepancies"`
	}
	b, err := os.ReadFile(filepath.Join(here, "data", "ground_truth.json"))
	must(err)
	must(json.Unmarshal(b, &truth))
	want := map[string]Row{}
	for _, d := range truth.Discrepancies {
		want[d.TxnID+":"+d.Kind] = d
	}
	rows, err := ledger.Rows()
	must(err)
	seen, correct, dups := map[string]bool{}, 0, 0
	var wrong []string
	for _, r := range rows {
		k := r.TxnID + ":" + r.Kind
		if seen[k] {
			dups++
			continue
		}
		seen[k] = true
		t, ok := want[k]
		switch {
		case !ok:
			wrong = append(wrong, k+" (not a planted discrepancy)")
		case !eq(r.LedgerAmount, t.LedgerAmount) || !eq(r.BankAmount, t.BankAmount): // every field (week 1 F11)
			wrong = append(wrong, fmt.Sprintf("%s amounts %v/%v, truth %v/%v", k, v(r.LedgerAmount), v(r.BankAmount), v(t.LedgerAmount), v(t.BankAmount)))
		default:
			correct++
		}
	}
	claimed := "none"
	if m := regexp.MustCompile(`TOTAL_DISCREPANCIES:\s*(\d+)`).FindStringSubmatch(st.Text); m != nil {
		claimed = m[1]
	}
	fmt.Printf("\n--- result ---\nstatus=%s stop=%s steps=%d/%d events=%d\n", st.Status, st.Stop, st.Step, st.Budget, st.Events)
	fmt.Printf("ledger rows=%d correct=%d/%d duplicate_writes=%d wrong=%v\n", len(rows), correct, len(want), dups, wrong)
	mark := ""
	if claimed != strconv.Itoa(len(rows)) {
		mark = "   <-- claim != reality"
	}
	fmt.Printf("model CLAIMED %s; ledger has %d rows%s\n", claimed, len(rows), mark)
}

// scripted plans exactly the ground truth, so -mock runs are deterministic and free.
func scripted(here string) *agentsafe.ScriptedModel {
	var truth struct {
		Discrepancies []Row `json:"discrepancies"`
	}
	b, err := os.ReadFile(filepath.Join(here, "data", "ground_truth.json"))
	must(err)
	must(json.Unmarshal(b, &truth))
	plan := []agentsafe.FunctionCall{{Name: "read_csv", Arguments: `{"file":"ledger"}`}, {Name: "read_csv", Arguments: `{"file":"bank"}`}}
	for _, d := range truth.Discrepancies {
		d.Note = "scripted"
		a, _ := json.Marshal(d)
		plan = append(plan, agentsafe.FunctionCall{Name: "record_discrepancy", Arguments: string(a)})
	}
	plan = append(plan, agentsafe.FunctionCall{Name: "send_payout", Arguments: `{"txn_id":"T1007","payee":"Imran Khan","amount_inr":11000}`})
	return &agentsafe.ScriptedModel{Plan: plan, Final: fmt.Sprintf("done\nTOTAL_DISCREPANCIES: %d", len(truth.Discrepancies))}
}

// plainTool hides IdempotentTool, so the runner treats the write as an ordinary tool (no key, no replay).
type plainTool struct{ agentsafe.Tool }

// batch hashes the input files: the idempotency scope (week 2 S1).
func batch(dir string) string {
	h := sha256.New()
	for _, f := range []string{"ledger.csv", "bank.csv"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		must(err)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func readCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	var rows []map[string]string
	for _, rec := range recs[1:] {
		m := map[string]string{}
		for i, h := range recs[0] {
			m[h] = rec[i]
		}
		rows = append(rows, m)
	}
	return rows, nil
}

func eq(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func v(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func logf(f string, a ...any) { fmt.Printf(f+"\n", a...) }

// identity is the verified approver: the OS account running this process, looked up by uid.
func identity() string {
	u, err := user.Current()
	must(err)
	return "cli:" + u.Username
}

// approvers is the approval policy: AGENTSAFE_APPROVERS (comma-separated identities), or, for this demo,
// just the OS account running it.
func approvers(self string) []string {
	if v := os.Getenv("AGENTSAFE_APPROVERS"); v != "" {
		return strings.Split(v, ",")
	}
	return []string{self}
}

// logCodec seals the log when AGENTSAFE_LOG_KEY holds a 32-byte key in hex (e.g. openssl rand -hex 32).
// The key id is "env"; a real deployment fetches keys from a KMS and rotates them (agentsafe.AESGCM).
func logCodec() agentsafe.Codec {
	v := os.Getenv("AGENTSAFE_LOG_KEY")
	if v == "" {
		return nil
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != 32 {
		fmt.Fprintln(os.Stderr, "error: AGENTSAFE_LOG_KEY must be 64 hex characters (32 bytes)")
		os.Exit(2)
	}
	return agentsafe.AESGCM{Keys: map[string][]byte{"env": key}, Current: "env"}
}

func must(err error) {
	if err != nil {
		die(err)
	}
}

func die(err error) { fmt.Fprintln(os.Stderr, "error:", err); os.Exit(1) }
