package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The outbox as people run it: each step a new process, a real SQLite file, a real email API, real kills.
func TestOutboxScenarios(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "outbox")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	type step struct {
		args   []string
		exit   bool // killed, or a failed reconciliation
		output []string
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"no faults", []step{
			{args: nil, output: []string{"finished", "1 credit note(s); email API: 0 email(s)"}},
			{args: []string{"worker"}, output: []string{"1 credit note(s); email API: 1 email(s)"}},
			{args: []string{"check"}, output: []string{"reconciliation: PASS"}},
		}},
		{"killed after the commit and after the send: one credit, one email", []step{
			{args: []string{"-crash"}, exit: true, output: []string{"killed right after the credit committed"}},
			{args: []string{"resume"}, output: []string{"nothing written", "finished", "1 credit note(s)"}},
			{args: []string{"-crash", "worker"}, exit: true, output: []string{"killed right after the email API accepted"}},
			{args: []string{"worker"}, output: []string{"1 credit note(s); email API: 1 email(s)"}},
			{args: []string{"check"}, output: []string{"reconciliation: PASS"}},
		}},
		{"control: no key, same crashes: two credits, three emails, and reconciliation fails", []step{
			{args: []string{"-no-key", "-crash"}, exit: true},
			{args: []string{"-no-key", "resume"}, output: []string{"2 credit note(s)"}},
			{args: []string{"-no-key", "-crash", "worker"}, exit: true},
			{args: []string{"-no-key", "worker"}, output: []string{"2 credit note(s); email API: 3 email(s)"}},
			{args: []string{"-no-key", "check"}, exit: true, output: []string{"reconciliation: FAIL", "credit:1001 exists 2 times"}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "demo")
			for _, s := range c.steps {
				b, err := exec.Command(bin, append([]string{"-dir", dir}, s.args...)...).CombinedOutput()
				out := string(b)
				var exit *exec.ExitError
				if s.exit != errors.As(err, &exit) {
					t.Fatalf("%v: want non-zero exit %v, err=%v\n%s", s.args, s.exit, err, out)
				}
				for _, want := range s.output {
					if !strings.Contains(out, want) {
						t.Fatalf("%v: want %q in:\n%s", s.args, want, out)
					}
				}
			}
		})
	}
}
