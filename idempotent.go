package agentsafe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// IdempotentTool is a tool with a side effect that must happen at most once per operation.
//
// The library does three things for it (week 2, now in one place, so no tool author can forget them):
//  1. derives a key from Identity(), the fields that DEFINE the operation. Never the model's call id
//     (it changes on retry, week 1 F9), never free text (a reworded note isn't a new operation);
//  2. before calling the tool, looks the key up in the run's log: same key + same payload → replay the
//     recorded result without calling anything; same key + different payload → conflict error, nothing runs;
//  3. passes the key INTO the tool. That covers the one case the log can't: a crash after the effect but
//     before its result was logged. The tool must make its effect idempotent on the key (a unique column
//     in your own database, or an Idempotency-Key header on a gateway). That's option (b) from week 2,
//     and it's what the transactional outbox needs for effects outside your database.
type IdempotentTool interface {
	Tool
	// Identity returns the operation's identity (hashed into the key) and its payload (compared on replay).
	Identity(args json.RawMessage) (identity, payload any, err error)
	// CallWithKey performs the effect at most once per key. On a repeated key it must return the original
	// outcome and must NOT perform the effect again.
	CallWithKey(ctx context.Context, key string, args json.RawMessage) (any, error)
}

// Canonical is the JSON used for hashing: object keys sorted, numbers normalised (4200 == 4200.0).
func Canonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var generic any // round-trip through `any`: maps marshal with sorted keys, numbers become float64
	if err := json.Unmarshal(b, &generic); err != nil {
		return "", err
	}
	out, err := json.Marshal(generic)
	return string(out), err
}

func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// keyFor returns the idempotency key and payload hash for a call to an IdempotentTool.
// scope separates unrelated batches (week 2: a hash of the input files).
func keyFor(scope string, t IdempotentTool, args json.RawMessage) (key, payloadHash string, err error) {
	identity, payload, err := t.Identity(args)
	if err != nil {
		return "", "", fmt.Errorf("identity: %w", err)
	}
	ci, err := Canonical(identity)
	if err != nil {
		return "", "", err
	}
	cp, err := Canonical(payload)
	if err != nil {
		return "", "", err
	}
	return hash(scope, t.Spec().Name, ci), hash(cp), nil
}

// replay builds the result the model sees for a repeated operation: the original result, marked.
func replay(original string) string {
	var m map[string]any
	if json.Unmarshal([]byte(original), &m) != nil {
		return original
	}
	m["already_recorded"] = true
	b, _ := json.Marshal(m)
	return string(b)
}

func conflict(key string) string {
	return errorJSON(fmt.Errorf("conflict: operation %s was already performed with DIFFERENT values; nothing was "+
		"done. Corrections need an explicit amendment, not a retry", key))
}
