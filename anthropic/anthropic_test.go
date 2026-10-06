package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// fakeAPI is a Messages API that enforces what the real one does to replayed thinking blocks: every request
// must carry the same system prompt and tools as the first, and its messages must extend the previous
// request's messages byte for byte (append-only). Anything else gets the real API's 400.
type fakeAPI struct {
	t         *testing.T
	mu        sync.Mutex
	responses []string // served in order
	requests  []map[string]json.RawMessage
	headers   []http.Header
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if len(f.requests) > 0 {
		if why := extends(f.requests[len(f.requests)-1], req); why != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid signature in thinking block. The block is bound to a different conversation. (%s)"}}`, why)
			f.requests = append(f.requests, req)
			return
		}
	}
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	if len(f.responses) == 0 {
		http.Error(w, "no more scripted responses", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, f.responses[0])
	f.responses = f.responses[1:]
}

// extends reports why next is not an append-only continuation of prev ("" if it is).
func extends(prev, next map[string]json.RawMessage) string {
	for _, k := range []string{"system", "tools", "model"} {
		if !bytes.Equal(compact(prev[k]), compact(next[k])) {
			return k + " changed"
		}
	}
	var pm, nm []json.RawMessage
	_ = json.Unmarshal(prev["messages"], &pm)
	_ = json.Unmarshal(next["messages"], &nm)
	if len(nm) < len(pm) {
		return fmt.Sprintf("history shrank from %d to %d messages", len(pm), len(nm))
	}
	for i := range pm {
		if !bytes.Equal(compact(pm[i]), compact(nm[i])) {
			return fmt.Sprintf("messages.%d changed:\n  was %s\n  now %s", i, compact(pm[i]), compact(nm[i]))
		}
	}
	return ""
}

func compact(b []byte) []byte {
	var out bytes.Buffer
	if json.Compact(&out, b) != nil {
		return b
	}
	return out.Bytes()
}

func response(id, stop string, blocks ...string) string {
	return fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","model":"claude-opus-5-5","content":[%s],"stop_reason":%q,"stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":20}}`,
		id, strings.Join(blocks, ","), stop)
}

const (
	thinking1 = `{"type":"thinking","thinking":"","signature":"sig-1-bound-to-the-first-request"}`
	thinking2 = `{"type":"thinking","thinking":"","signature":"sig-2"}`
)

type Payout struct {
	InvoiceID string            `json:"invoice_id"`
	Amount    agentsafe.Decimal `json:"amount"`
}

type ledgerTool struct {
	mu    sync.Mutex
	paid  map[string]bool
	reads int
}

func (l *ledgerTool) pay(ctx context.Context, p Payout) (map[string]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paid[agentsafe.KeyFrom(ctx)] = true
	return map[string]string{"status": "paid", "invoice": p.InvoiceID}, nil
}

func (l *ledgerTool) read(context.Context, struct{}) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	return "INV-1 4200.50", nil
}

func newRunner(t *testing.T, _ *fakeAPI, srv *httptest.Server, path string, l *ledgerTool, hook func(string)) *agentsafe.Runner {
	t.Helper()
	m := New(sdk.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0)))
	r, err := agentsafe.New(m, &agentsafe.FileLog{Path: path},
		agentsafe.WithTools(
			agentsafe.Func("read_ledger", "Read the ledger", l.read),
			agentsafe.Func("send_payout", "Pay an invoice", l.pay, agentsafe.Idempotent("invoice_id"),
				agentsafe.NeedsApproval(func(p Payout) any { return p }))),
		agentsafe.WithAuthorizer(agentsafe.AllowList("ops")), agentsafe.WithHook(hook))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The run: Claude reads the ledger twice in parallel, proposes a payout (thinking + tool_use), the run waits
// for approval, a NEW process approves it, the payout runs, the process crashes after logging the result, and
// a THIRD process resumes. Every request Claude receives must extend the previous one exactly.
func TestReplayIsAppendOnlyAcrossApprovalRestartAndCrash(t *testing.T) {
	api := &fakeAPI{t: t, responses: []string{
		response("msg_1", "tool_use", thinking1, `{"type":"text","text":"Checking the ledger."}`,
			`{"type":"tool_use","id":"toolu_a","name":"read_ledger","input":{}}`,
			`{"type":"tool_use","id":"toolu_b","name":"read_ledger","input":{}}`),
		response("msg_2", "tool_use", thinking2,
			`{"type":"tool_use","id":"toolu_c","name":"send_payout","input":{"invoice_id":"INV-1","amount":"4200.50"}}`),
		response("msg_3", "end_turn", `{"type":"text","text":"Paid INV-1."}`),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	l := &ledgerTool{paid: map[string]bool{}}
	ctx := context.Background()

	st, err := newRunner(t, api, srv, path, l, nil).Start(ctx, "You pay invoices.", "Pay INV-1.")
	if err != nil || st.Status != agentsafe.StatusAwaitingApproval {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}

	// Process 2 approves, and crashes right after the payout's result is logged.
	crashed := false
	p2 := newRunner(t, api, srv, path, l, func(p string) {
		if p == "after_result_logged" && !crashed {
			crashed = true
			panic("kill -9")
		}
	})
	func() {
		defer func() { _ = recover() }()
		_, _ = p2.Approve(ctx, st.Waiting.Key, "ops")
	}()
	if !crashed {
		t.Fatal("setup: the crash didn't happen")
	}

	// Process 3 resumes.
	st, err = newRunner(t, api, srv, path, l, nil).Continue(ctx)
	if err != nil || st.Status != agentsafe.StatusFinished {
		t.Fatalf("resume must be accepted by the API: err=%v status=%s", err, st.Status)
	}
	if len(api.requests) != 3 || len(l.paid) != 1 || l.reads != 2 {
		t.Fatalf("want 3 requests, 1 payout, 2 reads: got %d, %d, %d", len(api.requests), len(l.paid), l.reads)
	}

	// The replayed turns carry Claude's thinking blocks, signature intact.
	var msgs []map[string]any
	_ = json.Unmarshal(api.requests[2]["messages"], &msgs)
	if !strings.Contains(string(api.requests[2]["messages"]), "sig-1-bound-to-the-first-request") ||
		!strings.Contains(string(api.requests[2]["messages"]), "sig-2") {
		t.Fatal("thinking blocks must be replayed with their signatures")
	}
	// Parallel calls: both results in ONE user message, in call order.
	results, _ := json.Marshal(msgs[2])
	if msgs[2]["role"] != "user" || strings.Index(string(results), "toolu_a") > strings.Index(string(results), "toolu_b") ||
		!strings.Contains(string(results), "toolu_b") {
		t.Fatalf("the two parallel results must come back in one user message, in order: %s", results)
	}
}

func TestRequestSettings(t *testing.T) {
	api := &fakeAPI{t: t, responses: []string{response("msg_1", "end_turn", `{"type":"text","text":"ok"}`)}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	r := newRunner(t, api, srv, filepath.Join(t.TempDir(), "r.jsonl"), &ledgerTool{paid: map[string]bool{}}, nil)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	req := api.requests[0]
	for field, want := range map[string]string{
		"model":         `"claude-opus-5-5"`,
		"thinking":      `{"block_binding":{"prefix_mismatch_behavior":"error"},"type":"adaptive"}`,
		"output_config": `{"effort":"high"}`,
		"fallbacks":     `"default"`,
		"max_tokens":    `16000`,
	} {
		if got := string(compact(req[field])); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	beta := strings.Join(api.headers[0].Values("anthropic-beta"), ",")
	for _, want := range []string{"thinking-binding-controls-2026-08-01", "server-side-fallback-2026-07-01"} {
		if !strings.Contains(beta, want) {
			t.Errorf("anthropic-beta %q is missing %s", beta, want)
		}
	}
	var tools []map[string]any
	_ = json.Unmarshal(req["tools"], &tools)
	schema, _ := json.Marshal(tools[1]["input_schema"])
	if string(schema) != `{"additionalProperties":false,"properties":{"amount":{"description":"an exact decimal number, as text","pattern":"^-?[0-9]+(\\.[0-9]+)?$","type":"string"},"invoice_id":{"type":"string"}},"required":["invoice_id","amount"],"type":"object"}` {
		t.Errorf("tool schema must reach Claude intact: %s", schema)
	}

	m := &Model{NoFallbacks: true, PrefixMismatch: "drop_block", Effort: "xhigh", Model: "claude-sonnet-5-5"}
	p, err := m.Params(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	for _, want := range []string{`"drop_block"`, `"xhigh"`, `"claude-sonnet-5-5"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("option not applied: %s missing from %s", want, b)
		}
	}
	if strings.Contains(string(b), "fallbacks") {
		t.Error("NoFallbacks must leave fallbacks out")
	}
}

func TestStopReasons(t *testing.T) {
	for stop, want := range map[string]string{"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length",
		"model_context_window_exceeded": "length", "refusal": "refusal"} {
		var resp sdk.BetaMessage
		body := response("m", stop, `{"type":"text","text":"x"}`)
		if stop == "refusal" {
			body = strings.Replace(body, `"stop_sequence":null`, `"stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"}`, 1)
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		d, err := decision(&resp)
		if err != nil || d.FinishReason != want {
			t.Errorf("%s -> %q (err %v), want %q", stop, d.FinishReason, err, want)
		}
		if stop == "refusal" && !strings.Contains(*d.Message.Content, "cyber") {
			t.Errorf("a refusal must say so in the run's final text: %q", *d.Message.Content)
		}
	}
	for _, stop := range []string{"pause_turn", "compaction", "some_future_reason"} {
		var resp sdk.BetaMessage
		_ = json.Unmarshal([]byte(response("m", stop)), &resp)
		if _, err := decision(&resp); err == nil {
			t.Errorf("%s must be an error, not a silent final answer", stop)
		}
	}
}

func TestToolResultsMarkErrors(t *testing.T) {
	m := New(sdk.Client{})
	p, err := m.Params([]agentsafe.Message{
		{Role: agentsafe.RoleSystem, Content: agentsafe.Str("s")},
		{Role: agentsafe.RoleUser, Content: agentsafe.Str("u")},
		{Role: agentsafe.RoleAssistant, ToolCalls: []agentsafe.ToolCall{{ID: "t1", Function: agentsafe.FunctionCall{Name: "x", Arguments: `{}`}}}},
		{Role: agentsafe.RoleTool, ToolCallID: "t1", Content: agentsafe.Str(`{"error":"refused: amount does not match"}`)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p.Messages[2])
	if !strings.Contains(string(b), `"is_error":true`) {
		t.Fatalf("an agentsafe error result must reach Claude as is_error: %s", b)
	}
	if _, err := m.Params([]agentsafe.Message{{Role: agentsafe.RoleUser, Content: agentsafe.Str("u")}, {Role: agentsafe.RoleSystem}}, nil); err == nil {
		t.Fatal("a system message after the first must be refused: it would change the bound prefix")
	}
}
