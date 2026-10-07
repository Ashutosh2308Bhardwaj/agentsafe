package agentsafe

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestRedactFieldsMasksAtAnyDepthAndKeepsTheRest(t *testing.T) {
	r := RedactFields("payee", "acct")
	for in, want := range map[string]string{
		`{"payee":"Ramesh","amount":100}`:                    `{"amount":100,"payee":"[REDACTED]"}`,
		`{"rows":[{"payee":"A","n":1},{"payee":"B","n":2}]}`: `{"rows":[{"n":1,"payee":"[REDACTED]"},{"n":2,"payee":"[REDACTED]"}]}`,
		`{"acct":{"number":"987","ifsc":"X"}}`:               `{"acct":"[REDACTED]"}`,
		`{"amount":100}`:                                     `{"amount":100}`,
		`refused: amount does not match`:                     `refused: amount does not match`,
		``:                                                   ``,
		// A tool call's arguments are JSON inside a JSON string: masked inside too.
		`{"arguments":"{\"payee\":\"Ramesh\",\"amount\":1}"}`: `{"arguments":"{\"amount\":1,\"payee\":\"[REDACTED]\"}"}`,
	} {
		if got := r(in); got != want {
			t.Errorf("RedactFields(%s)\n got %s\nwant %s", in, got, want)
		}
	}
	// Nothing to mask: the original bytes, not a re-encoding.
	if in := `{ "b": 1,  "a": 2 }`; r(in) != in {
		t.Errorf("unchanged input must keep its formatting, got %s", r(in))
	}
}

func TestRedactPatternAndChaining(t *testing.T) {
	acct := RedactPattern(regexp.MustCompile(`\b\d{9,18}\b`))
	if got := acct("account 9876543210 failed (ref T1007, 11000)"); got != "account [REDACTED] failed (ref T1007, 11000)" {
		t.Fatalf("got %s", got)
	}
	both := Redactors(RedactFields("payee"), acct)
	if got := both(`{"payee":"Ramesh","note":"acct 9876543210"}`); got != `{"note":"acct [REDACTED]","payee":"[REDACTED]"}` {
		t.Fatalf("got %s", got)
	}
}

func TestConsoleOutputIsRedactedButTheRunUsesRealValues(t *testing.T) {
	r, tool := payRun(t, `{"ref":"T1007","amount":11000,"acct":"9876543210"}`)
	var out strings.Builder
	r.Logf = func(f string, a ...any) { fmt.Fprintf(&out, f+"\n", a...) }
	// Free text (an error quoting the summary) isn't JSON: RedactFields can't see into it, so a pattern covers it.
	r.Redact = Redactors(RedactFields("acct", "ref"), RedactPattern(regexp.MustCompile(`\b\d{9,18}\b`), regexp.MustCompile(`T\d{4}`)))
	r.Authorizer = AuthorizerFunc(func(_ context.Context, a Approval) error {
		if a.By != "ops@test" {
			return fmt.Errorf("payout %s needs ops", a.Summary)
		}
		return nil
	})
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Approve(context.Background(), st.Waiting.Key, "mallory"); err == nil {
		t.Fatal("setup: mallory must be refused, to print a denial")
	}
	if _, err := r.Approve(context.Background(), st.Waiting.Key, "ops@test"); err != nil || tool.paid != 1 {
		t.Fatalf("redaction must not change what runs: err=%v paid=%d", err, tool.paid)
	}
	printed := out.String()
	for _, secret := range []string{"9876543210", "T1007"} {
		if strings.Contains(printed, secret) {
			t.Fatalf("%q reached the console:\n%s", secret, printed)
		}
	}
	if !strings.Contains(printed, Masked) || !strings.Contains(printed, "awaiting approval") || !strings.Contains(printed, "DENIED") {
		t.Fatalf("the console should still say what happened, masked:\n%s", printed)
	}
	// The log keeps the real values: it is what a resume would hand back to the model.
	events, _ := r.Log.Read(context.Background())
	var real bool
	for _, e := range events {
		real = real || strings.Contains(e.Args, "9876543210")
	}
	if !real {
		t.Fatal("redaction must never touch the log")
	}
}

func TestRefusalReasonsAreRedactedOnTheConsole(t *testing.T) {
	r, _ := payRun(t, `{"ref":"T1007","amount":7300}`) // wrong amount: refused by Validate
	var out strings.Builder
	r.Logf = func(f string, a ...any) { fmt.Fprintf(&out, f+"\n", a...) }
	r.Redact = RedactFields("ref")
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "T1007") || !strings.Contains(out.String(), "refused") {
		t.Fatalf("refused call printed unmasked:\n%s", out.String())
	}
}

func TestConsoleMarksWhereItCutsAValue(t *testing.T) {
	if got := clip(`{"amount":"12000.00"}`, 12); got != `{"amount":"1…` {
		t.Fatalf("a cut value must say it was cut: %q", got)
	}
	if got := clip(`{"a":1}`, 12); got != `{"a":1}` {
		t.Fatalf("short text is left alone: %q", got)
	}
}
