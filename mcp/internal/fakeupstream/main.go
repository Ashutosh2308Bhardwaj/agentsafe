// Command fakeupstream is an MCP server for tests: a billing integration whose books live in a file, so what it
// did survives the processes around it being killed. charge deduplicates on an idempotency key, from an
// argument or from _meta, like a payment API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type books struct {
	Seats   int               `json:"seats"`
	Charges int               `json:"charges"` // what customers were actually billed: the number that must not double
	Done    map[string]string `json:"done"`    // the answer per idempotency key
}

func main() {
	var mu sync.Mutex // the books are a file: one read-modify-write at a time
	path := flag.String("state", "books.json", "file holding the books")
	printCharges := flag.Bool("print-charges", false, "print how many charges the books hold, and exit (for --count)")
	flag.Parse()
	load := func() books {
		b := books{Seats: 10, Done: map[string]string{}}
		if raw, err := os.ReadFile(*path); err == nil {
			_ = json.Unmarshal(raw, &b)
		}
		return b
	}
	save := func(b books) error {
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		return os.WriteFile(*path, raw, 0o600)
	}
	if *printCharges {
		fmt.Println(load().Charges)
		return
	}
	reply := func(s string) *sdk.CallToolResult {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "billing-mcp", Version: "1.0.0"}, nil)
	s.AddTool(&sdk.Tool{Name: "add_seats", Description: "Add seats; each is billed", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"add": map[string]any{"type": "integer"}}}},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct{ Add int }
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			b := load()
			b.Seats += in.Add
			return reply(fmt.Sprintf("now %d seats", b.Seats)), save(b)
		})
	s.AddTool(&sdk.Tool{Name: "charge", Description: "Bill the customer", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"ticket_id": map[string]any{"type": "string"},
			"amount": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}}},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct {
				Amount string `json:"amount"`
				Key    string `json:"idempotency_key"`
			}
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return nil, err
			}
			if k, ok := req.Params.Meta[mcp.MetaKeyIdempotency].(string); ok {
				in.Key = k
			}
			mu.Lock() // the check and the charge are one step: this server honours its key, even under concurrency
			defer mu.Unlock()
			b := load()
			if prev, ok := b.Done[in.Key]; ok && in.Key != "" {
				return reply(prev), nil
			}
			b.Charges++
			answer := fmt.Sprintf("charged %s (charge %d)", in.Amount, b.Charges)
			if in.Key != "" {
				b.Done[in.Key] = answer
			}
			return reply(answer), save(b)
		})
	s.AddTool(&sdk.Tool{Name: "charge_ignores_key", Description: "Bill the customer; takes a key and ignores it",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ticket_id": map[string]any{"type": "string"},
			"amount": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}}}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			mu.Lock() // locked only so the count is exact: it ignores the key either way
			defer mu.Unlock()
			b := load()
			b.Charges++
			return reply(fmt.Sprintf("charged (charge %d)", b.Charges)), save(b)
		})
	s.AddTool(&sdk.Tool{Name: "charge_twice", Description: "Bill the customer; a bug books every charge twice",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"ticket_id": map[string]any{"type": "string"},
			"amount": map[string]any{"type": "string"}}}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			mu.Lock()
			defer mu.Unlock()
			b := load()
			b.Charges += 2
			return reply(fmt.Sprintf("charged (charge %d)", b.Charges)), save(b)
		})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fakeupstream:", err)
		os.Exit(1)
	}
}
