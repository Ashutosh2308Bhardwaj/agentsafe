package agentsafe

import (
	"encoding/json"
	"regexp"
	"slices"
)

// Redaction masks content on its way OUT of the process: console output (Runner.Logf) and exported traces
// (Trace.Redact). It is one-way and never touches the log: the log is what a run resumes from, and it is
// protected by sealing instead (seal.go).
//
//	the log (resume, audit)        sealed: encrypted, reversible with the key
//	console, traces, vendors       redacted: masked, irreversible

// A Redactor masks sensitive parts of one value: a tool's arguments, a result, an approval summary, or an
// error message. It returns the value to show. It must not fail: when unsure, mask more.
type Redactor func(s string) string

// Masked replaces every redacted value.
const Masked = "[REDACTED]"

// RedactFields masks the values of the named JSON keys at any depth, keeping the structure readable:
// {"payee":"Ramesh","amount":100} becomes {"payee":"[REDACTED]","amount":100}. A value that isn't JSON (an
// error message, plain text) is returned unchanged: pair it with RedactPattern for free text.
func RedactFields(keys ...string) Redactor {
	return func(s string) string {
		var v any
		if json.Unmarshal([]byte(s), &v) != nil {
			return s
		}
		changed := false
		v = maskKeys(v, keys, &changed)
		if !changed {
			return s // keep the original formatting when there was nothing to mask
		}
		b, err := json.Marshal(v)
		if err != nil {
			return Masked
		}
		return string(b)
	}
}

func maskKeys(v any, keys []string, changed *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if slices.Contains(keys, k) {
				t[k], *changed = Masked, true
			} else {
				t[k] = maskKeys(x, keys, changed)
			}
		}
	case []any:
		for i, x := range t {
			t[i] = maskKeys(x, keys, changed)
		}
	case string:
		// Tool arguments are a JSON string inside JSON (FunctionCall.Arguments): look inside.
		var inner any
		if json.Unmarshal([]byte(t), &inner) == nil {
			if _, isContainer := inner.(map[string]any); isContainer || isArray(inner) {
				c := false
				inner = maskKeys(inner, keys, &c)
				if c {
					b, err := json.Marshal(inner)
					if err != nil {
						return Masked
					}
					*changed = true
					return string(b)
				}
			}
		}
	}
	return v
}

func isArray(v any) bool { _, ok := v.([]any); return ok }

// RedactPattern masks every match of the patterns, in any text: e.g. account numbers in an error message,
// `\b\d{9,18}\b`.
func RedactPattern(patterns ...*regexp.Regexp) Redactor {
	return func(s string) string {
		for _, p := range patterns {
			s = p.ReplaceAllString(s, Masked)
		}
		return s
	}
}

// Redactors applies each Redactor in turn.
func Redactors(rs ...Redactor) Redactor {
	return func(s string) string {
		for _, r := range rs {
			s = r(s)
		}
		return s
	}
}

// show is what the runner prints for a content value.
func (r *Runner) show(s string) string {
	if r.Redact == nil {
		return s
	}
	return r.Redact(s)
}

// contentAttrs are the trace attributes that carry tool or approval content.
var contentAttrs = []string{"agentsafe.approval.summary", "agentsafe.approval.reason", "agentsafe.approval.denied_reason"}

// Redact masks content in the trace before it's exported: approval summaries and reasons, denial reasons,
// and error messages (span status). Names, ids, keys, outcomes, timings and token counts are kept.
func (t *Trace) Redact(r Redactor) {
	for i := range t.Spans {
		s := &t.Spans[i]
		s.StatusMsg = r(s.StatusMsg)
		redactAttrs(s.Attrs, r)
		for j := range s.Events {
			redactAttrs(s.Events[j].Attrs, r)
		}
	}
}

func redactAttrs(attrs []Attr, r Redactor) {
	for i, a := range attrs {
		if v, ok := a.Value.(string); ok && slices.Contains(contentAttrs, a.Key) {
			attrs[i].Value = r(v)
		}
	}
}
