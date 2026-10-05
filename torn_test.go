package agentsafe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fullRunLog returns the bytes of a complete, real run log (scripted model, two tool calls, an answer).
func fullRunLog(t *testing.T) []byte {
	t.Helper()
	r, _, _ := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.Log.(*FileLog).Path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestTornAtEveryByte simulates a power cut at every byte of a real log. At each cut:
//   - Read returns exactly the events whose lines were complete (nothing more, nothing less);
//   - those events are still a valid run (a crash only ever leaves a valid prefix);
//   - the next Append cuts the torn tail off and writes a clean, correctly numbered line after it.
func TestTornAtEveryByte(t *testing.T) {
	data := fullRunLog(t)
	dir := t.TempDir()
	for cut := 0; cut <= len(data); cut++ {
		prefix := data[:cut]
		path := filepath.Join(dir, "cut.jsonl")
		if err := os.WriteFile(path, prefix, 0o600); err != nil {
			t.Fatal(err)
		}
		want := bytes.Count(prefix, []byte{'\n'}) // only newline-terminated lines were acknowledged
		events, err := (&FileLog{Path: path}).Read()
		if err != nil {
			t.Fatalf("cut at byte %d: a torn tail must not make the log unreadable: %v", cut, err)
		}
		if len(events) != want {
			t.Fatalf("cut at byte %d: want %d events, got %d", cut, want, len(events))
		}
		if _, err := Rebuild(events); err != nil {
			t.Fatalf("cut at byte %d: the surviving prefix must be a valid run: %v", cut, err)
		}

		writer := &FileLog{Path: path}
		if err := writer.Append(Event{Type: EvRunFinished, Stop: "marker"}); err != nil {
			t.Fatalf("cut at byte %d: append after a torn tail: %v", cut, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(after, data[:goodPrefix(prefix)]) {
			t.Fatalf("cut at byte %d: repair must keep every acknowledged byte", cut)
		}
		again, err := (&FileLog{Path: path}).Read()
		if err != nil || len(again) != want+1 || again[want].Seq != want+1 || again[want].Stop != "marker" {
			t.Fatalf("cut at byte %d: the new event must follow the acknowledged ones cleanly: %v %d", cut, err, len(again))
		}
	}
}

// goodPrefix is the length of the newline-terminated part of b.
func goodPrefix(b []byte) int {
	return bytes.LastIndexByte(b, '\n') + 1
}

func TestZeroFilledTailIsTorn(t *testing.T) {
	// Some filesystems extend the file before the data reaches disk, so a power cut leaves zero bytes.
	data := append(fullRunLog(t), []byte("\x00\x00\x00\x00\n")...)
	path := filepath.Join(t.TempDir(), "z.jsonl")
	_ = os.WriteFile(path, data, 0o600)
	events, err := (&FileLog{Path: path}).Read()
	if err != nil || len(events) != bytes.Count(data, []byte{'\n'})-1 {
		t.Fatalf("a zero-filled last line is a torn write: %v %d", err, len(events))
	}
}

func TestDamageInTheMiddleIsRefused(t *testing.T) {
	data := fullRunLog(t)
	lines := bytes.SplitAfter(data, []byte{'\n'})
	lines[2] = []byte("{this is not json}\n") // acknowledged history, with later lines after it
	path := filepath.Join(t.TempDir(), "mid.jsonl")
	_ = os.WriteFile(path, bytes.Join(lines, nil), 0o600)

	if _, err := (&FileLog{Path: path}).Read(); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("damage before valid lines must be refused, got %v", err)
	}
	before, _ := os.ReadFile(path)
	if err := (&FileLog{Path: path}).Append(Event{Type: EvRunFinished}); !errors.Is(err, ErrCorruptLog) {
		t.Fatalf("a writer must not append to (or truncate) a corrupt log, got %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("a corrupt log must be left exactly as found, for investigation")
	}
}
