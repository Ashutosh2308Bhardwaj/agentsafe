package agentsafe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// faultyFile is a real file that fails on command, as a full disk, a failing device or a network filesystem
// would.
type faultyFile struct {
	file
	failWrite, halfWrite, failSync, failClose, failTruncate bool
}

func (f *faultyFile) Write(b []byte) (int, error) {
	if f.halfWrite {
		n, _ := f.file.Write(b[:len(b)/2]) // part of the line reaches the disk, then the device fills up
		return n, syscall.ENOSPC
	}
	if f.failWrite {
		return 0, syscall.EIO
	}
	return f.file.Write(b)
}
func (f *faultyFile) Sync() error {
	if f.failSync {
		return syscall.EIO
	}
	return f.file.Sync()
}
func (f *faultyFile) Close() error {
	err := f.file.Close()
	if f.failClose {
		return syscall.EIO
	}
	return err
}
func (f *faultyFile) Truncate(n int64) error {
	if f.failTruncate {
		return syscall.EIO
	}
	return f.file.Truncate(n)
}

// faultOnce makes the store's next opened file fail one way; later opens are healthy.
func faultOnce(s *fileStore, fault faultyFile) {
	used := false
	s.open = func(name string, flag int, perm os.FileMode) (file, error) {
		f, err := osOpen(name, flag, perm)
		if err != nil || used {
			return f, err
		}
		used = true
		ff := fault
		ff.file = f
		return &ff, nil
	}
}

func appendN(t *testing.T, s *fileStore, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		if err := s.AppendLine(context.Background(), i, line("l"+string(rune('0'+i)))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func line(s string) []byte { return []byte(`{"v":"` + s + `"}`) }

func TestFailedWritesPoisonTheStore(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		fault     faultyFile
		lineAfter bool // is the failed line on disk afterwards?
	}{
		"fsync fails":              {faultyFile{failSync: true}, true},  // the bytes reached the file; durability unknown
		"close fails after sync":   {faultyFile{failClose: true}, true}, // durable, but the store can't know that
		"write fails":              {faultyFile{failWrite: true}, false},
		"half a line, then ENOSPC": {faultyFile{halfWrite: true}, false}, // a torn tail
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "r.jsonl")
			s := newFileStore(path)
			appendN(t, s, 1, 2)
			faultOnce(s, tc.fault)
			if err := s.AppendLine(ctx, 3, line("in doubt")); err == nil {
				t.Fatal("the failed write must be reported")
			}
			// The same store writes nothing more: line 4 on top of an uncertain line 3 could leave a gap.
			if err := s.AppendLine(ctx, 3, line("retry")); !errors.Is(err, ErrStorePoisoned) {
				t.Fatalf("a poisoned store must refuse, got %v", err)
			}
			// A new store (a new process) reads what is really there and continues from it.
			fresh := newFileStore(path)
			lines, err := fresh.ReadLines(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if tc.lineAfter {
				want = 3
			}
			if len(lines) != want {
				t.Fatalf("want %d lines on disk, got %d", want, len(lines))
			}
			if err := fresh.AppendLine(ctx, want+1, line("next")); err != nil {
				t.Fatalf("a new store must continue cleanly: %v", err)
			}
			again, _ := fresh.ReadLines(ctx)
			if len(again) != want+1 || !strings.Contains(string(again[want]), "next") {
				t.Fatalf("the log must be whole and in order: %q", again)
			}
		})
	}
}

func TestFailedRepairPoisonsTheStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	appendN(t, newFileStore(path), 1, 1)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"torn`)
	_ = f.Close()
	for name, fault := range map[string]faultyFile{"truncate": {failTruncate: true}, "sync": {failSync: true}, "close": {failClose: true}} {
		s := newFileStore(path)
		faultOnce(s, fault)
		if err := s.AppendLine(ctx, 2, line("x")); err == nil {
			t.Fatalf("%s: a failed repair must be reported", name)
		}
		if err := s.AppendLine(ctx, 2, line("x")); !errors.Is(err, ErrStorePoisoned) {
			t.Fatalf("%s: a failed repair must poison the store, got %v", name, err)
		}
	}
}

// End to end: fsync fails on the payout's tool_started. The payout doesn't run; a new process pays once.
func TestFsyncFailureBeforeAPayoutMeansNoPayout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	paid := 0
	pay := Func("pay", "", func(context.Context, struct {
		Ref string `json:"ref"`
	}) (string, error) {
		paid++
		return "paid", nil
	}, Idempotent("ref"))
	model := &ScriptedModel{Plan: []FunctionCall{{Name: "pay", Arguments: `{"ref":"R1"}`}}, Final: "done"}

	log := &FileLog{Path: path}
	store := log.journal().Store.(*fileStore)
	writes := 0
	store.open = func(name string, flag int, perm os.FileMode) (file, error) {
		f, err := osOpen(name, flag, perm)
		if err != nil || flag&os.O_APPEND == 0 {
			return f, err
		}
		writes++
		if writes == 3 { // run_started, model_decided, then tool_started
			return &faultyFile{file: f, failSync: true}, nil
		}
		return f, nil
	}
	r, _ := New(model, log, WithTools(pay))
	if _, err := r.Start(context.Background(), "sys", "task"); !errors.Is(err, syscall.EIO) {
		t.Fatalf("the fsync failure must stop the run: %v", err)
	}
	if paid != 0 {
		t.Fatalf("the payout ran although its tool_started may not be durable: paid=%d", paid)
	}
	if _, err := r.Continue(context.Background()); !errors.Is(err, ErrStorePoisoned) {
		t.Fatalf("the same log must refuse to continue: %v", err)
	}
	r2, _ := New(model, &FileLog{Path: path}, WithTools(pay))
	st, err := r2.Continue(context.Background())
	if err != nil || st.Status != StatusFinished || paid != 1 {
		t.Fatalf("a new process must resume and pay once: err=%v status=%s paid=%d", err, st.Status, paid)
	}
}
