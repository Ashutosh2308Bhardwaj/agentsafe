package trace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/trace"
)

type Payout struct {
	InvoiceID string `json:"invoice_id"`
}

// Build turns a run's log into OpenTelemetry spans (GenAI semantic conventions): the run, each model call,
// each tool call. The log is the only input, so the same log always gives the same trace.
func ExampleBuild() {
	dir, _ := os.MkdirTemp("", "agentsafe-trace")
	defer func() { _ = os.RemoveAll(dir) }()
	pay := agentsafe.Func("send_payout", "Pay", func(context.Context, Payout) (string, error) { return "paid", nil },
		agentsafe.Idempotent("invoice_id"))
	log := &agentsafe.FileLog{Path: filepath.Join(dir, "run.jsonl")}
	r, _ := agentsafe.New(&agentsafe.ScriptedModel{
		Plan: []agentsafe.FunctionCall{{Name: "send_payout", Arguments: `{"invoice_id":"INV-1"}`}}, Final: "done"},
		log, agentsafe.WithTools(pay))
	_, _ = r.Start(context.Background(), "sys", "Pay INV-1.")

	events, _ := log.Read(context.Background())
	tr, _ := trace.Build(events, "payouts")
	for _, s := range tr.Spans {
		fmt.Println(s.Name)
	}
	// Output:
	// invoke_agent payouts
	// chat scripted
	// execute_tool send_payout
	// chat scripted
}
