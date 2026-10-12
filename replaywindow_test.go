package agentsafe

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// comment is a write whose operation is identified by all its arguments (as the MCP proxy's ["*"] does): the
// same text on the same issue is the same operation. It remembers each operation for window, and counts what it
// did per key it was sent. unknown makes a call act and then report an unknown outcome; honours is whether the
// system it calls deduplicates on the key.
type comment struct {
	window  time.Duration
	unknown bool
	honours bool
	keys    []string // the key of each effect, in order
}

func (c *comment) Spec() ToolSpec {
	return ToolSpec{Name: "comment", Description: "comment", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (c *comment) Call(context.Context, json.RawMessage) (any, error) {
	return nil, fmt.Errorf("use CallWithKey")
}
func (c *comment) Identity(args json.RawMessage) (any, any, error) {
	var all map[string]any
	return all, map[string]any{}, json.Unmarshal(args, &all)
}
func (c *comment) ReplayWindow() time.Duration { return c.window }
func (c *comment) HonoursKey() bool            { return c.honours }
func (c *comment) CallWithKey(_ context.Context, key string, _ json.RawMessage) (any, error) {
	c.keys = append(c.keys, key)
	if c.unknown {
		return nil, fmt.Errorf("%w: timed out after posting", ErrOutcomeUnknown)
	}
	return fmt.Sprintf("comment %d", len(c.keys)), nil
}

// clockGateway is a gateway over path whose clock is *now, so a test can let a week pass.
func clockGateway(t *testing.T, path string, tool Tool, now *time.Time) *Gateway {
	t.Helper()
	g, err := OpenGateway(context.Background(), &FileLog{Path: path}, WithTools(tool))
	if err != nil {
		t.Fatal(err)
	}
	g.r.now = func() time.Time { return *now }
	return g
}

const lgtm = `{"issue":7,"body":"LGTM"}`

// The same comment a minute later is a retry, answered from the log. A week later it's a deliberate repeat: it
// runs, with a new key, and a retry of THAT is answered from the log, by this gateway and by one restarted on it.
func TestADeliberateRepeatAfterTheReplayWindowRunsAgain(t *testing.T) {
	tool := &comment{window: 10 * time.Minute, honours: true}
	path := filepath.Join(t.TempDir(), "proxy.jsonl")
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	g := clockGateway(t, path, tool, &now)

	first := gwCall(t, g, "comment", lgtm)
	now = now.Add(time.Minute)
	if r := gwCall(t, g, "comment", lgtm); !r.Replayed || len(tool.keys) != 1 {
		t.Fatalf("a minute later it's a retry, answered from the log: %+v, %d effects", r, len(tool.keys))
	}
	now = now.Add(7 * 24 * time.Hour)
	week := gwCall(t, g, "comment", lgtm)
	if week.Replayed || week.Result == first.Result || len(tool.keys) != 2 {
		t.Fatalf("a week later it's a new comment: %+v, %d effects", week, len(tool.keys))
	}
	if tool.keys[0] == tool.keys[1] {
		t.Fatal("the new operation needs its own key, or a downstream that deduplicates would answer it with the first")
	}
	now = now.Add(time.Minute)
	if r := gwCall(t, g, "comment", lgtm); !r.Replayed || r.Result != replay(week.Result) {
		t.Fatalf("a retry of the new comment is answered from the log: %+v", r)
	}
	_ = g.Close()

	now = now.Add(time.Minute)
	g = clockGateway(t, path, tool, &now)
	defer func() { _ = g.Close() }()
	if r := gwCall(t, g, "comment", lgtm); !r.Replayed || r.Result != replay(week.Result) || len(tool.keys) != 2 {
		t.Fatalf("restarted on the log, the window runs from the recorded time: %+v, %d effects", r, len(tool.keys))
	}
}

// An unknown outcome may have happened: it never expires into a new operation, however long ago it was.
func TestAnUnknownOutcomeNeverExpires(t *testing.T) {
	tool := &comment{window: 10 * time.Minute, unknown: true}
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	g := clockGateway(t, filepath.Join(t.TempDir(), "proxy.jsonl"), tool, &now)
	defer func() { _ = g.Close() }()

	if r := gwCall(t, g, "comment", lgtm); !strings.Contains(r.Result, "outcome unknown") {
		t.Fatalf("recorded as unknown: %+v", r)
	}
	now = now.Add(30 * 24 * time.Hour)
	if r := gwCall(t, g, "comment", lgtm); !r.Replayed || !strings.Contains(r.Result, "outcome unknown") || len(tool.keys) != 1 {
		t.Fatalf("a month later it's still unknown, not retried: %+v, %d attempts", r, len(tool.keys))
	}
}

// Without a window, an operation is remembered for the life of the log.
func TestWithoutAReplayWindowAnOperationIsRemembered(t *testing.T) {
	tool := &comment{honours: true}
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	g := clockGateway(t, filepath.Join(t.TempDir(), "proxy.jsonl"), tool, &now)
	defer func() { _ = g.Close() }()

	gwCall(t, g, "comment", lgtm)
	now = now.Add(365 * 24 * time.Hour)
	if r := gwCall(t, g, "comment", lgtm); !r.Replayed || len(tool.keys) != 1 {
		t.Fatalf("a year later, still the same operation: %+v, %d effects", r, len(tool.keys))
	}
}
