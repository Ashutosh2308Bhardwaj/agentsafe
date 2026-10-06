package agentsafe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenAICompatible is a Model for any /chat/completions API (Groq, OpenAI, Ollama, vLLM, ...).
// Plain net/http on purpose: timeouts and retries are explicit here, never inherited from an SDK's
// defaults (week 1 F2: one SDK had no timeout, another retried twice silently).
type OpenAICompatible struct {
	BaseURL     string // e.g. https://api.groq.com/openai/v1
	APIKey      string
	Model       string // e.g. openai/gpt-oss-120b
	Timeout     time.Duration
	MaxAttempts int                  // 0 = 3. Retries only transient failures, and says so via Logf.
	Logf        func(string, ...any) // nil = silent

	limits http.Header // last rate-limit headers, for pacing
}

type chatRequest struct {
	Model    string     `json:"model"`
	Messages []Message  `json:"messages"`
	Tools    []chatTool `json:"tools,omitempty"`
}

type chatTool struct {
	Type     string   `json:"type"`
	Function ToolSpec `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// Describe implements Describer. The provider is the host part of BaseURL's API (best effort, for traces).
func (m *OpenAICompatible) Describe() (string, string) {
	for _, known := range []string{"groq", "openai", "anthropic", "mistral", "ollama", "together", "fireworks"} {
		if strings.Contains(m.BaseURL, known) {
			return known, m.Model
		}
	}
	return "openai_compatible", m.Model
}

// Decide implements Model.
func (m *OpenAICompatible) Decide(ctx context.Context, messages []Message, tools []ToolSpec) (Decision, error) {
	wire := make([]Message, len(messages))
	for i, msg := range messages {
		msg.Native = nil // another provider's form: not part of the OpenAI wire format
		wire[i] = msg
	}
	req := chatRequest{Model: m.Model, Messages: wire}
	for _, t := range tools {
		req.Tools = append(req.Tools, chatTool{Type: "function", Function: t})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, err
	}
	attempts := m.MaxAttempts
	if attempts == 0 {
		attempts = 3
	}
	timeout := m.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		m.pace(ctx)
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return Decision{}, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Authorization", "Bearer "+m.APIKey)

		resp, err := client.Do(hreq)
		if err != nil { // network error or timeout: transient
			lastErr = err
			m.backoff(ctx, attempt, attempts, fmt.Sprintf("%v", err), nil)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // fully read; a close error can't change what we got
		m.limits = resp.Header

		switch {
		case resp.StatusCode == http.StatusOK:
			var cr chatResponse
			if err := json.Unmarshal(raw, &cr); err != nil {
				return Decision{}, fmt.Errorf("decode response: %w", err)
			}
			if len(cr.Choices) == 0 {
				return Decision{}, fmt.Errorf("response has no choices: %.200s", raw)
			}
			return Decision{Message: cr.Choices[0].Message, FinishReason: cr.Choices[0].FinishReason, Usage: cr.Usage}, nil
		case resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, raw)
			m.backoff(ctx, attempt, attempts, fmt.Sprintf("HTTP %d", resp.StatusCode), resp.Header)
		default: // 4xx: our request is wrong; retrying can't help
			return Decision{}, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, raw)
		}
	}
	return Decision{}, fmt.Errorf("gave up after %d attempts: %w", attempts, lastErr)
}

// backoff sleeps before the next attempt: the server's Retry-After if it sent one (week 1 F4), else
// exponential with jitter. It logs every retry: a retry you can't see is a duplicate you can't see.
func (m *OpenAICompatible) backoff(ctx context.Context, attempt, attempts int, why string, h http.Header) {
	if attempt == attempts {
		return
	}
	delay := time.Duration(1<<attempt)*time.Second + time.Duration(rand.Intn(1000))*time.Millisecond //nolint:gosec // backoff jitter, not security
	src := "backoff"
	if h != nil {
		if s, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil {
			delay, src = time.Duration(s*float64(time.Second)), "server hint"
		}
	}
	m.logf("[retry %d/%d: %s, sleeping %s (%s)]", attempt, attempts-1, why, delay.Round(100*time.Millisecond), src)
	sleep(ctx, delay)
}

// pace waits for the per-minute token window to reset if the last response said it's nearly spent,
// instead of sending a request we know will be rejected (week 1 F4). Groq sends these headers.
func (m *OpenAICompatible) pace(ctx context.Context) {
	if m.limits == nil {
		return
	}
	left, err := strconv.Atoi(m.limits.Get("x-ratelimit-remaining-tokens"))
	if err != nil || left >= 3000 {
		return
	}
	wait, err := time.ParseDuration(m.limits.Get("x-ratelimit-reset-tokens")) // e.g. "4.965s", "1m26.4s"
	if err != nil {
		wait = 10 * time.Second
	}
	m.logf("[pacing: %d tokens left this minute, waiting %s]", left, wait.Round(100*time.Millisecond))
	sleep(ctx, wait)
}

func (m *OpenAICompatible) logf(f string, a ...any) {
	if m.Logf != nil {
		m.Logf(f, a...)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
