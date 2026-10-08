package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Config is a policy file: who may approve, and how each tool is protected.
//
//	{
//	  "approvers": ["user:alice", "user:bob"],
//	  "tools": {
//	    "create_refund": {"identity": ["ticket_id", "charge_id"], "key": "argument",
//	                      "key_argument": "idempotency_key", "approval": "always", "timeout": "10s"}
//	  }
//	}
type Config struct {
	// Approvers are the identities allowed to decide approvals. agentsafe-mcp identifies a person by the OS
	// account running `agentsafe-mcp approve` ("user:" + username): give each approver their own account.
	Approvers []string          `json:"approvers"`
	Tools     map[string]Policy `json:"tools"`
}

// LoadConfig reads a policy file. Unknown fields are errors: a misspelt one would silently weaken a policy.
func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path) //nolint:gosec // the operator's own policy file
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
