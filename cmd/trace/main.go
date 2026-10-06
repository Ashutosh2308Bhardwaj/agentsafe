// Command trace builds an OpenTelemetry trace from an agentsafe event log. Nothing else is needed: the log
// is the source of truth, and the same log always produces the same trace.
//
//	go run ./cmd/trace examples/reconcile/out/p1-log.jsonl                 print the run as a tree
//	go run ./cmd/trace -otlp trace.json examples/reconcile/out/p1-log.jsonl   also write OTLP/JSON
//	go run ./cmd/trace -redact payee -otlp trace.json ...                       mask fields before export
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/trace"
)

func main() {
	otlp := flag.String("otlp", "", "write OTLP/JSON (ExportTraceServiceRequest) to this file")
	agent := flag.String("agent", "reconcile", "gen_ai.agent.name for the root span")
	redact := flag.String("redact", "", "comma-separated JSON fields to mask before printing or exporting, e.g. payee,account")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: trace [-otlp out.json] <log.jsonl>")
		os.Exit(2)
	}
	events, err := (&agentsafe.FileLog{Path: flag.Arg(0), Codec: logCodec()}).Read(context.Background()) // AGENTSAFE_LOG_KEY for sealed logs
	check(err)
	t, err := trace.Build(events, *agent)
	check(err)
	if *redact != "" {
		t.Redact(agentsafe.RedactFields(strings.Split(*redact, ",")...))
	}
	fmt.Print(t.Tree())
	if *otlp != "" {
		b, err := t.OTLPJSON("agentsafe-" + *agent)
		check(err)
		check(os.WriteFile(*otlp, b, 0o600)) // traces can carry tool arguments: owner-only
		fmt.Printf("\nOTLP/JSON: %s (%d spans)\n", *otlp, len(t.Spans))
	}
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

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
