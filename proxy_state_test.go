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
		"call_received in an agent run":           next(agent, Event{Type: EvCallReceived, CallID: "c", Tool: "t"}),
		"a second call while one is pending":      next(executing, Event{Type: EvCallReceived, CallID: "c9", Tool: "t"}),
		"call_received without a tool":            next(open, Event{Type: EvCallReceived, CallID: "c9"}),
		"a decision for a key that isn't waiting": next(open, Event{Type: EvApprovalDecided, CallID: "c1", Key: "k", Decision: "approved", By: "ops"}),
		"run_paused in a proxy run":               next(open, Event{Type: EvRunPaused, Reason: "budget_exhausted"}),
		"a result for a call never started":       next(executing, Event{Type: EvToolResult, CallID: "c1", Result: "x"}),
		"run_started with an unknown kind":        {{V: FormatVersion, Seq: 1, Type: EvRunStarted, Kind: "batch", KeyBits: KeyBits}},
		"a proxy run without a key length":        {{V: FormatVersion, Seq: 1, Type: EvRunStarted, Kind: KindProxy}},
		"a second result for one key, proxied":    next(next(next(open, Event{Type: EvCallReceived, CallID: "c4", Tool: "create_refund"}), Event{Type: EvToolStarted, CallID: "c4"}), Event{Type: EvToolResult, CallID: "c4", Key: "1520a7e1bc6ea3caac5bb435a697b556", Result: "x"}),
	} {
		if _, err := Rebuild(bad); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s must be refused, got %v", name, err)
		}
	}
}

// Approvals in a proxy run: the call that needs one is answered at once (pending) and the run stays open; the
// decision arrives later, by key; a later call with the key runs (approved) or is refused (rejected).
func TestProxyApprovalsByKey(t *testing.T) {
	const k, k2 = "1520a7e1bc6ea3caac5bb435a697b556", "9e107d9d372bb6826bd81d3542a419d6"
	ev := []Event{
		{Type: EvRunStarted, Kind: KindProxy, KeyBits: KeyBits, By: "support-agent"},
		{Type: EvCallReceived, CallID: "c1", Tool: "refund", Args: `{"ticket_id":"T-77"}`},
		{Type: EvApprovalRequested, CallID: "c1", Tool: "refund", Key: k, Summary: `{"ticket_id":"T-77"}`},
		{Type: EvCallReceived, CallID: "c2", Tool: "get_charge", Args: `{}`}, // other calls flow meanwhile
		{Type: EvToolStarted, CallID: "c2", Tool: "get_charge"},
		{Type: EvToolResult, CallID: "c2", Tool: "get_charge", Result: `"4200.50"`},
		{Type: EvApprovalDenied, CallID: "c1", Key: k, Decision: "approved", By: "intern", Reason: "not on the list"},
		{Type: EvApprovalDecided, CallID: "c1", Key: k, Decision: "approved", By: "finance"},
		{Type: EvCallReceived, CallID: "c3", Tool: "refund", Args: `{"ticket_id":"T-77"}`},
		{Type: EvToolStarted, CallID: "c3", Tool: "refund", Key: k},
		{Type: EvToolResult, CallID: "c3", Tool: "refund", Key: k, Result: `{"id":"re_001"}`},
		{Type: EvCallReceived, CallID: "c4", Tool: "refund", Args: `{"ticket_id":"T-78"}`},
		{Type: EvApprovalRequested, CallID: "c4", Tool: "refund", Key: k2, Summary: `{"ticket_id":"T-78"}`},
		{Type: EvApprovalDecided, CallID: "c4", Key: k2, Decision: "rejected", By: "finance", Reason: "duplicate ticket"},
	}
	for i := range ev {
		ev[i].Seq, ev[i].V = i+1, FormatVersion
	}
	for n := 1; n <= len(ev); n++ {
		if _, err := Rebuild(ev[:n]); err != nil {
			t.Fatalf("prefix of %d events: %v", n, err)
		}
	}
	waiting, _ := Rebuild(ev[:3])
	if waiting.Status != StatusOpen || waiting.Requests[k].CallID != "c1" || waiting.ByKey[k] != "requested" {
		t.Fatalf("a request leaves the run open, waiting by key: %+v", waiting)
	}
	st, _ := Rebuild(ev)
	if st.ByKey[k] != "approved" || st.ByKey[k2] != "rejected" || len(st.Requests) != 0 || st.Denials != 1 || len(st.Effects) != 1 {
		t.Fatalf("decided by key, one effect, one refused attempt: %+v", st)
	}

	next := func(prefix []Event, e Event) []Event {
		e.Seq, e.V = len(prefix)+1, FormatVersion
		return append(append([]Event{}, prefix...), e)
	}
	for name, bad := range map[string][]Event{
		"asking again for a key already requested": next(next(ev[:3], Event{Type: EvCallReceived, CallID: "c9", Tool: "refund"}),
			Event{Type: EvApprovalRequested, CallID: "c9", Tool: "refund", Key: k}),
		"asking again for a rejected key": next(next(ev, Event{Type: EvCallReceived, CallID: "c9", Tool: "refund"}),
			Event{Type: EvApprovalRequested, CallID: "c9", Tool: "refund", Key: k2}),
		"deciding twice":                   next(ev[:8], Event{Type: EvApprovalDecided, CallID: "c1", Key: k, Decision: "rejected", By: "ops"}),
		"deciding while a call is running": next(ev[:4], Event{Type: EvApprovalDecided, CallID: "c1", Key: k, Decision: "approved", By: "ops"}),
		"a decision with no one making it": next(ev[:3], Event{Type: EvApprovalDecided, CallID: "c1", Key: k, Decision: "approved"}),
		"a decision naming the wrong call": next(ev[:3], Event{Type: EvApprovalDecided, CallID: "c2", Key: k, Decision: "approved", By: "ops"}),
		"a decision that's neither":        next(ev[:3], Event{Type: EvApprovalDecided, CallID: "c1", Key: k, Decision: "maybe", By: "ops"}),
	} {
		if _, err := Rebuild(bad); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s must be refused, got %v", name, err)
		}
	}
}
