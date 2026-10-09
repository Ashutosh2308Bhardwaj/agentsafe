// Command retry sends one tool call twice to an MCP server, as a client does when the first answer is lost to a
// timeout, and prints both answers. Run it against a server directly, then through agentsafe-mcp:
//
//	retry --tool issue_write --args '{...}' -- github-mcp-server stdio
//	retry --tool issue_write --args '{...}' -- agentsafe-mcp --log calls.jsonl --policy policy.json -- github-mcp-server stdio
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	tool := flag.String("tool", "", "the tool to call")
	args := flag.String("args", "{}", "its arguments, as JSON")
	flag.Parse()
	if err := run(*tool, json.RawMessage(*args), flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "retry:", err)
		os.Exit(1)
	}
}

func run(tool string, args json.RawMessage, argv []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the command to test is the point
	cmd.Stderr = os.Stderr
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "retry", Version: "0"}, nil).Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = cs.Close() }()
	for i := 1; i <= 2; i++ {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			return err
		}
		text := ""
		for _, c := range res.Content {
			if t, ok := c.(*sdk.TextContent); ok {
				text = t.Text
			}
		}
		if len(text) > 160 {
			text = text[:160] + "..."
		}
		fmt.Printf("call %d: isError=%v replayed=%v %s\n", i, res.IsError, res.Meta["io.github.ashutosh2308bhardwaj.agentsafe/replayed"] == true, text)
	}
	return nil
}
