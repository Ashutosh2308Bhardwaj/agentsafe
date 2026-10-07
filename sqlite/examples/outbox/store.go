package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // the "sqlite" driver
)

// The business tables live in the same SQLite database as the agent's log. op_key is the agentsafe
// idempotency key of the operation that created the row: UNIQUE, so the row and its key can't exist apart.
const schema = `
CREATE TABLE IF NOT EXISTS credit_notes (
	id       INTEGER PRIMARY KEY,
	order_id TEXT NOT NULL,
	customer TEXT NOT NULL,
	amount   TEXT NOT NULL,
	op_key   TEXT UNIQUE
);
CREATE TABLE IF NOT EXISTS outbox (
	id      INTEGER PRIMARY KEY,
	topic   TEXT NOT NULL,
	payload TEXT NOT NULL,
	sent    INTEGER NOT NULL DEFAULT 0
);`

// openDB opens the database with the settings agentsafe/sqlite.Open uses (durable commits, waiting for
// other writers), and creates the business tables.
func openDB(path string) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(FULL)", "fullfsync(1)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// CreditNote is store credit issued to a customer.
type CreditNote struct {
	ID       int64  `json:"id"`
	OrderID  string `json:"order_id"`
	Customer string `json:"customer"`
	Amount   string `json:"amount"`
}

// issueCredit writes the credit note, its idempotency key and the message announcing it in ONE transaction.
// If the key is already there, the operation already happened: it returns that credit note and writes
// nothing. The database is the idempotency store, so this is exactly-once without anyone else's help.
func issueCredit(ctx context.Context, db *sql.DB, key, orderID, customer, amount string) (CreditNote, bool, error) {
	c := CreditNote{OrderID: orderID, Customer: customer, Amount: amount}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return c, false, err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	if key != "" {
		err := tx.QueryRowContext(ctx, `SELECT id, order_id, customer, amount FROM credit_notes WHERE op_key = ?`, key).
			Scan(&c.ID, &c.OrderID, &c.Customer, &c.Amount)
		if err == nil {
			return c, true, tx.Commit() // done before: a retry after a crash or a timeout lands here
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return c, false, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO credit_notes (order_id, customer, amount, op_key) VALUES (?, ?, ?, NULLIF(?, ''))`,
		orderID, customer, amount, key)
	if err != nil {
		return c, false, err
	}
	if c.ID, err = res.LastInsertId(); err != nil {
		return c, false, err
	}
	payload, err := json.Marshal(map[string]any{"to": customer, "template": "credit_issued", "credit_note": c.ID, "amount": amount})
	if err != nil {
		return c, false, err
	}
	// The outbox row commits with the credit note: the customer is told if and only if the credit exists.
	// Sending the email here instead would tell them about a credit a rollback then removed, or (sent after
	// the commit) lose the email if the process died in between.
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (topic, payload) VALUES ('credit_issued', ?)`, string(payload)); err != nil {
		return c, false, err
	}
	return c, false, tx.Commit()
}

// creditNotes lists every credit note with its key: what the system of record says happened.
func creditNotes(ctx context.Context, db *sql.DB) ([]CreditNote, []string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, order_id, customer, amount, COALESCE(op_key, '') FROM credit_notes ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var notes []CreditNote
	var keys []string
	for rows.Next() {
		var c CreditNote
		var key string
		if err := rows.Scan(&c.ID, &c.OrderID, &c.Customer, &c.Amount, &key); err != nil {
			return nil, nil, err
		}
		notes, keys = append(notes, c), append(keys, key)
	}
	return notes, keys, rows.Err()
}

type outboxRow struct {
	ID      int64
	Payload string
}

func pendingOutbox(ctx context.Context, db *sql.DB) ([]outboxRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, payload FROM outbox WHERE sent = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func markSent(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `UPDATE outbox SET sent = 1 WHERE id = ?`, id)
	return err
}

func outboxKey(id int64) string { return fmt.Sprintf("outbox-%d", id) }
