package agentsafe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
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
	// outcome and must NOT perform the effect again. That includes CONCURRENT calls with one key: after a
	// timeout the runner retries while the first call may still be running (exec.go). Return an error
	// wrapping ErrOutcomeUnknown when you can't tell whether the effect happened (e.g. a 504).
	CallWithKey(ctx context.Context, key string, args json.RawMessage) (any, error)
}

// KeyHonouring is implemented by an IdempotentTool that knows whether the system it calls deduplicates on the
// key. A key does two jobs: the log answers an operation it saw finish (always), and a retry after an outcome
// the log couldn't see is safe (only if the downstream honours the key). A tool that reports false keeps the
// first job and gives up the second: an unknown outcome is never retried. The call is recorded as "outcome
// unknown" under its key, so asking for the operation again gets that answer instead of a second attempt.
//
// A tool that doesn't implement KeyHonouring honours its key: that's what IdempotentTool asks of CallWithKey.
// An MCP server with no way to receive a key is the case this exists for (agentsafe/mcp).
type KeyHonouring interface {
	HonoursKey() bool
}

func honoursKey(t Tool) bool {
	kh, ok := t.(KeyHonouring)
	return !ok || kh.HonoursKey()
}

// Canonical is the JSON used for hashing: object keys sorted, numbers normalised (4200 == 4200.0).
func Canonical(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	d := json.NewDecoder(bytes.NewReader(b)) // round-trip through `any`: maps marshal with sorted keys
	d.UseNumber()
	var generic any
	if err := d.Decode(&generic); err != nil {
		return "", err
	}
	if generic, err = exactNumbers(generic); err != nil {
		return "", err
	}
	out, err := json.Marshal(generic)
	return string(out), err
}

// exactNumbers makes every number canonical without changing its value. A number that round-trips through
// float64 keeps the form Canonical has always used, so existing keys stay the same; one that doesn't (an
// integer above 2^53, say) keeps its exact decimal text: rounded, two different invoice numbers could share
// one key, and the second payout would be replayed instead of made. Found by FuzzCanonicalKeepsNumbersExact.
func exactNumbers(v any) (any, error) {
	var err error
	switch t := v.(type) {
	case json.Number:
		return canonicalNumber(t)
	case map[string]any:
		for k, x := range t {
			if t[k], err = exactNumbers(x); err != nil {
				return nil, err
			}
		}
	case []any:
		for i, x := range t {
			if t[i], err = exactNumbers(x); err != nil {
				return nil, err
			}
		}
	}
	return v, nil
}

// maxExponent bounds a number's exponent: computing 1e-100000000 exactly would take gigabytes, and a model
// can send any literal. No amount or identifier needs more than float64's range.
const maxExponent = 400

func canonicalNumber(n json.Number) (any, error) {
	s := string(n)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		if exp, err := strconv.Atoi(s[i+1:]); err != nil || exp > maxExponent || exp < -maxExponent {
			return nil, fmt.Errorf("number %.40s is out of range", s)
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("number %.40s: %w", s, err)
	}
	exact, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("number %.40s isn't a decimal", s)
	}
	// Two different numbers collide only if they round to one float64, and then at least one of them differs
	// from the shortest decimal of that float64 (what json writes for it). So: equal to it -> the float64 form
	// keys have always used; different -> the exact text.
	shortest, _ := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if exact.Cmp(shortest) == 0 {
		return f, nil
	}
	return json.Number(decimalText(exact)), nil
}

// decimalText writes r (a decimal: its denominator is 2^a*5^b) with exactly the digits it needs.
func decimalText(r *big.Rat) string {
	places, den := 0, new(big.Int).Set(r.Denom())
	two, five, zero, m := big.NewInt(2), big.NewInt(5), big.NewInt(0), new(big.Int)
	for twos, fives := 0, 0; den.Cmp(big.NewInt(1)) > 0; {
		switch {
		case m.Mod(den, two).Cmp(zero) == 0:
			den.Div(den, two)
			twos++
		case m.Mod(den, five).Cmp(zero) == 0:
			den.Div(den, five)
			fives++
		default:
			return r.RatString() // not a decimal (can't happen for a parsed literal)
		}
		places = max(twos, fives)
	}
	return r.FloatString(places)
}

// KeyBits is the idempotency key length of new runs: 128 bits, 32 hex characters. A run keeps the length it
// started with (run_started.key_bits), so keys never change under a run that outlives a library upgrade.
// 128, not the full 256: the key is also the payment provider's Idempotency-Key, and some providers cap it
// near UUID length.
const KeyBits = 128

// hash is the first bits of the SHA-256 of parts, in hex. Lengths are prefixes of one digest, so a 64-bit
// key is the first 16 characters of the 128-bit key for the same operation.
func hash(bits int, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:bits/4]
}

// keyFor returns the idempotency key and payload hash for a call to an IdempotentTool, bits long (the run's
// KeyBits). scope separates unrelated batches (week 2: a hash of the input files).
func keyFor(scope string, t IdempotentTool, args json.RawMessage, bits int) (key, payloadHash string, err error) {
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
	return hash(bits, scope, t.Spec().Name, ci), hash(bits, cp), nil
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
