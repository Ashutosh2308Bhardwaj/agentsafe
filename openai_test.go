package agentsafe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChat is a /chat/completions endpoint that answers from a script of (status, headers, body).
type fakeChat struct {
	mu      sync.Mutex
	replies []fakeReply
	bodies  []string
	auth    []string
}

type fakeReply struct {
	status  int
	headers map[string]string
	body    string
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	f.bodies, f.auth = append(f.bodies, string(b)), append(f.auth, r.Header.Get("Authorization"))
	rep := fakeReply{status: 500, body: "script exhausted"}
	if len(f.replies) > 0 {
		rep, f.replies = f.replies[0], f.replies[1:]
	}
	for k, v := range rep.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

func (f *fakeChat) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

const okToolCall = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",
"function":{"name":"pay","arguments":"{\"ref\":\"T1\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`

func chatModel(t *testing.T, replies ...fakeReply) (*OpenAICompatible, *fakeChat, *[]string) {
	t.Helper()
	f := &fakeChat{replies: replies}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var logs []string
	var mu sync.Mutex
	return &OpenAICompatible{BaseURL: srv.URL, APIKey: "sk-test", Model: "m", Timeout: 5 * time.Second,
		Logf: func(format string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(format, a...)); mu.Unlock() }}, f, &logs
}

func TestOpenAIDecideParsesAToolCallAndNeverSendsNative(t *testing.T) {
	m, f, _ := chatModel(t, fakeReply{status: 200, body: okToolCall})
	msgs := []Message{{Role: RoleSystem, Content: Str("sys")}, {Role: RoleUser, Content: Str("pay T1")},
		{Role: RoleAssistant, Content: Str("earlier"), Native: json.RawMessage(`{"claude":"thinking blocks"}`)}}
	d, err := m.Decide(context.Background(), msgs, []ToolSpec{{Name: "pay", Description: "pay", Parameters: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if d.FinishReason != "tool_calls" || len(d.Message.ToolCalls) != 1 || d.Message.ToolCalls[0].Function.Arguments != `{"ref":"T1"}` ||
		d.Usage.PromptTokens != 11 || d.Usage.CompletionTokens != 7 {
		t.Fatalf("decision not parsed: %+v", d)
	}
	if strings.Contains(f.bodies[0], "native") || strings.Contains(f.bodies[0], "thinking blocks") {
		t.Fatalf("another provider's Native form must never reach an OpenAI-compatible API: %s", f.bodies[0])
	}
	if f.auth[0] != "Bearer sk-test" || !strings.Contains(f.bodies[0], `"tools":[{"type":"function","function":{"name":"pay"`) {
		t.Fatalf("auth or tools missing: %q %s", f.auth[0], f.bodies[0])
	}
	if msgs[2].Native == nil {
		t.Fatal("stripping Native for the wire must not change the caller's messages")
	}
}

func TestOpenAIRetriesTransientFailuresHonouringRetryAfter(t *testing.T) {
	m, f, logs := chatModel(t,
		fakeReply{status: 429, headers: map[string]string{"Retry-After": "0"}, body: "slow down"},
		fakeReply{status: 503, headers: map[string]string{"Retry-After": "0"}, body: "overloaded"},
		fakeReply{status: 200, body: okToolCall})
	if _, err := m.Decide(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if f.requests() != 3 || len(*logs) != 2 || !strings.Contains((*logs)[0], "HTTP 429") || !strings.Contains((*logs)[0], "server hint") {
		t.Fatalf("want 2 logged retries using the server's hint, got %d requests, logs %q", f.requests(), *logs)
	}
}

func TestOpenAIGivesUpAfterMaxAttempts(t *testing.T) {
	r := fakeReply{status: 500, headers: map[string]string{"Retry-After": "0"}, body: "boom"}
	m, f, _ := chatModel(t, r, r, r, r)
	_, err := m.Decide(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "gave up after 3 attempts") || !strings.Contains(err.Error(), "HTTP 500: boom") || f.requests() != 3 {
		t.Fatalf("want 3 attempts then the last error, got %v after %d", err, f.requests())
	}
}

func TestOpenAIClientErrorsAreNotRetried(t *testing.T) {
	m, f, _ := chatModel(t, fakeReply{status: 400, body: `{"error":"bad tool schema"}`})
	if _, err := m.Decide(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 400") || f.requests() != 1 {
		t.Fatalf("a 4xx is our mistake: no retry, got %v after %d", err, f.requests())
	}
}

func TestOpenAIBadResponsesAreErrorsNotEmptyAnswers(t *testing.T) {
	for body, want := range map[string]string{`{"choices":`: "decode response", `{"choices":[]}`: "no choices"} {
		m, _, _ := chatModel(t, fakeReply{status: 200, body: body})
		if _, err := m.Decide(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", body, want, err)
		}
	}
}

func TestOpenAINetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listening
	m := &OpenAICompatible{BaseURL: url, MaxAttempts: 1}
	if _, err := m.Decide(context.Background(), nil, nil); err == nil || !strings.Contains(err.Error(), "gave up after 1 attempts") {
		t.Fatalf("want a network error, got %v", err)
	}
}

func TestOpenAIPacesWhenTheTokenWindowIsNearlySpent(t *testing.T) {
	low := map[string]string{"x-ratelimit-remaining-tokens": "120", "x-ratelimit-reset-tokens": "60ms"}
	m, _, logs := chatModel(t, fakeReply{status: 200, headers: low, body: okToolCall}, fakeReply{status: 200, body: okToolCall})
	if _, err := m.Decide(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := m.Decide(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) < 50*time.Millisecond || len(*logs) != 1 || !strings.Contains((*logs)[0], "pacing: 120 tokens left") {
		t.Fatalf("the second call must wait for the window: waited %s, logs %q", time.Since(t0), *logs)
	}

	// An unreadable reset time falls back to a long wait, which a cancelled context cuts short.
	m.limits = http.Header{"X-Ratelimit-Remaining-Tokens": {"5"}, "X-Ratelimit-Reset-Tokens": {"soon"}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	t0 = time.Now()
	_, _ = m.Decide(ctx, nil, nil)
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("pacing must stop when the context is cancelled, waited %s", time.Since(t0))
	}
}

func TestOpenAICancelledDuringBackoffReturnsPromptly(t *testing.T) {
	m, _, _ := chatModel(t, fakeReply{status: 429, headers: map[string]string{"Retry-After": "30"}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := m.Decide(ctx, nil, nil)
	if err == nil || time.Since(t0) > 5*time.Second || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a 30s Retry-After must not outlive the caller's context: %v after %s", err, time.Since(t0))
	}
}

func TestOpenAIDescribe(t *testing.T) {
	for url, want := range map[string]string{"https://api.groq.com/openai/v1": "groq", "http://localhost:11434/v1": "openai_compatible"} {
		if p, model := (&OpenAICompatible{BaseURL: url, Model: "m"}).Describe(); p != want || model != "m" {
			t.Errorf("%s -> %s %s", url, p, model)
		}
	}
}
