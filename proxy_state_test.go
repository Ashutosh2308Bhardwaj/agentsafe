package agentsafe

import (
	"errors"
	"testing"
)

// A proxy run: calls arrive from outside, one at a time; after each resolves, the run is open again.
func proxyHistory() []Event {
	ev := []Event{
		{Type: EvRunStarted, Kind: KindProxy, KeyBits: KeyBits, By: "support-agent"},
		{Type: EvCallReceived, CallID: "c1", Tool: "get_charge", Args: `{"charge_id":"ch_3QA"}`, Client: "langgraph"},
		{Type: EvToolStarted, CallID: "c1", Tool: "get_charge", Args: `{"charge_id":"ch_3QA"}`},
		{Type: EvToolResult, CallID: "c1", Tool: "get_charge", Result: `{"amount":"4200.50"}`},
		{Type: EvCallReceived, CallID: "c2", Tool: "create_refund", Args: `{"charge_id":"ch_3QA","amount":"1200.00"}`},
		{Type: EvToolStarted, CallID: "c2", Tool: "create_refund", Key: "1520a7e1bc6ea3caac5bb435a697b556", PayloadHash: "p"},
		{Type: EvToolResult, CallID: "c2", Tool: "create_refund", Key: "1520a7e1bc6ea3caac5bb435a697b556", PayloadHash: "p",
			Result: `{"id":"re_001"}`},
		{Type: EvCallReceived, CallID: "c3", Tool: "nope", Args: `{}`},
		{Type: EvToolRefused, CallID: "c3", Tool: "nope", Result: `{"error":"no tool named \"nope\""}`},
	}
	for i := range ev {
		ev[i].Seq, ev[i].V = i+1, FormatVersion
	}
	return ev
}

func TestProxyRunCallAfterCall(t *testing.T) {
	ev := proxyHistory()
	for n := 1; n <= len(ev); n++ { // every prefix is a valid run (a crash leaves a prefix)
		if _, err := Rebuild(ev[:n]); err != nil {
			t.Fatalf("prefix of %d events: %v", n, err)
		}
	}
	st, _ := Rebuild(ev)
	if st.Kind != KindProxy || st.Status != StatusOpen || len(st.Messages) != 0 || len(st.Effects) != 1 || st.StartedBy != "support-agent" {
		t.Fatalf("an open proxy run with one keyed effect and no conversation: %+v", st)
	}
	mid, _ := Rebuild(ev[:2])
	if mid.Status != StatusExecuting || len(mid.Pending) != 1 || mid.Pending[0].Function.Name != "get_charge" {
		t.Fatalf("a received call is pending: %+v", mid)
	}
}

func TestProxyAndAgentRunsDontMix(t *testing.T) {
	proxy := proxyHistory()
	agent := []Event{{V: FormatVersion, Seq: 1, Type: EvRunStarted, Task: "t", MaxSteps: 2, KeyBits: KeyBits}}
	next := func(ev []Event, e Event) []Event {
		e.Seq, e.V = len(ev)+1, FormatVersion
		return append(append([]Event{}, ev...), e)
	}
	open := proxy // ends open
	executing := proxy[:2]
	for name, bad := range map[string][]Event{
		"model_decided in a proxy run": next(open, Event{Type: EvModelDecided, Step: 1,
			Message: &Message{Role: RoleAssistant, Content: Str("x")}}),
		"call_received in an agent run":        next(agent, Event{Type: EvCallReceived, CallID: "c", Tool: "t"}),
		"a second call while one is pending":   next(executing, Event{Type: EvCallReceived, CallID: "c9", Tool: "t"}),
		"call_received without a tool":         next(open, Event{Type: EvCallReceived, CallID: "c9"}),
		"approval_requested in a proxy run":    next(executing, Event{Type: EvApprovalRequested, CallID: "c1", Key: "k", Tool: "get_charge"}),
		"run_paused in a proxy run":            next(open, Event{Type: EvRunPaused, Reason: "budget_exhausted"}),
		"a result for a call never started":    next(executing, Event{Type: EvToolResult, CallID: "c1", Result: "x"}),
		"run_started with an unknown kind":     {{V: FormatVersion, Seq: 1, Type: EvRunStarted, Kind: "batch", KeyBits: KeyBits}},
		"a proxy run without a key length":     {{V: FormatVersion, Seq: 1, Type: EvRunStarted, Kind: KindProxy}},
		"a second result for one key, proxied": next(next(next(open, Event{Type: EvCallReceived, CallID: "c4", Tool: "create_refund"}), Event{Type: EvToolStarted, CallID: "c4"}), Event{Type: EvToolResult, CallID: "c4", Key: "1520a7e1bc6ea3caac5bb435a697b556", Result: "x"}),
	} {
		if _, err := Rebuild(bad); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s must be refused, got %v", name, err)
		}
	}
}
