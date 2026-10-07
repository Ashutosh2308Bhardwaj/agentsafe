# Example: a refund that's checked, approved, and sent once

Ticket T-77: *"customer cus_42 was overcharged 1200.00 on charge ch_3QA. Refund the overcharge."* A support agent handles it through a payments API shaped like Stripe's. Four things can go wrong, and the first three happen with real models:

1. **The model gets the amount wrong.** Here it slips a decimal place and proposes 12000.00.
2. **The wrong person approves it**, or the person who asked for it approves it themselves.
3. **The process dies right after the money moved**, before it recorded that it did.
4. **The refund is sent again** on restart. Unlike a full refund, which the API would reject as exceeding the charge, a second *partial* refund goes through. That's money gone.

No API key or model is needed: the model is scripted, and the payments API is a fake served over real HTTP on localhost, with its books in a file that survives a kill.

## Run it

```bash
go run ./examples/refund                                      # refuses 12000.00, proposes 1200.00, waits
go run ./examples/refund approve intern@example.com           # refused: not on the approver list
go run ./examples/refund approve support@example.com          # refused: support started this run
go run ./examples/refund approve finance@example.com -crash   # approved, refunded, killed before logging
go run ./examples/refund resume                               # finishes: one refund of 1200.00
rm -r refund-demo
```

Then the control, with the refund tool not sending its key:

```bash
go run ./examples/refund -no-key
go run ./examples/refund -no-key -crash approve finance@example.com
go run ./examples/refund -no-key resume                       # two refunds: 2400.00 back for a 1200.00 overcharge
```

## What makes it work

```go
refund := agentsafe.Func("create_refund", "Refund part or all of a charge, as a support ticket asks",
	func(ctx context.Context, in RefundRequest) (*Refund, error) {
		return createRefund(ctx, api.url, in.ChargeID, string(in.Amount), agentsafe.KeyFrom(ctx))
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
```

(Shown without the `-no-key` switch.) The order is the point:

- **`Check` runs first.** The 12000.00 refund is refused with what the charge actually says ("exceeds the 4200.50 left on ch_3QA"), and the model corrects itself. Nobody is ever asked to approve a refund the data contradicts, so approvers aren't trained to click through nonsense.
- **`ApprovalIf` waits for a human**, durably: the run can sit in the log for days, and any process can approve it. The approver sees the *checked* values.
- **The `Authorizer` decides who may approve.** `AllowList` names finance and the support lead; `NotRequester` stops whoever started the run from approving it (maker-checker). Every refused attempt is recorded in the log.
- **`Idempotent("ticket_id", "charge_id")`** makes the key from the operation's identity. The amount is its *payload*: the same ticket and charge with a different amount is a conflict the model is told about, never a second refund.
- **`KeyFrom(ctx)` goes to the API as its `Idempotency-Key`.** The process was killed after the API refunded and before the log recorded it, so the log can't know. The API can: it answers the retried request with the first refund.

## The limit

agentsafe gives the refund a stable key; **the payments API has to honour it**. Make the fake API ignore the key and the protected run refunds twice: with that change, `TestRefundScenarios` fails. If your provider has no idempotency keys, look the refund up by your own reference before retrying. And check afterwards: [reconciliation](../reconcile) compares what the agent did with what the systems of record say.

`main_test.go` runs every step above as a separate process, with a real kill.
