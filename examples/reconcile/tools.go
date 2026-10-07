package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Row is one recorded discrepancy.
type Row struct {
	ID           int      `json:"discrepancy_id,omitempty"`
	Key          string   `json:"key,omitempty"` // the idempotency key travels INTO the effect (option b)
	TxnID        string   `json:"txn_id"`
	Kind         string   `json:"kind"`
	LedgerAmount *float64 `json:"ledger_amount"`
	BankAmount   *float64 `json:"bank_amount"`
	Note         string   `json:"note,omitempty"`
}

// ReadCSV is a read-only tool: it returns a source file's rows.
type ReadCSV struct{ Dir string }

// Spec describes the tool to the model.
func (t *ReadCSV) Spec() agentsafe.ToolSpec {
	return agentsafe.ToolSpec{Name: "read_csv",
		Description: "Read a whole file. 'ledger' = payouts we sent. 'bank' = the bank's settlement file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","enum":["ledger","bank"]}},"required":["file"]}`)}
}

// Call reads the file.
func (t *ReadCSV) Call(_ context.Context, args json.RawMessage) (any, error) {
	var a struct{ File string }
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if a.File != "ledger" && a.File != "bank" {
		return nil, fmt.Errorf("unknown file %q", a.File)
	}
	rows, err := readCSV(filepath.Join(t.Dir, a.File+".csv"))
	return map[string]any{"file": a.File, "rows": rows}, err
}

// CompareRows is a read-only tool: it returns both files' rows for one transaction.
type CompareRows struct{ Dir string }

// Spec describes the tool to the model.
func (t *CompareRows) Spec() agentsafe.ToolSpec {
	return agentsafe.ToolSpec{Name: "compare_rows",
		Description: "Return every row with this txn_id from BOTH files, side by side.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"txn_id":{"type":"string"}},"required":["txn_id"]}`)}
}

// Call returns the rows for the transaction from both files.
func (t *CompareRows) Call(_ context.Context, args json.RawMessage) (any, error) {
	var a struct {
		TxnID string `json:"txn_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	out := map[string]any{"txn_id": a.TxnID}
	for _, f := range []string{"ledger", "bank"} {
		rows, err := readCSV(filepath.Join(t.Dir, f+".csv"))
		if err != nil {
			return nil, err
		}
		var match []map[string]string
		for _, r := range rows {
			if r["txn_id"] == a.TxnID {
				match = append(match, r)
			}
		}
		out[f] = match
	}
	return out, nil
}

// RecordDiscrepancy is the WRITE. It implements agentsafe.IdempotentTool: it declares what identifies the
// operation, and its effect dedupes on the key the library hands it. The library does the rest.
type RecordDiscrepancy struct {
	Ledger *Ledger
	Dir    string // source files, for Validate
}

// Identity says (txn_id, kind) define the discrepancy. The amounts are the payload (a retry with different
// amounts is a conflict). The note is in neither: rewording it doesn't make a new discrepancy.
func (t *RecordDiscrepancy) Identity(args json.RawMessage) (any, any, error) {
	var r Row
	if err := json.Unmarshal(args, &r); err != nil {
		return nil, nil, err
	}
	return map[string]any{"txn_id": r.TxnID, "kind": r.Kind},
		map[string]any{"ledger_amount": r.LedgerAmount, "bank_amount": r.BankAmount}, nil
}

// CallWithKey writes the row WITH its key in one line, so the effect and its key can't be separated by a
// crash; and returns the existing row if the key is already there (the post-crash retry).
func (t *RecordDiscrepancy) CallWithKey(_ context.Context, key string, args json.RawMessage) (any, error) {
	var r Row
	if err := json.Unmarshal(args, &r); err != nil {
		return nil, err
	}
	if !slices.Contains(kinds, r.Kind) {
		return nil, fmt.Errorf("unknown kind %q", r.Kind)
	}
	r.Key = key
	id, existed, err := t.Ledger.AppendOnce(r)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"recorded": true, "discrepancy_id": id}
	if existed {
		out["already_recorded"] = true
	}
	return out, nil
}

// Spec describes the tool to the model.
func (t *RecordDiscrepancy) Spec() agentsafe.ToolSpec {
	return agentsafe.ToolSpec{Name: "record_discrepancy",
		Description: "Record ONE discrepancy in the reconciliation ledger. This is a write: each call adds a row.",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"txn_id":{"type":"string"},
			"kind":{"type":"string","enum":["amount_mismatch","missing_in_bank","missing_in_ledger","duplicate_in_bank","duplicate_in_ledger"]},
			"ledger_amount":{"type":["number","null"]},"bank_amount":{"type":["number","null"]},"note":{"type":"string"}},
			"required":["txn_id","kind","ledger_amount","bank_amount","note"]}`)}
}

// Call writes without a key. The runner uses it only when the tool is wrapped in plainTool (NO_IDEMPOTENCY=1).
func (t *RecordDiscrepancy) Call(_ context.Context, args json.RawMessage) (any, error) {
	var r Row
	if err := json.Unmarshal(args, &r); err != nil {
		return nil, err
	}
	if !slices.Contains(kinds, r.Kind) {
		return nil, fmt.Errorf("unknown kind %q", r.Kind)
	}
	id, err := t.Ledger.Append(r)
	if err != nil {
		return nil, err
	}
	return map[string]any{"recorded": true, "discrepancy_id": id}, nil
}

// Ledger is the system of record the write touches: one JSON line per discrepancy.
type Ledger struct {
	Path     string
	DropNext bool // silent fault (week 4 S3): the next new write is acknowledged and NOT stored
}

// Rows reads every recorded discrepancy.
func (l *Ledger) Rows() ([]Row, error) {
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	var rows []Row
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Row
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

// AppendOnce appends r unless a row with the same key exists, in which case it returns that row's id.
// One line = row + key: the write and its idempotency record are a single append.
func (l *Ledger) AppendOnce(r Row) (id int, existed bool, err error) {
	rows, err := l.Rows()
	if err != nil {
		return 0, false, err
	}
	for _, x := range rows {
		if r.Key != "" && x.Key == r.Key {
			return x.ID, true, nil
		}
	}
	if l.DropNext {
		l.DropNext = false
		return len(rows) + 1, false, nil // "recorded", with a plausible id. Nothing was written.
	}
	id, err = l.Append(r)
	return id, false, err
}

// Append writes a row without checking for an existing one: AppendOnce calls it after its check, and on its own
// it is the control experiment's write (a retry adds a second row).
func (l *Ledger) Append(r Row) (int, error) {
	rows, err := l.Rows()
	if err != nil {
		return 0, err
	}
	r.ID = len(rows) + 1
	b, err := json.Marshal(r)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return 0, err
	}
	// A write path must check Close: some filesystems only report a failed write here.
	return r.ID, f.Close()
}

// ---- grounding: the source of truth for both tools -------------------------------------------------------

// source returns the ledger and bank rows for a txn_id, with amounts parsed.
func source(dir, txnID string) (ledger, bank []map[string]string, err error) {
	for _, f := range []string{"ledger", "bank"} {
		rows, err := readCSV(filepath.Join(dir, f+".csv"))
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rows {
			if r["txn_id"] == txnID {
				if f == "ledger" {
					ledger = append(ledger, r)
				} else {
					bank = append(bank, r)
				}
			}
		}
	}
	return ledger, bank, nil
}

func amountOf(rows []map[string]string) *float64 {
	if len(rows) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(rows[0]["amount_inr"], 64)
	if err != nil {
		return nil
	}
	return &v
}

func show(p *float64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

// Validate grounds a discrepancy in the two files BEFORE it is written (week 3 W3-5: every value the model
// got wrong was on a missing_* row). The error tells the model what the source says, so it can correct
// itself; a refused call writes nothing and doesn't consume the operation's key.
func (t *RecordDiscrepancy) Validate(_ context.Context, args json.RawMessage) error {
	var r Row
	if err := json.Unmarshal(args, &r); err != nil {
		return err
	}
	ledger, bank, err := source(t.Dir, r.TxnID)
	if err != nil {
		return err
	}
	wantL, wantB, err := expectedAmounts(r.Kind, r.TxnID, ledger, bank)
	if err != nil {
		return err
	}
	if !eq(r.LedgerAmount, wantL) || !eq(r.BankAmount, wantB) {
		return fmt.Errorf("for %s %s the files say ledger_amount=%s, bank_amount=%s; you sent %s, %s",
			r.TxnID, r.Kind, show(wantL), show(wantB), show(r.LedgerAmount), show(r.BankAmount))
	}
	return nil
}

// expectedAmounts says whether the files show a discrepancy of this kind for txnID and, if they do, which
// amounts a correct record of it carries (nil: that side has no amount).
func expectedAmounts(kind, txnID string, ledger, bank []map[string]string) (wantL, wantB *float64, err error) {
	la, ba := amountOf(ledger), amountOf(bank)
	switch kind {
	case "amount_mismatch":
		if la == nil || ba == nil || *la == *ba {
			return nil, nil, fmt.Errorf("%s is not an amount mismatch in the files (ledger %s, bank %s)", txnID, show(la), show(ba))
		}
		return la, ba, nil
	case "missing_in_bank":
		if la == nil || len(bank) > 0 {
			return nil, nil, fmt.Errorf("%s is not missing from the bank (ledger rows %d, bank rows %d)", txnID, len(ledger), len(bank))
		}
		return la, nil, nil
	case "missing_in_ledger":
		if ba == nil || len(ledger) > 0 {
			return nil, nil, fmt.Errorf("%s is not missing from the ledger (ledger rows %d, bank rows %d)", txnID, len(ledger), len(bank))
		}
		return nil, ba, nil
	case "duplicate_in_bank":
		if len(bank) < 2 {
			return nil, nil, fmt.Errorf("%s appears %d time(s) in the bank file, not twice", txnID, len(bank))
		}
		return la, ba, nil
	case "duplicate_in_ledger":
		if len(ledger) < 2 {
			return nil, nil, fmt.Errorf("%s appears %d time(s) in the ledger, not twice", txnID, len(ledger))
		}
		return la, ba, nil
	default:
		return nil, nil, fmt.Errorf("unknown kind %q", kind)
	}
}

// ---- send_payout: irreversible, validated, gated, idempotent ---------------------------------------------

// SendPayout re-issues a payout that our ledger says was sent but the bank never settled.
type SendPayout struct {
	Dir     string
	Gateway *Gateway
}

type payoutArgs struct {
	TxnID  string  `json:"txn_id"`
	Payee  string  `json:"payee"`
	Amount float64 `json:"amount_inr"`
}

// Spec describes the tool to the model.
func (t *SendPayout) Spec() agentsafe.ToolSpec {
	return agentsafe.ToolSpec{Name: "send_payout",
		Description: "Re-send a payout that is in our ledger but missing from the bank settlement (the payee was never " +
			"paid). Moves real money: a human approves it before it runs. One call per txn_id.",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"txn_id":{"type":"string"},"payee":{"type":"string"},"amount_inr":{"type":"number"}},
			"required":["txn_id","payee","amount_inr"]}`)}
}

func (t *SendPayout) parse(a json.RawMessage) (payoutArgs, error) {
	var p payoutArgs
	return p, json.Unmarshal(a, &p)
}

// Call refuses: a payout is only ever sent with its idempotency key (CallWithKey).
func (t *SendPayout) Call(context.Context, json.RawMessage) (any, error) {
	return nil, errors.New("send_payout must run through the idempotent path")
}

// Identity says a re-issue OF a txn_id is the operation; payee and amount are its payload. Identity comes from
// the source data (the txn_id), never from a UUID the model makes up (week 2 S1).
func (t *SendPayout) Identity(a json.RawMessage) (any, any, error) {
	p, err := t.parse(a)
	return map[string]any{"op": "reissue", "txn_id": p.TxnID}, map[string]any{"payee": p.Payee, "amount_inr": p.Amount}, err
}

// Validate checks that the txn is be in the ledger and absent from the bank, and payee + amount must equal the ledger.
func (t *SendPayout) Validate(_ context.Context, a json.RawMessage) error {
	p, err := t.parse(a)
	if err != nil {
		return err
	}
	ledger, bank, err := source(t.Dir, p.TxnID)
	if err != nil {
		return err
	}
	if len(ledger) != 1 || len(bank) != 0 {
		return fmt.Errorf("%s is not a payout missing from the bank (ledger rows %d, bank rows %d): nothing to re-send",
			p.TxnID, len(ledger), len(bank))
	}
	if la := amountOf(ledger); la == nil || *la != p.Amount || ledger[0]["payee"] != p.Payee {
		return fmt.Errorf("the ledger says %s pays %s ₹%s; you sent %s ₹%v", p.TxnID, ledger[0]["payee"], show(la), p.Payee, p.Amount)
	}
	return nil
}

// NeedsApproval is always true: money never moves without a human.
func (t *SendPayout) NeedsApproval(json.RawMessage) bool { return true }

// Summary is built from validated args only (Validate runs first).
func (t *SendPayout) Summary(a json.RawMessage) (any, error) {
	p, err := t.parse(a)
	return map[string]any{"action": "send_payout", "payee": p.Payee, "amount_inr": p.Amount, "ref": p.TxnID,
		"why": "in our ledger as sent; absent from the bank settlement file"}, err
}

// CallWithKey sends with the key as the gateway's Idempotency-Key. If the response is lost, ask the gateway
// by key before anything else (outcome unknown → look, don't guess: week 1 F13).
func (t *SendPayout) CallWithKey(_ context.Context, key string, a json.RawMessage) (any, error) {
	p, err := t.parse(a)
	if err != nil {
		return nil, err
	}
	pay, err := t.Gateway.Pay(key, p.Payee, p.Amount, p.TxnID)
	if errors.Is(err, ErrTimeout) {
		found, serr := t.Gateway.Status(key)
		if serr != nil {
			return nil, fmt.Errorf("outcome unknown and status check failed (key %s): %w", key, serr)
		}
		if found != nil {
			return map[string]any{"paid": true, "payment_id": found.PaymentID, "resolved_by": "status lookup after a timeout"}, nil
		}
		if pay, err = t.Gateway.Pay(key, p.Payee, p.Amount, p.TxnID); err != nil { // not found: safe to retry, same key
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return map[string]any{"paid": true, "payment_id": pay.PaymentID}, nil
}
