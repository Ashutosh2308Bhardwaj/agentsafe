// Command fakeupstream is an MCP server for tests: a billing integration whose seats live in a file, so what
// it did survives the processes around it being killed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	state := flag.String("state", "seats.txt", "file holding the seat count")
	flag.Parse()
	seats := func() int {
		b, err := os.ReadFile(*state)
		if err != nil {
			return 10
		}
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "billing-mcp", Version: "1.0.0"}, nil)
	s.AddTool(&sdk.Tool{Name: "add_seats", Description: "Add seats; each is billed", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"add": map[string]any{"type": "integer"}}}},
		func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var in struct{ Add int }
			if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
				return nil, err
			}
			n := seats() + in.Add
			if err := os.WriteFile(*state, []byte(strconv.Itoa(n)), 0o600); err != nil {
				return nil, err
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("now %d seats", n)}}}, nil
		})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fakeupstream:", err)
		os.Exit(1)
	}
}
