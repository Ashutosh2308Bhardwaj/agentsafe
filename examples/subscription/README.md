# Example: an HTTP call that must happen once

A support agent handles ticket T-1042: *"customer cus_42 asks for 5 more seats."* It calls a billing API, and every seat added is billed. Adding seats is relative, so if the call happens twice, the customer gets 10 seats and two invoices.

Two ordinary failures make it happen twice:

- **The request times out after the API made the change.** The client can't tell "not done" from "done, answer lost". If it gives up, the seats exist and the agent reports a failure. If it retries blindly, the seats are added again.
- **The process dies right after the API made the change**, before it recorded the result. A restart sees an unfinished call. Run it again blindly and the seats are added again.

No API key or model is needed: the model is scripted, and the billing API is a fake served over real HTTP on localhost. It's shaped like Stripe's: a request may carry an `Idempotency-Key`, and a repeated key gets the first response back. Its books live in a file, so they survive this process being killed.

## Run it

```bash
go run ./examples/subscription -slow            # times out after the change, retried: 15 seats, 1 invoice
rm -r subscription-demo

go run ./examples/subscription -crash           # killed right after the change...
go run ./examples/subscription resume           # ...a new process finishes: 15 seats, 1 invoice
rm -r subscription-demo

go run ./examples/subscription -no-key -slow    # the control: 20 seats, 2 invoices
```

| Scenario | Seats | Invoices |
|---|---|---|
| no fault | 15 | 1 × 60.00 |
| `-slow`: timeout after the change | 15 | 1 |
| `-crash`, then `resume` | 15 | 1 |
| `-no-key -slow` (control) | **20** | **2 × 60.00** |
| `-no-key -crash`, then `-no-key resume` (control) | **20** | **2 × 60.00** |

`-no-key` is the most common real bug: the tool has a key and doesn't send it. The same faults then bill twice, which proves the faults are real and that the key is what stops them.

## What makes it work

The whole integration is the tool and two options (shown without the `-no-key` switch):

```go
addSeats := agentsafe.Func("add_seats", "Add seats to a customer's subscription, as a support ticket asks",
	func(ctx context.Context, c SeatChange) (map[string]any, error) {
		return addSeatsRequest(ctx, api.url, c.CustomerID, c.Add, agentsafe.KeyFrom(ctx))
	},
	agentsafe.Idempotent("ticket_id"),         // one operation per ticket
	agentsafe.Timeout(500*time.Millisecond)) // no answer by then: the outcome is unknown, not failed
```

- **`Idempotent("ticket_id")`: the key comes from what the operation *is*.** It comes from the support ticket, never from the model's call id (a model that retries uses a new one) or from the arguments as a whole. "Add 5 seats" asked for in two tickets is two operations; the same ticket retried is one.
- **`KeyFrom(ctx)` goes to the API as its `Idempotency-Key`.** The log stops agentsafe from repeating an operation it *knows* finished. The key stops the API from repeating one agentsafe *can't know* about: the change made just before a timeout or a crash.
- **A timeout is "unknown", not "failed".** The runner retries an unknown outcome with the same key (`WithToolRetries`), and the API answers with the first response.
- **A crash leaves `tool_started` in the log with no result.** That's the write-ahead record: the restarted run knows this call may have happened, and retries it with the same key.

## The limit

agentsafe gives the call a stable key; **the system you call has to honour it**. Make the fake API ignore the key and the protected runs bill twice too: with that change, `TestSubscriptionScenarios` fails on both fault scenarios. If your API has no idempotency keys, look for a way to ask "did request X happen?" (a lookup by your own reference), and do that before retrying. If it has neither, the honest guarantee is at-least-once plus [reconciliation](../reconcile).

The test, `main_test.go`, runs each scenario as real processes with a real kill.
