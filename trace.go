package agentsafe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A trace is built FROM the event log, never recorded separately: the log is the source of truth, so the
// trace can't disagree with it, and the same log always gives the same trace (IDs are derived from events,
// not random). Span names and attributes follow the OpenTelemetry GenAI semantic conventions (status:
// Development); anything the conventions don't cover is namespaced agentsafe.*.
//
//	invoke_agent {name}         INTERNAL  the run (in-process agent)
//	├─ chat {model}             CLIENT    one model decision
//	├─ execute_tool {tool}      INTERNAL  one tool call: from first tool_started to its outcome
//	└─ approval {tool}          INTERNAL  a human decision: from approval_requested to approval_decided
//
// Timing: a chat span starts at the event before its model_decided (the runner's previous action), so it
// includes client-side pacing/retry waits. The log has no separate "request sent" event; that's a choice.

// Attr is one span attribute. Value is string, int, bool or []string.
type Attr struct {
	Key   string
	Value any
}

// SpanEvent is a timestamped note on a span (a retry, a pause, a budget extension).
type SpanEvent struct {
	Time  time.Time
	Name  string
	Attrs []Attr
}

// Span is one unit of work in the trace.
type Span struct {
	TraceID, SpanID, ParentID string
	Name                      string
	Kind                      int // OTLP: 1 internal, 3 client
	Start, End                time.Time
	Attrs                     []Attr
	Events                    []SpanEvent
	Status                    int // OTLP: 0 unset, 1 ok, 2 error
	StatusMsg                 string
	Step                      int  // the model step this span belongs to (0 = run level)
	Open                      bool // no outcome in the log: in progress, waiting, or crashed
}

// Trace is a run as spans. Spans[0] is the root.
type Trace struct{ Spans []Span }

const (
	kindInternal = 1
	kindClient   = 3
	statusOK     = 1
	statusError  = 2
)

func id(n int, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:])[:n]
}

// BuildTrace turns a run's events into a trace. agent names the root span (gen_ai.agent.name).
func BuildTrace(events []Event, agent string) (*Trace, error) {
	st, err := Rebuild(events) // a trace of an invalid log would be a confident lie: refuse it
	if err != nil {
		return nil, fmt.Errorf("log is not a valid run: %w", err)
	}
	if events, err = UpgradeAll(events); err != nil {
		return nil, err
	}
	if len(events) == 0 || events[0].Type != EvRunStarted {
		return nil, fmt.Errorf("log does not start with run_started")
	}
	first := events[0]
	traceID := id(32, "trace", first.Time.Format(time.RFC3339Nano), first.Task)
	rootID := id(16, traceID, "root")
	root := Span{TraceID: traceID, SpanID: rootID, Name: "invoke_agent " + agent, Kind: kindInternal,
		Start: first.Time, End: events[len(events)-1].Time, Attrs: []Attr{
			{"gen_ai.operation.name", "invoke_agent"},
			{"gen_ai.agent.name", agent},
			{"gen_ai.conversation.id", traceID},
			{"gen_ai.provider.name", first.Provider},
			{"gen_ai.request.model", first.Model},
			{"agentsafe.run.status", string(st.Status)},
			{"agentsafe.run.stop", st.Stop},
			{"agentsafe.run.steps", st.Step},
			{"agentsafe.run.budget", st.Budget},
			{"agentsafe.log.events", len(events)},
		}}
	switch st.Status {
	case StatusFinished:
		root.Status = statusOK
	case StatusPaused, StatusAwaitingApproval:
		root.Open, root.StatusMsg = true, "run is "+string(st.Status)
	default:
		root.Open, root.StatusMsg = true, "run has no terminal event: in progress or crashed"
	}

	var spans []Span
	open := map[string]int{} // call id (or "approval:"+call id) -> index in spans
	step := 0
	prev := first.Time
	child := func(seq int, name string, kind int, start time.Time, attrs ...Attr) int {
		spans = append(spans, Span{TraceID: traceID, SpanID: id(16, traceID, strconv.Itoa(seq), name),
			ParentID: rootID, Name: name, Kind: kind, Start: start, End: start, Attrs: attrs, Step: step, Open: true})
		return len(spans) - 1
	}

	for _, e := range events {
		switch e.Type {
		case EvRunStarted: // the root span, built above
		case EvModelDecided:
			step = e.Step
			var proposed []string
			for _, c := range e.Message.ToolCalls {
				proposed = append(proposed, c.Function.Name)
			}
			attrs := []Attr{{"gen_ai.operation.name", "chat"}, {"gen_ai.provider.name", first.Provider},
				{"gen_ai.request.model", first.Model}, {"gen_ai.response.finish_reasons", []string{e.FinishReason}},
				{"agentsafe.step", e.Step}, {"agentsafe.proposed_tools", proposed}}
			if e.Usage != nil {
				attrs = append(attrs, Attr{"gen_ai.usage.input_tokens", e.Usage.PromptTokens},
					Attr{"gen_ai.usage.output_tokens", e.Usage.CompletionTokens})
			}
			name := "chat " + first.Model // semconv: {gen_ai.operation.name} {gen_ai.request.model}
			if first.Model == "" {
				name = "chat" // logs from before run_started recorded the model
			}
			i := child(e.Seq, name, kindClient, prev, attrs...)
			spans[i].End, spans[i].Open, spans[i].Status = e.Time, false, statusOK

		case EvToolStarted:
			if i, ok := open[e.CallID]; ok { // a second start: the previous attempt died mid-call (week 3 W3-4)
				spans[i].Events = append(spans[i].Events, SpanEvent{Time: e.Time,
					Name: "retry: previous attempt has no logged outcome and may have executed"})
				setAttr(&spans[i], "agentsafe.attempts", attrInt(spans[i], "agentsafe.attempts")+1)
				break
			}
			open[e.CallID] = child(e.Seq, "execute_tool "+e.Tool, kindInternal, e.Time,
				Attr{"gen_ai.operation.name", "execute_tool"}, Attr{"gen_ai.tool.name", e.Tool},
				Attr{"gen_ai.tool.call.id", e.CallID}, Attr{"gen_ai.tool.type", "function"},
				Attr{"agentsafe.idempotency.key", e.Key}, Attr{"agentsafe.attempts", 1})

		case EvToolResult:
			i, ok := open[e.CallID]
			if !ok {
				return nil, fmt.Errorf("tool_result %s without an open span", e.CallID)
			}
			delete(open, e.CallID)
			outcome := outcomeOf(e)
			spans[i].End, spans[i].Open = e.Time, false
			setAttr(&spans[i], "agentsafe.outcome", outcome)
			if outcome == "error" || outcome == "conflict" {
				spans[i].Status, spans[i].StatusMsg = statusError, errorText(e.Result)
			} else {
				spans[i].Status = statusOK
			}

		case EvToolRefused: // never attempted: failed validation, or rejected (its approval span is already closed)
			j := child(e.Seq, "execute_tool "+e.Tool, kindInternal, e.Time,
				Attr{"gen_ai.operation.name", "execute_tool"}, Attr{"gen_ai.tool.name", e.Tool},
				Attr{"gen_ai.tool.call.id", e.CallID}, Attr{"gen_ai.tool.type", "function"},
				Attr{"agentsafe.outcome", "refused"}, Attr{"agentsafe.attempts", 0})
			spans[j].Open, spans[j].Status, spans[j].StatusMsg = false, statusError, errorText(e.Result)

		case EvApprovalRequested:
			open["approval:"+e.CallID] = child(e.Seq, "approval "+e.Tool, kindInternal, e.Time,
				Attr{"agentsafe.approval.key", e.Key}, Attr{"agentsafe.approval.summary", e.Summary})

		case EvApprovalDecided:
			if i, ok := open["approval:"+e.CallID]; ok {
				delete(open, "approval:"+e.CallID)
				spans[i].End, spans[i].Open, spans[i].Status = e.Time, false, statusOK
				setAttr(&spans[i], "agentsafe.approval.decision", e.Decision)
				setAttr(&spans[i], "agentsafe.approval.by", e.By)
				if e.Reason != "" {
					setAttr(&spans[i], "agentsafe.approval.reason", e.Reason)
				}
			}

		case EvApprovalDenied:
			if i, ok := open["approval:"+e.CallID]; ok {
				spans[i].Events = append(spans[i].Events, SpanEvent{Time: e.Time, Name: "denied: " + e.Decision + " by " + e.By,
					Attrs: []Attr{{"agentsafe.approval.denied_reason", e.Reason}}})
			}
		case EvRunPaused:
			root.Events = append(root.Events, SpanEvent{Time: e.Time, Name: "paused: " + e.Reason})
		case EvBudgetExtended:
			root.Events = append(root.Events, SpanEvent{Time: e.Time, Name: "budget_extended",
				Attrs: []Attr{{"agentsafe.extra_steps", e.ExtraSteps}, {"agentsafe.by", e.By}}})
		case EvRunFinished:
			root.Events = append(root.Events, SpanEvent{Time: e.Time, Name: "finished: " + e.Stop})
		}
		prev = e.Time
	}
	for _, i := range open { // started, no outcome: exactly the "maybe executed" state, made visible
		spans[i].End = root.End
		spans[i].StatusMsg = "no outcome in the log: waiting, in progress, or crashed mid-call"
	}
	sort.SliceStable(spans, func(a, b int) bool { return spans[a].Start.Before(spans[b].Start) })
	return &Trace{Spans: append([]Span{root}, spans...)}, nil
}

func outcomeOf(e Event) string {
	if e.Replayed {
		if strings.Contains(e.Result, "conflict") {
			return "conflict"
		}
		return "replayed"
	}
	var m map[string]any
	if json.Unmarshal([]byte(e.Result), &m) == nil {
		if _, isErr := m["error"]; isErr {
			return "error"
		}
		if m["already_recorded"] == true {
			return "deduplicated_by_tool" // the key reached the tool after a crash, and the tool caught it
		}
	}
	return "ok"
}

func errorText(result string) string {
	var m map[string]any
	if json.Unmarshal([]byte(result), &m) == nil {
		if s, ok := m["error"].(string); ok {
			return s
		}
	}
	return result
}

func setAttr(s *Span, k string, v any) {
	for i := range s.Attrs {
		if s.Attrs[i].Key == k {
			s.Attrs[i].Value = v
			return
		}
	}
	s.Attrs = append(s.Attrs, Attr{k, v})
}

func attrInt(s Span, k string) int {
	for _, a := range s.Attrs {
		if a.Key == k {
			if n, ok := a.Value.(int); ok {
				return n
			}
		}
	}
	return 0
}

// ---- OTLP/JSON export ------------------------------------------------------------------------------------

// OTLPJSON encodes the trace as an OTLP ExportTraceServiceRequest (JSON), loadable by any OTLP collector:
// e.g. curl -X POST localhost:4318/v1/traces -H 'Content-Type: application/json' --data @trace.json
func (t *Trace) OTLPJSON(service string) ([]byte, error) {
	type kv = map[string]any
	val := func(v any) kv {
		switch x := v.(type) {
		case string:
			return kv{"stringValue": x}
		case int:
			return kv{"intValue": strconv.Itoa(x)} // int64 is a string in OTLP/JSON
		case bool:
			return kv{"boolValue": x}
		case []string:
			vals := []kv{}
			for _, s := range x {
				vals = append(vals, kv{"stringValue": s})
			}
			return kv{"arrayValue": kv{"values": vals}}
		}
		return kv{"stringValue": fmt.Sprint(v)}
	}
	attrs := func(as []Attr) []kv {
		out := []kv{}
		for _, a := range as {
			if s, ok := a.Value.(string); ok && s == "" {
				continue
			}
			out = append(out, kv{"key": a.Key, "value": val(a.Value)})
		}
		return out
	}
	ns := func(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }
	var spans []kv
	for _, s := range t.Spans {
		sp := kv{"traceId": s.TraceID, "spanId": s.SpanID, "name": s.Name, "kind": s.Kind,
			"startTimeUnixNano": ns(s.Start), "endTimeUnixNano": ns(s.End), "attributes": attrs(s.Attrs),
			"status": kv{"code": s.Status, "message": s.StatusMsg}}
		if s.ParentID != "" {
			sp["parentSpanId"] = s.ParentID
		}
		var evs []kv
		for _, e := range s.Events {
			evs = append(evs, kv{"timeUnixNano": ns(e.Time), "name": e.Name, "attributes": attrs(e.Attrs)})
		}
		if evs != nil {
			sp["events"] = evs
		}
		spans = append(spans, sp)
	}
	return json.MarshalIndent(kv{"resourceSpans": []kv{{
		"resource":   kv{"attributes": attrs([]Attr{{"service.name", service}})},
		"scopeSpans": []kv{{"scope": kv{"name": "github.com/Ashutosh2308Bhardwaj/agentsafe"}, "spans": spans}},
	}}}, "", "  ")
}

// ---- human view ------------------------------------------------------------------------------------------

// Tree renders the trace for a terminal: one line per span, tool calls indented under the step that
// proposed them, with the facts a reviewer needs to reconstruct the run.
func (t *Trace) Tree() string {
	var b strings.Builder
	r := t.Spans[0]
	fmt.Fprintf(&b, "%s  %s  [%s %s, steps %v/%v, %v events]\n", r.Name, dur(r), get(r, "agentsafe.run.status"),
		get(r, "agentsafe.run.stop"), get(r, "agentsafe.run.steps"), get(r, "agentsafe.run.budget"), get(r, "agentsafe.log.events"))
	for _, s := range t.Spans[1:] {
		indent, line := "├─ ", ""
		switch {
		case s.Name == "chat" || strings.HasPrefix(s.Name, "chat "):
			tokens := "" // providers that don't report usage (and the scripted model) show none, not blanks
			if in := get(s, "gen_ai.usage.input_tokens"); in != nil && in != "" {
				tokens = fmt.Sprintf("  %v in / %v out", in, get(s, "gen_ai.usage.output_tokens"))
			}
			line = fmt.Sprintf("step %v  %s  %s%s  → %v", get(s, "agentsafe.step"), s.Name, dur(s), tokens,
				list(get(s, "agentsafe.proposed_tools"), get(s, "gen_ai.response.finish_reasons")))
		case strings.HasPrefix(s.Name, "execute_tool "):
			indent = "│   └─ "
			line = fmt.Sprintf("%s  %s  %v", s.Name, dur(s), get(s, "agentsafe.outcome"))
			if n := attrInt(s, "agentsafe.attempts"); n > 1 {
				line += fmt.Sprintf("  ⚠ %d attempts (crash mid-call)", n)
			}
			if s.StatusMsg != "" {
				line += "  — " + truncate(s.StatusMsg, 90)
			}
		case strings.HasPrefix(s.Name, "approval "):
			line = fmt.Sprintf("⏸ %s  waited %s  %v by %v", s.Name, dur(s), get(s, "agentsafe.approval.decision"), get(s, "agentsafe.approval.by"))
		default:
			line = s.Name
		}
		if s.Open {
			line += "  [OPEN: " + s.StatusMsg + "]"
		}
		b.WriteString(indent + line + "\n")
	}
	for _, e := range r.Events {
		fmt.Fprintf(&b, "•  %s  %s\n", e.Time.Format("15:04:05"), e.Name)
	}
	return b.String()
}

func get(s Span, k string) any {
	for _, a := range s.Attrs {
		if a.Key == k {
			return a.Value
		}
	}
	return ""
}

func list(proposed, finish any) string {
	if p, ok := proposed.([]string); ok && len(p) > 0 {
		return strings.Join(p, ", ")
	}
	if f, ok := finish.([]string); ok && len(f) > 0 {
		return "answer (" + f[0] + ")"
	}
	return "-"
}

func dur(s Span) string {
	d := s.End.Sub(s.Start)
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return d.Round(100 * time.Millisecond).String()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
