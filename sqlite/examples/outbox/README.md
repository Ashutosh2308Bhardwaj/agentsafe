# Example: writing to your own database, once

An agent applies a late-delivery policy: order 1001 (cus_42, 850.00) arrived 9 days late, so the customer gets 10% back as store credit, and an email saying so. The credit is a row in **your own database**, which changes the problem. There's no one else's API to trust with an idempotency key: the database *is* the idempotency store.

Three crashes break a naive version:

- **Killed after the credit commits, before the agent logs it.** A restart sees an unfinished call; run it again and the customer gets a second credit.
- **Killed between the commit and sending the email.** If the email is sent in the same code path, it's lost. Send it before the commit, and a rollback leaves the customer told about a credit that doesn't exist.
- **The email worker killed after the email API accepted a message, before marking it sent.** The next run sends it again.

It runs offline: a scripted model, a real SQLite file, and a fake email API on localhost whose records survive a kill. It lives in the `sqlite` module because it needs a database driver (the core module has no dependencies).

## Run it

```bash
cd sqlite
go run ./examples/outbox -crash          # the credit commits; killed before the log knows
go run ./examples/outbox resume          # waits out the dead process's lease, retries: "nothing written"
go run ./examples/outbox -crash worker   # the email is accepted; killed before it's marked sent
go run ./examples/outbox worker          # resent with the same key: still one email
go run ./examples/outbox check           # reconciliation: PASS
rm -r outbox-demo
```

The control, `-no-key` on every command, makes the same crashes write **2 credit notes and send 3 emails**, and `check` fails: `credit:1001 exists 2 times`.

## What makes it work

**One transaction for the effect, its key, and its message** (simplified; the real code, with error handling, is `issueCredit` in `store.go`):

```go
tx := db.BeginTx(ctx, nil)
// already there? the operation happened: return it, write nothing
tx.QueryRow(`SELECT ... FROM credit_notes WHERE op_key = ?`, key)
tx.Exec(`INSERT INTO credit_notes (order_id, customer, amount, op_key) VALUES (...)`) // op_key is UNIQUE
tx.Exec(`INSERT INTO outbox (topic, payload) VALUES ('credit_issued', ...)`)
tx.Commit()
```

- **The key commits with the row.** `KeyFrom(ctx)` is the same on every retry of the operation (`Idempotent("order_id")`: one credit per order). A crash before the commit leaves nothing, and the retry writes it. A crash after it leaves the key, and the retry finds it and writes nothing. There's no moment where the credit exists without its key. That moment, effect done and key not yet recorded, is where duplicates come from. With the effect and key written separately, a random-kill sweep produced 27 duplicate writes; with one transaction, 0 ([docs/EVIDENCE.md](../../../docs/EVIDENCE.md)).
- **`UNIQUE` is the backstop.** Remove the lookup and a retry still can't write a second credit: the insert fails on the key instead.
- **The outbox commits with the credit,** so the customer is told *if and only if* the credit exists. The worker sends pending messages and then marks them sent. A crash in between means a resend, with `outbox-<id>` as the email API's `Idempotency-Key`, so the customer gets one email.
- **The lease.** The agent's log is in the same database (`agentsafe/sqlite`), with a lease that expires instead of disappearing when a process dies. `resume` waits it out (2 s here, 30 s by default). That wait is what stops a process that only *looked* dead (paused, cut off from the network) from coming back and acting a second time.
- **`check` reconciles independently.** It derives what should exist from the source data, reads what does exist from the database, and checks both against the agent's log: every credit exactly once, with the right values, traced to a logged result. It doesn't trust the agent's account.

`main_test.go` runs every step as its own process, with real kills.
