package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

// metaPayout pays once per ref and records the metadata each attempt carried.
type metaPayout struct {
	seen []string
	paid map[string]bool
}

func (m *metaPayout) tool() Tool {
	return Func("pay", "pay", func(ctx context.Context, in struct {
		Ref string `json:"ref"`
	}) (string, error) {
		m.seen = append(m.seen, string(CallMetaFrom(ctx)))
		m.paid[in.Ref] = true
		return "paid " + in.Ref, nil
	}, Idempotent("ref"))
}

func metaGateway(t *testing.T, path string, m *metaPayout, hook func(string)) *Gateway {
	t.Helper()
	g, err := OpenGateway(context.Background(), &FileLog{Path: path}, WithTools(m.tool()), WithHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

const traced = `{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`

func TestTheToolGetsTheCallsMetadataAndARetryGetsTheSame(t *testing.T) {
	ctx := context.Background()
	path, m := filepath.Join(t.TempDir(), "proxy.jsonl"), &metaPayout{paid: map[string]bool{}}
	g := metaGateway(t, path, m, func(p string) {
		if p == "after_tool_executed" {
			panic(crash{})
		}
	})
	func() {
		defer func() { _ = recover() }()
		_, _ = g.Handle(ctx, Request{Client: "c", Tool: "pay", Args: json.RawMessage(`{"ref":"T-1"}`), Meta: json.RawMessage(traced)})
	}()
	_ = g.Close()
	metaGateway(t, path, m, nil) // a new process settles the interrupted call: retried with its key

	if len(m.seen) != 2 || m.seen[0] != traced || m.seen[1] != traced {
		t.Fatalf("the first attempt and the retry after the crash carry the same metadata, from the log: %q", m.seen)
	}
	events, _ := (&FileLog{Path: path}).Read(ctx)
	if events[1].Type != EvCallReceived || events[1].Meta != traced {
		t.Fatalf("the metadata is recorded on call_received: %+v", events[1])
	}
}

// Metadata isn't part of the operation: a trace id differs on every retry, and a retry must be a replay.
func TestMetadataIsNotPartOfTheOperation(t *testing.T) {
	ctx := context.Background()
	m := &metaPayout{paid: map[string]bool{}}
	g := metaGateway(t, filepath.Join(t.TempDir(), "proxy.jsonl"), m, nil)
	args := json.RawMessage(`{"ref":"T-1"}`)
	if _, err := g.Handle(ctx, Request{Tool: "pay", Args: args, Meta: json.RawMessage(`{"traceparent":"a"}`)}); err != nil {
		t.Fatal(err)
	}
	r, err := g.Handle(ctx, Request{Tool: "pay", Args: args, Meta: json.RawMessage(`{"traceparent":"b"}`)})
	if err != nil || !r.Replayed || len(m.seen) != 1 {
		t.Fatalf("same operation, new metadata: a replay, not a conflict or a second effect: %+v %v seen=%q", r, err, m.seen)
	}
	if _, err := g.Handle(ctx, Request{Tool: "pay", Args: args, Meta: json.RawMessage(`[1]`)}); !errors.Is(err, ErrConfig) {
		t.Fatalf("metadata must be a JSON object: %v", err)
	}
}
