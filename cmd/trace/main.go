// Command trace builds an OpenTelemetry trace from an agentsafe event log. Nothing else is needed: the log
// is the source of truth, and the same log always produces the same trace.
//
//	go run ./cmd/trace examples/reconcile/out/p1-log.jsonl                 print the run as a tree
//	go run ./cmd/trace -otlp trace.json examples/reconcile/out/p1-log.jsonl   also write OTLP/JSON
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

func main() {
	otlp := flag.String("otlp", "", "write OTLP/JSON (ExportTraceServiceRequest) to this file")
	agent := flag.String("agent", "reconcile", "gen_ai.agent.name for the root span")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: trace [-otlp out.json] <log.jsonl>")
		os.Exit(2)
	}
	events, err := (&agentsafe.FileLog{Path: flag.Arg(0)}).Read()
	check(err)
	t, err := agentsafe.BuildTrace(events, *agent)
	check(err)
	fmt.Print(t.Tree())
	if *otlp != "" {
		b, err := t.OTLPJSON("agentsafe-" + *agent)
		check(err)
		check(os.WriteFile(*otlp, b, 0o600)) // traces can carry tool arguments: owner-only
		fmt.Printf("\nOTLP/JSON: %s (%d spans)\n", *otlp, len(t.Spans))
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
