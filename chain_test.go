package agentsafe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ls := bytes.SplitAfter(data, []byte{'\n'}) // every line keeps its newline
	if len(ls) > 0 && len(ls[len(ls)-1]) == 0 {
		ls = ls[:len(ls)-1]
	}
	return ls
}

func write(t *testing.T, path string, ls [][]byte) {
	t.Helper()
	out := bytes.Join(ls, nil)
	if !bytes.HasSuffix(out, []byte{'\n'}) {
		out = append(out, '\n')
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func chainedRun(t *testing.T) string {
	t.Helper()
	r, _, _ := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	return r.Log.(*FileLog).Path
}

func TestEveryLineLinksToTheOneBefore(t *testing.T) {
	path := chainedRun(t)
	events, err := (&FileLog{Path: path}).Read()
	if err != nil {
		t.Fatal(err)
	}
	ls := lines(t, path)
	for i, e := range events {
		want := genesis
		if i > 0 {
			h := sha256.Sum256(lineContent(ls[i-1]))
			want = hex.EncodeToString(h[:])
		}
		if e.Prev != want {
			t.Fatalf("event %d: prev %q, want %q", e.Seq, e.Prev, want)
		}
	}
	head, _ := (&FileLog{Path: path}).Head()
	h := sha256.Sum256(lineContent(ls[len(ls)-1]))
	if head != hex.EncodeToString(h[:]) {
		t.Fatal("Head must be the hash of the last line")
	}
}

func TestTamperingIsDetected(t *testing.T) {
	attacks := map[string]func([][]byte) [][]byte{
		"edit a line": func(ls [][]byte) [][]byte { // line 2 is the model's first decision
			ls[1] = bytes.Replace(ls[1], []byte(`"step":1`), []byte(`"step":9`), 1)
			return ls
		},
		"delete a line": func(ls [][]byte) [][]byte { return append(ls[:3:3], ls[4:]...) },
		"swap two lines": func(ls [][]byte) [][]byte {
			ls[2], ls[3] = ls[3], ls[2]
			return ls
		},
		"insert a copy": func(ls [][]byte) [][]byte {
			return append(ls[:3:3], append([][]byte{ls[2]}, ls[3:]...)...)
		},
		"strip a link": func(ls [][]byte) [][]byte {
			var e map[string]any
			_ = json.Unmarshal(ls[3], &e)
			delete(e, "prev")
			b, _ := json.Marshal(e)
			ls[3] = append(b, '\n')
			return ls
		},
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			path := chainedRun(t)
			before, _ := os.ReadFile(path)
			write(t, path, attack(lines(t, path)))
			if after, _ := os.ReadFile(path); bytes.Equal(before, after) {
				t.Fatal("test bug: the attack didn't change the file")
			}
			if _, err := (&FileLog{Path: path}).Read(); !errors.Is(err, ErrTampered) {
				t.Fatalf("want ErrTampered, got %v", err)
			}
		})
	}
}

// pausedForApproval returns a keyed or unkeyed run waiting on a gated payout, and its tool.
func pausedForApproval(t *testing.T, key []byte) (*Runner, *payTool, State) {
	t.Helper()
	r, tool := payRun(t, `{"ref":"T1007","amount":11000}`)
	r.Log.(*FileLog).Key = key
	st, err := r.Start(context.Background(), "sys", "task")
	if err != nil || st.Status != StatusAwaitingApproval {
		t.Fatalf("setup: %v %s", err, st.Status)
	}
	return r, tool, st
}

// forgeApproval appends an approval_decided line the way an attacker without the key would: a correct seq
// and an unkeyed SHA-256 link to the last line.
func forgeApproval(t *testing.T, path string, st State) {
	t.Helper()
	ls := lines(t, path)
	h := sha256.Sum256(lineContent(ls[len(ls)-1]))
	forged, _ := json.Marshal(Event{V: FormatVersion, Seq: len(ls) + 1, Type: EvApprovalDecided, Time: time.Now().UTC(),
		Prev: hex.EncodeToString(h[:]), CallID: st.Waiting.CallID, Key: st.Waiting.Key, Decision: "approved", By: "cfo"})
	write(t, path, append(ls, append(forged, '\n')))
}

func TestForgedApprovalIsRefusedWithAKey(t *testing.T) {
	r, tool, st := pausedForApproval(t, []byte("secret-not-on-this-host"))
	forgeApproval(t, r.Log.(*FileLog).Path, st)
	r2 := &Runner{Model: r.Model, Tools: r.Tools, Log: &FileLog{Path: r.Log.(*FileLog).Path, Key: []byte("secret-not-on-this-host")}}
	if _, err := r2.Continue(context.Background()); !errors.Is(err, ErrTampered) {
		t.Fatalf("a forged approval must stop the run, got %v", err)
	}
	if tool.paid != 0 {
		t.Fatal("nothing may be paid on a forged approval")
	}
}

func TestWithoutAKeyAForgedTailNeedsAnAnchoredHead(t *testing.T) {
	// The documented limit: unkeyed, an attacker can append a correctly linked line. The chain alone can't
	// catch the LAST line; comparing against a head anchored elsewhere does.
	r, _, st := pausedForApproval(t, nil)
	path := r.Log.(*FileLog).Path
	anchored, err := (&FileLog{Path: path}).Head() // e.g. stored with the approval request in your database
	if err != nil {
		t.Fatal(err)
	}
	forgeApproval(t, path, st)
	events, err := (&FileLog{Path: path}).Read()
	if err != nil || events[len(events)-1].Type != EvApprovalDecided {
		t.Fatalf("expected the limitation to show (the forged tail reads as a valid approval without a key), got %v", err)
	}
	if now, _ := (&FileLog{Path: path}).Head(); now == anchored {
		t.Fatal("the head moved, so comparing with the anchored head must reveal the forged line")
	}
}

func TestLegacyUnchainedLogContinuesTheChain(t *testing.T) {
	data, err := os.ReadFile("testdata/golden_v0_real_gateway_crash.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	_ = os.WriteFile(path, data, 0o600)
	l := &FileLog{Path: path}
	if err := l.Append(Event{Type: EvRunFinished, Stop: "audit-note"}); err != nil {
		t.Fatal(err)
	}
	events, err := (&FileLog{Path: path}).Read()
	if err != nil {
		t.Fatalf("an unchained prefix followed by a chained line must read: %v", err)
	}
	ls := lines(t, path)
	h := sha256.Sum256(lineContent(ls[len(ls)-2]))
	if last := events[len(events)-1]; last.Prev != hex.EncodeToString(h[:]) {
		t.Fatal("the first chained line must link to the last legacy line")
	}
}

func TestReadingAKeyedLogWithoutTheKeyFails(t *testing.T) {
	r, _, _ := pausedForApproval(t, []byte("k1"))
	_, err := (&FileLog{Path: r.Log.(*FileLog).Path}).Read()
	if !errors.Is(err, ErrTampered) || !strings.Contains(err.Error(), "link") {
		t.Fatalf("a keyed log read with no or the wrong key can't be verified, got %v", err)
	}
}

func TestConvertedLineEndingsStillVerify(t *testing.T) {
	// git on Windows (core.autocrlf) rewrites "\n" to "\r\n" on checkout. A chained log must still verify,
	// and still continue the chain, after that.
	path := chainedRun(t)
	data, _ := os.ReadFile(path)
	_ = os.WriteFile(path, bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n")), 0o600)
	l := &FileLog{Path: path}
	if _, err := l.Read(); err != nil {
		t.Fatalf("CRLF line endings must not read as tampering: %v", err)
	}
	if err := l.Append(Event{Type: EvRunFinished, Stop: "after-crlf"}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileLog{Path: path}).Read(); err != nil {
		t.Fatalf("a line appended after CRLF conversion must link correctly: %v", err)
	}
}
