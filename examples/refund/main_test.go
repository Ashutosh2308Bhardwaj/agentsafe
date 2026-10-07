package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The refund as people run it: each step a new process, a real HTTP API, a real kill.
func TestRefundScenarios(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "refund")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	type step struct {
		args   []string
		exit   bool     // the process must exit non-zero (refused, or killed)
		output []string // must all appear
	}
	const none, once, twice = "refunded 0.00 of 4200.50; 0 refund(s)", "refunded 1200.00 of 4200.50; 1 refund(s)",
		"refunded 2400.00 of 4200.50; 2 refund(s)"
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"grounded, gated, refused, then approved, killed and resumed: one refund", []step{
			{args: nil, output: []string{"exceeds the 4200.50 left on ch_3QA", "waiting for finance to approve", none}},
			{args: []string{"approve", "intern@example.com"}, exit: true, output: []string{"not on the approver list", none}},
			{args: []string{"approve", "support@example.com"}, exit: true, output: []string{"started this run", none}},
			{args: []string{"-crash", "approve", "finance@example.com"}, exit: true, output: []string{"killed right after"}},
			{args: []string{"resume"}, output: []string{"started before a crash", "finished", once}},
			{args: []string{"resume"}, output: []string{"finished", once}}, // nothing left to do, nothing re-sent
		}},
		{"control: the key not sent, killed and resumed: two refunds", []step{
			{args: []string{"-no-key"}, output: []string{"waiting for finance to approve"}},
			{args: []string{"-no-key", "-crash", "approve", "finance@example.com"}, exit: true},
			{args: []string{"-no-key", "resume"}, output: []string{twice}},
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
