package trace

import (
	"slices"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// contentAttrs are the trace attributes that carry tool or approval content.
var contentAttrs = []string{"agentsafe.approval.summary", "agentsafe.approval.reason", "agentsafe.approval.denied_reason"}

// Redact masks content in the trace before it's exported: approval summaries and reasons, denial reasons,
// and error messages (span status). Names, ids, keys, outcomes, timings and token counts are kept.
func (t *Trace) Redact(r agentsafe.Redactor) {
	for i := range t.Spans {
		s := &t.Spans[i]
		s.StatusMsg = r(s.StatusMsg)
		redactAttrs(s.Attrs, r)
		for j := range s.Events {
			redactAttrs(s.Events[j].Attrs, r)
		}
	}
}

func redactAttrs(attrs []Attr, r agentsafe.Redactor) {
	for i, a := range attrs {
		if v, ok := a.Value.(string); ok && slices.Contains(contentAttrs, a.Key) {
			attrs[i].Value = r(v)
		}
	}
}
