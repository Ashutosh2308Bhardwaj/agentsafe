package agentsafe

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Sealing keeps sensitive content out of the log at rest, without losing the run.
//
// The log can't simply be redacted: it is what a crashed run resumes from, so a masked account number in the
// log is a masked account number handed back to the model (and to the payout) on resume. Instead, the content
// fields of each event are encrypted into one "sealed" field, and decrypted when the log is read. This is the
// approach of Temporal's payload codecs: history stores ciphertext; the worker, which holds the key, decrypts.
//
//	sealed (ciphertext)   system, task, message, args, result, summary, text, reason
//	plain  (the audit)    v, seq, prev, type, time, call_id, tool, key, payload_hash, replayed, by, decision,
//	                      step, usage, finish_reason, stop, max_steps, extra_steps, provider, model
//
// So without the key you can still see that a payout was proposed, approved by whom, executed once, and that
// the history is intact (the hash chain covers the stored, sealed line); you can't see whom it paid or how much.
// Deleting a key erases every run sealed with it (crypto-shredding) while that audit skeleton survives.
//
// Limits: key and payload_hash are SHA-256 of business fields. A low-entropy field (a small amount, a short
// reference) can be guessed by hashing candidates. By is an identity, kept plain for audit.

// A Codec encrypts and decrypts a sealed event's content. aad is the event's position (seq, type, call id):
// a Codec should authenticate it, so a sealed payload moved onto another line fails to open.
type Codec interface {
	Seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	Open(ctx context.Context, ciphertext, aad []byte) ([]byte, error)
}

// ErrSealed is returned when a sealed event reaches code that would need its content: a Log read without
// the Codec, or Rebuild of events that weren't opened. It is refused, never read as empty fields: an event
// whose result reads as "" is a different history.
var ErrSealed = errors.New("agentsafe: event is sealed; read the log with its Codec")

// ErrCannotOpen is returned when a sealed event can't be opened: wrong or deleted key, or a modified payload.
var ErrCannotOpen = errors.New("agentsafe: sealed event can't be opened")

// sealedContent is what gets encrypted: every field that can carry tool or model content.
type sealedContent struct {
	System  string   `json:"system,omitempty"`
	Task    string   `json:"task,omitempty"`
	Message *Message `json:"message,omitempty"`
	Args    string   `json:"args,omitempty"`
	Result  string   `json:"result,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Meta    string   `json:"meta,omitempty"`
	Text    string   `json:"text,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

func aadOf(e Event) []byte {
	return fmt.Appendf(nil, "agentsafe/sealed/%d/%s/%s", e.Seq, e.Type, e.CallID)
}

// SealEvent moves e's content into e.Sealed, encrypted by c. Seq, Type and CallID must be final (they're
// authenticated). Storage backends call it before writing; an event with no content is left as it is.
func SealEvent(ctx context.Context, e Event, c Codec) (Event, error) {
	if e.Sealed != "" {
		return e, errors.New("agentsafe: event is already sealed")
	}
	sc := sealedContent{System: e.System, Task: e.Task, Message: e.Message, Args: e.Args, Result: e.Result,
		Summary: e.Summary, Text: e.Text, Reason: e.Reason, Meta: e.Meta}
	if sc == (sealedContent{}) {
		return e, nil
	}
	plain, err := json.Marshal(sc)
	if err != nil {
		return e, err
	}
	ct, err := c.Seal(ctx, plain, aadOf(e))
	if err != nil {
		return e, fmt.Errorf("sealing event %d: %w", e.Seq, err)
	}
	e.System, e.Task, e.Message, e.Args, e.Result, e.Summary, e.Text, e.Reason = "", "", nil, "", "", "", "", ""
	e.Meta = ""
	e.Sealed = base64.StdEncoding.EncodeToString(ct)
	return e, nil
}

// OpenEvent restores a sealed event's content. An unsealed event is returned as it is.
func OpenEvent(ctx context.Context, e Event, c Codec) (Event, error) {
	if e.Sealed == "" {
		return e, nil
	}
	if c == nil {
		return e, fmt.Errorf("%w (event %d)", ErrSealed, e.Seq)
	}
	ct, err := base64.StdEncoding.DecodeString(e.Sealed)
	if err != nil {
		return e, fmt.Errorf("%w: event %d: %w", ErrCannotOpen, e.Seq, err)
	}
	plain, err := c.Open(ctx, ct, aadOf(e))
	if err != nil {
		return e, fmt.Errorf("%w: event %d: %w", ErrCannotOpen, e.Seq, err)
	}
	var sc sealedContent
	if err := json.Unmarshal(plain, &sc); err != nil {
		return e, fmt.Errorf("%w: event %d: %w", ErrCannotOpen, e.Seq, err)
	}
	e.System, e.Task, e.Message, e.Args, e.Result = sc.System, sc.Task, sc.Message, sc.Args, sc.Result
	e.Summary, e.Text, e.Reason, e.Meta, e.Sealed = sc.Summary, sc.Text, sc.Reason, sc.Meta, ""
	return e, nil
}

// AESGCM is a Codec using AES-256-GCM with a keyring, so keys can be rotated: new events are sealed with
// Current, and any key still in Keys opens what it sealed. Removing a key from Keys erases every event
// sealed with it. Keep keys off the log's host (a KMS, a secrets manager).
type AESGCM struct {
	Keys    map[string][]byte // key id -> 32-byte key
	Current string            // the id new events are sealed with
}

// Seal implements Codec. Output: len(id) | id | 12-byte random nonce | ciphertext+tag.
func (a AESGCM) Seal(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	if len(a.Current) == 0 || len(a.Current) > 255 {
		return nil, errors.New("agentsafe: AESGCM.Current must be a key id of 1-255 bytes")
	}
	g, err := a.gcm(a.Current)
	if err != nil {
		return nil, err
	}
	out := append([]byte{byte(len(a.Current))}, a.Current...) //nolint:gosec // G115: len checked to be 1-255 above
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)
	return g.Seal(out, nonce, plaintext, aad), nil
}

// Open implements Codec.
func (a AESGCM) Open(_ context.Context, ciphertext, aad []byte) ([]byte, error) {
	if len(ciphertext) < 1 || len(ciphertext) < 1+int(ciphertext[0]) {
		return nil, errors.New("truncated")
	}
	id, rest := string(ciphertext[1:1+ciphertext[0]]), ciphertext[1+ciphertext[0]:]
	g, err := a.gcm(id)
	if err != nil {
		return nil, err
	}
	if len(rest) < g.NonceSize() {
		return nil, errors.New("truncated")
	}
	return g.Open(nil, rest[:g.NonceSize()], rest[g.NonceSize():], aad)
}

func (a AESGCM) gcm(id string) (cipher.AEAD, error) {
	key, ok := a.Keys[id]
	if !ok {
		return nil, fmt.Errorf("no key %q (rotated out, or erased)", id)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key %q is %d bytes; AES-256 needs 32", id, len(key))
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
