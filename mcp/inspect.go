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
	// KeyArgument is an argument that looks like an idempotency key (idempotency_key, request_id, client_token...):
	// a server that deduplicates on it lets agentsafe retry safely. A name is a hint, not proof: run verify.
	KeyArgument string `json:"key_argument,omitempty"`
	// Required are the arguments the schema requires: the starting guess for the operation's identity.
	Required []string `json:"required,omitempty"`
	// Policy is the suggested starting policy, nil when none can be guessed (a write with no required arguments:
	// its identity has to be written by hand). It stays a suggestion: read it before you use it.
	Policy *Policy `json:"policy,omitempty"`
	Note   string  `json:"note,omitempty"`
}

// keyNames are argument names, lowercased without _ and -, that APIs use for an idempotency key.
var keyNames = []string{"idempotencykey", "idempotencytoken", "requestid", "clientrequestid", "clienttoken",
	"dedupkey", "dedupekey", "deduplicationkey", "deduplicationid"}

// Inspect lists the upstream's tools and, for each, what it says about itself and a starting policy. It calls no
// tool, so it's safe against any server.
//
// The suggestion fails closed: a read-only tool passes; a write is keyed on its required arguments (and on its
// key argument, if it has one) and waits for approval if it may be destructive, as MCP assumes unless told
// otherwise; a write with no required arguments gets no policy, and stays hidden until someone writes one.
func Inspect(ctx context.Context, upstream *sdk.ClientSession) (Inspection, error) {
	var in Inspection
	if r := upstream.InitializeResult(); r != nil && r.ServerInfo != nil {
		in.Server, in.Version = r.ServerInfo.Name, r.ServerInfo.Version
	}
	for t, err := range upstream.Tools(ctx, nil) {
		if err != nil {
			return in, fmt.Errorf("listing the upstream's tools: %w", err)
		}
		in.Tools = append(in.Tools, summarize(t))
	}
	return in, nil
}

func summarize(t *sdk.Tool) ToolSummary {
	s := ToolSummary{Name: t.Name, Write: true, Destructive: true, Annotated: t.Annotations != nil}
	if a := t.Annotations; a != nil {
		s.Write = !a.ReadOnlyHint
		s.Destructive = s.Write && (a.DestructiveHint == nil || *a.DestructiveHint)
		s.Idempotent = s.Write && a.IdempotentHint
	}
	if !s.Write {
		s.Policy, s.Note = &Policy{Pass: true}, "read-only, as the server says"
		return s
	}
	props, required := schemaProperties(t.InputSchema), schemaRequired(t.InputSchema)
	for name := range props {
		if isKeyName(name) && (s.KeyArgument == "" || name < s.KeyArgument) {
			s.KeyArgument = name
		}
	}
	for _, r := range required {
		if r != s.KeyArgument {
			s.Required = append(s.Required, r)
		}
	}
	if len(s.Required) == 0 {
		s.Note = "a write with no required arguments: write its identity by hand (it stays hidden until then)"
		return s
	}
	p := &Policy{Identity: s.Required, Key: KeyNone}
	if s.KeyArgument != "" {
		p.Key, p.KeyArgument = KeyArgument, s.KeyArgument
		s.Note = "has a key argument: confirm the server deduplicates on it (agentsafe-mcp verify)"
	} else {
		s.Note = "no key argument: an outcome lost to a crash or timeout is recorded as unknown, never retried"
	}
	if s.Destructive {
		p.Approval = "always"
	}
	s.Policy = p
	return s
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

// Starter is a policy file from an inspection: a policy for each tool that has a suggestion, and approvers.
func (in Inspection) Starter(approvers ...string) Config {
	c := Config{Approvers: approvers, Tools: map[string]Policy{}}
	for _, t := range in.Tools {
		if t.Policy != nil {
			c.Tools[t.Name] = *t.Policy
		}
	}
	return c
}
