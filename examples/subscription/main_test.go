package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Each scenario as a user runs it: real processes, a real HTTP API, a real kill. The control (-no-key)
// proves the faults are real: without the key, the same faults bill twice.
func TestSubscriptionScenarios(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "subscription")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	const once, twice = "cus_42 has 15 seats; 1 invoice(s)", "cus_42 has 20 seats; 2 invoice(s)"
	for _, c := range []struct {
		name  string
		runs  [][]string // each run is a new process; a run listed with "!" first is expected to be killed
		want  string
		retry string // what the last run must say about the retry
	}{
		{"no fault", [][]string{{}}, once, ""},
		{"timeout after the change", [][]string{{"-slow"}}, once, "retrying with the same key"},
		{"killed after the change, then resumed", [][]string{{"!", "-crash"}, {"resume"}}, once, "started before a crash"},
		{"control: timeout, no key", [][]string{{"-no-key", "-slow"}}, twice, "retrying with the same key"},
		{"control: killed, no key", [][]string{{"!", "-no-key", "-crash"}, {"-no-key", "resume"}}, twice, "started before a crash"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "demo")
			var out string
			for _, args := range c.runs {
				killed := len(args) > 0 && args[0] == "!"
				if killed {
					args = args[1:]
				}
				b, err := exec.Command(bin, append([]string{"-dir", dir}, args...)...).CombinedOutput()
				out = string(b)
				var exit *exec.ExitError
				if killed != errors.As(err, &exit) {
					t.Fatalf("%v: killed=%v, err=%v\n%s", args, killed, err, out)
				}
			}
			if !strings.Contains(out, c.want) || !strings.Contains(out, c.retry) {
				t.Fatalf("want %q and %q in the last run's output:\n%s", c.want, c.retry, out)
			}
		})
	}
}
