package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Inspection is what Inspect found on a server, without calling any of its tools.
type Inspection struct {
	Server  string        `json:"server"`  // the name the server reports
	Version string        `json:"version"` // and its version
	Tools   []ToolSummary `json:"tools"`
}

// ToolSummary is one tool: what it says about itself, and the starting policy that follows from it.
type ToolSummary struct {
	Name string `json:"name"`
	// Write is true unless the tool says it's read-only (readOnlyHint). The hints are the server's word, not a
	// guarantee: MCP calls them untrusted.
	Write bool `json:"write"`
	// Annotated: the server sent annotations at all. Without them MCP's defaults apply: not read-only, destructive.
	Annotated   bool `json:"annotated"`
	Destructive bool `json:"destructive"` // destructiveHint, as MCP defaults it (true) for a write
	Idempotent  bool `json:"idempotent"`  // idempotentHint: same arguments twice, no further effect (the server's claim)
	// KeyArgument is a candidate: an argument named like an idempotency key (idempotency_key, request_id,
	// client_token...). A name isn't proof the server deduplicates on it, so the suggested policy doesn't use it:
	// verify it (agentsafe-mcp verify with key: argument), then switch the policy to it.
	KeyArgument string `json:"key_argument,omitempty"`
	// Required are the arguments the schema requires: often, not always, what identifies an operation.
	Required []string `json:"required,omitempty"`
	// Policy is the suggested starting policy; nil means the tool stays hidden until someone writes one. It stays
	// a suggestion: read it before you use it.
	Policy *Policy `json:"policy"`
	Note   string  `json:"note,omitempty"`
}

// keyNames are argument names, lowercased without _ and -, that APIs use for an idempotency key.
var keyNames = []string{"idempotencykey", "idempotencytoken", "requestid", "clientrequestid", "clienttoken",
	"dedupkey", "dedupekey", "deduplicationkey", "deduplicationid"}

// InspectOption changes what Inspect suggests.
type InspectOption func(*inspectOptions)

type inspectOptions struct{ trustAnnotations bool }

// TrustAnnotations lets the server's hints relax the suggestion: a tool marked read-only passes, and a write
// marked not destructive needs no approval. MCP calls the hints untrusted (a buggy or hostile server can mark a
// write read-only), so it's for a server you trust.
func TrustAnnotations() InspectOption { return func(o *inspectOptions) { o.trustAnnotations = true } }

// Inspect lists the upstream's tools and, for each, what it says about itself and a starting policy. It calls no
// tool, so it's safe against any server.
//
// The suggestion fails closed. By default a hint can only make it more careful, since the server's word isn't
// proof; only TrustAnnotations lets hints relax it. A write
// is identified by all its arguments (identity ["*"]), gets no key, and waits for approval. A tool marked
// read-only gets no policy, so it stays hidden until someone reviews it (TrustAnnotations: it passes, and a
// write marked not destructive needs no approval). No key, even for an argument named like one: a key lets
// agentsafe retry an outcome it lost, which is safe only if the server deduplicates on it, and a name doesn't
// say that. The argument is reported as a candidate for agentsafe-mcp verify.
//
// All arguments, not the required ones, because a schema can't say what makes a call one operation: required
// arguments can be too few (a generic make_api_request(service, method, request) would make every payment the
// same operation, refusing the second). With all of them an exact repeat is answered from the log and nothing
// legitimate is refused; naming the identity fields by hand adds conflict detection (the same ticket with a new
// amount).
func Inspect(ctx context.Context, upstream *sdk.ClientSession, opts ...InspectOption) (Inspection, error) {
	var o inspectOptions
	for _, opt := range opts {
		opt(&o)
	}
	var in Inspection
	if r := upstream.InitializeResult(); r != nil && r.ServerInfo != nil {
		in.Server, in.Version = r.ServerInfo.Name, r.ServerInfo.Version
	}
	for t, err := range upstream.Tools(ctx, nil) {
		if err != nil {
			return in, fmt.Errorf("listing the upstream's tools: %w", err)
		}
		in.Tools = append(in.Tools, summarize(t, o))
	}
	return in, nil
}

func summarize(t *sdk.Tool, o inspectOptions) ToolSummary {
	s := ToolSummary{Name: t.Name, Write: true, Destructive: true, Annotated: t.Annotations != nil}
	if a := t.Annotations; a != nil {
		s.Write = !a.ReadOnlyHint
		s.Destructive = s.Write && (a.DestructiveHint == nil || *a.DestructiveHint)
		s.Idempotent = s.Write && a.IdempotentHint
	}
	if !s.Write {
		if o.trustAnnotations {
			s.Policy, s.Note = &Policy{Pass: true}, "read-only, as the server says (annotations trusted)"
		} else {
			s.Note = "read-only by the server's word: hidden until you review it, then \"pass\": true"
		}
		return s
	}
	s.KeyArgument = candidateKey(schemaProperties(t.InputSchema))
	for _, r := range schemaRequired(t.InputSchema) {
		if r != s.KeyArgument {
			s.Required = append(s.Required, r)
		}
	}
	p := &Policy{Identity: []string{AllArguments}, Key: KeyNone}
	if s.KeyArgument != "" {
		s.Note = "candidate key " + s.KeyArgument + ": verify the server deduplicates on it (agentsafe-mcp verify), " +
			"then set key: argument. Until then an outcome lost to a crash or timeout is unknown, never retried"
	} else {
		s.Note = "no key argument: an outcome lost to a crash or timeout is recorded as unknown, never retried"
	}
	if s.Destructive || !o.trustAnnotations {
		p.Approval = "always"
	}
	s.Policy = p
	return s
}

// candidateKey is the argument named like an idempotency key (the first by name, if several), or "".
func candidateKey(props map[string]bool) string {
	var key string
	for name := range props {
		if isKeyName(name) && (key == "" || name < key) {
			key = name
		}
	}
	return key
}

func isKeyName(name string) bool {
	n := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
	return slices.Contains(keyNames, n)
}

// schemaRequired is the schema's required arguments, sorted.
func schemaRequired(schema any) []string {
	var s struct {
		Required []string `json:"required"`
	}
	if b, err := json.Marshal(schema); err != nil || json.Unmarshal(b, &s) != nil {
		return nil
	}
	slices.Sort(s.Required)
	return s.Required
}

// Starter is a policy file from an inspection: a policy for each tool that has a suggestion, and approvers. A
// tool without one stays hidden.
func (in Inspection) Starter(approvers ...string) Config {
	c := Config{Approvers: approvers, Tools: map[string]Policy{}}
	for _, t := range in.Tools {
		if t.Policy != nil {
			c.Tools[t.Name] = *t.Policy
		}
	}
	return c
}
