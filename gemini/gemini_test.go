package gemini

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
	"google.golang.org/genai"
)

// fakeAPI is a generateContent endpoint that rejects a request whose systemInstruction, tools or earlier
// contents differ from the previous request's: the replay must be append-only, signatures included.
type fakeAPI struct {
	mu        sync.Mutex
	responses []string
	requests  []map[string]json.RawMessage
	paths     []string
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
			_, _ = fmt.Fprintf(w, `{"error":{"code":400,"message":"history doesn't match: %s","status":"INVALID_ARGUMENT"}}`, strings.ReplaceAll(why, `"`, `'`))
			return
		}
	}
	f.requests = append(f.requests, req)
	f.paths = append(f.paths, r.URL.Path)
	if len(f.responses) == 0 {
		http.Error(w, "no more scripted responses", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, f.responses[0])
	f.responses = f.responses[1:]
}

func extends(prev, next map[string]json.RawMessage) string {
	for _, k := range []string{"systemInstruction", "tools"} {
		if !bytes.Equal(compact(prev[k]), compact(next[k])) {
			return k + " changed"
		}
	}
	var pc, nc []json.RawMessage
	_ = json.Unmarshal(prev["contents"], &pc)
	_ = json.Unmarshal(next["contents"], &nc)
	if len(nc) < len(pc) {
		return "history shrank"
	}
	for i := range pc {
		if !bytes.Equal(compact(pc[i]), compact(nc[i])) {
			return fmt.Sprintf("contents.%d changed: was %s now %s", i, compact(pc[i]), compact(nc[i]))
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

func response(finish string, parts ...string) string {
	return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[%s]},"finishReason":%q}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":30}}`,
		strings.Join(parts, ","), finish)
}

type Payout struct {
	InvoiceID string            `json:"invoice_id"`
	Amount    agentsafe.Decimal `json:"amount"`
}

type ledger struct {
	mu    sync.Mutex
	paid  map[string]bool
	reads int
}

func (l *ledger) pay(ctx context.Context, p Payout) (map[string]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paid[agentsafe.KeyFrom(ctx)] = true
	return map[string]string{"status": "paid", "invoice": p.InvoiceID}, nil
}

func (l *ledger) read(context.Context, struct{}) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	return "INV-1 4200.50", nil // not a JSON object: must reach Gemini wrapped as {"result": ...}
}

func newRunner(t *testing.T, srv *httptest.Server, path string, l *ledger, hook func(string)) *agentsafe.Runner {
	t.Helper()
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: "test", Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := agentsafe.New(New(client, "gemini-test-model"), &agentsafe.FileLog{Path: path},
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

// Same journey as the Claude adapter's test: parallel calls (no ids: the adapter derives them), a gated payout
// (Gemini gives it an id), an approval in a new process that crashes after the payout, and a third process
// that resumes. Every request must extend the previous one exactly, signatures included.
func TestReplayIsAppendOnlyAcrossApprovalRestartAndCrash(t *testing.T) {
	api := &fakeAPI{responses: []string{
		response("STOP",
			`{"text":"I should read the ledger.","thought":true}`,
			`{"functionCall":{"name":"read_ledger","args":{}},"thoughtSignature":"c2lnLWZpcnN0LWNhbGw="}`,
			`{"functionCall":{"name":"read_ledger","args":{}}}`),
		response("STOP",
			`{"functionCall":{"id":"fc_pay_1","name":"send_payout","args":{"invoice_id":"INV-1","amount":"4200.50"}},"thoughtSignature":"c2lnLXBheQ=="}`),
		response("STOP", `{"text":"Paid INV-1."}`),
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "run.jsonl")
	l := &ledger{paid: map[string]bool{}}
	ctx := context.Background()

	st, err := newRunner(t, srv, path, l, nil).Start(ctx, "You pay invoices.", "Pay INV-1.")
	if err != nil || st.Status != agentsafe.StatusAwaitingApproval {
		t.Fatalf("err=%v status=%s", err, st.Status)
	}
	crashed := false
	p2 := newRunner(t, srv, path, l, func(p string) {
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
	st, err = newRunner(t, srv, path, l, nil).Continue(ctx)
	if err != nil || st.Status != agentsafe.StatusFinished {
		t.Fatalf("resume must be accepted: err=%v status=%s", err, st.Status)
	}
	if len(api.requests) != 3 || len(l.paid) != 1 || l.reads != 2 {
		t.Fatalf("want 3 requests, 1 payout, 2 reads: got %d, %d, %d", len(api.requests), len(l.paid), l.reads)
	}
	if !strings.HasSuffix(api.paths[0], "/models/gemini-test-model:generateContent") {
		t.Fatalf("stateless generateContent expected, got %s", api.paths[0])
	}

	last := string(api.requests[2]["contents"])
	for _, want := range []string{"c2lnLWZpcnN0LWNhbGw=", "c2lnLXBheQ==", `"thought":true`} {
		if !strings.Contains(last, want) {
			t.Errorf("model turns must be replayed verbatim (missing %s)", want)
		}
	}
	if strings.Contains(last, derivedPrefix) {
		t.Error("ids the adapter made up must never be sent to Gemini")
	}
	var contents []map[string]any
	_ = json.Unmarshal(api.requests[2]["contents"], &contents)
	reads, _ := json.Marshal(contents[2])
	if contents[2]["role"] != "user" || strings.Count(string(reads), "functionResponse") != 2 ||
		!strings.Contains(string(reads), `"result":"INV-1 4200.50"`) {
		t.Fatalf("both parallel responses in one user turn, non-objects wrapped: %s", reads)
	}
	pay, _ := json.Marshal(contents[4])
	if !strings.Contains(string(pay), `"id":"fc_pay_1"`) || !strings.Contains(string(pay), `"status":"paid"`) {
		t.Fatalf("a response must echo the id Gemini gave its call: %s", pay)
	}
}

func TestRequestShape(t *testing.T) {
	m := New(nil, "gemini-test-model")
	contents, config, err := m.Request([]agentsafe.Message{
		{Role: agentsafe.RoleSystem, Content: agentsafe.Str("You pay invoices.")},
		{Role: agentsafe.RoleUser, Content: agentsafe.Str("Pay INV-1.")},
	}, []agentsafe.ToolSpec{agentsafe.Func("send_payout", "Pay", (&ledger{}).pay).Spec()})
	if err != nil {
		t.Fatal(err)
	}
	if config.SystemInstruction.Parts[0].Text != "You pay invoices." || len(contents) != 1 || config.MaxOutputTokens != DefaultMaxOutputTokens {
		t.Fatalf("system instruction, one user turn, output cap: %+v", config)
	}
	schema, _ := json.Marshal(config.Tools[0].FunctionDeclarations[0].ParametersJsonSchema)
	if !strings.Contains(string(schema), `"additionalProperties":false`) || !strings.Contains(string(schema), `"required":["invoice_id","amount"]`) {
		t.Fatalf("the tool's JSON schema must reach Gemini intact: %s", schema)
	}
	if _, err := New(nil, "").Decide(context.Background(), nil, nil); err == nil {
		t.Fatal("no model id must be an error, not a guessed default")
	}
	if _, _, err := m.Request([]agentsafe.Message{{Role: agentsafe.RoleTool, ToolCallID: "x", Content: agentsafe.Str("{}")}}, nil); err == nil {
		t.Fatal("a response to a call no turn made must be refused")
	}
}

func TestFinishReasons(t *testing.T) {
	for finish, want := range map[string]string{"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "refusal",
		"PROHIBITED_CONTENT": "refusal", "RECITATION": "refusal"} {
		var resp genai.GenerateContentResponse
		if err := json.Unmarshal([]byte(response(finish, `{"text":"x"}`)), &resp); err != nil {
			t.Fatal(err)
		}
		d, err := decision(&resp, 3)
		if err != nil || d.FinishReason != want {
			t.Errorf("%s -> %q (err %v), want %q", finish, d.FinishReason, err, want)
		}
	}
	for _, finish := range []string{"MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "OTHER", "SOME_FUTURE_REASON"} {
		var resp genai.GenerateContentResponse
		_ = json.Unmarshal([]byte(response(finish, `{"text":"x"}`)), &resp)
		if _, err := decision(&resp, 3); err == nil {
			t.Errorf("%s must stop the run with an error, not be taken as an answer", finish)
		}
	}
	var blocked genai.GenerateContentResponse
	_ = json.Unmarshal([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`), &blocked)
	if d, err := decision(&blocked, 3); err != nil || d.FinishReason != "refusal" || !strings.Contains(*d.Message.Content, "SAFETY") {
		t.Fatalf("a blocked prompt is a refusal that says why: %+v %v", d, err)
	}
	var calls genai.GenerateContentResponse
	_ = json.Unmarshal([]byte(response("STOP", `{"functionCall":{"name":"a"}}`, `{"functionCall":{"name":"b","args":{"x":1}}}`)), &calls)
	d, err := decision(&calls, 7)
	if err != nil || d.FinishReason != "tool_calls" || d.Message.ToolCalls[0].ID != "gemini-7-0" ||
		d.Message.ToolCalls[1].ID != "gemini-7-1" || d.Message.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("calls without ids get deterministic, unique ids; no args is {}: %+v %v", d.Message.ToolCalls, err)
	}
}
