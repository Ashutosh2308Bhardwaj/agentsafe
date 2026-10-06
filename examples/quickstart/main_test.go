package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The quickstart as a user runs it: three separate processes sharing one log.
func TestQuickstartAsThreeProcesses(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "quickstart")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	first := run()
	if !strings.Contains(first, `awaiting_approval: {"invoice_id":"INV-1","amount":"4200.50"}`) || strings.Contains(first, "paying") {
		t.Fatalf("the first run must wait for approval and pay nothing:\n%s", first)
	}
	second := run("approve")
	if strings.Count(second, "paying INV-1 4200.50") != 1 || !strings.Contains(second, "finished - INV-1 paid.") {
		t.Fatalf("approving in a new process must pay once and finish:\n%s", second)
	}
	third := run("approve")
	if strings.Contains(third, "paying") || !strings.Contains(third, "nothing to approve; the run is finished") {
		t.Fatalf("approving again must not pay again:\n%s", third)
	}
}

// The README shows this program; it must be this program, byte for byte.
func TestREADMEShowsThisProgram(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	code, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	const start, end = "<!-- quickstart:start -->\n```go\n", "```\n<!-- quickstart:end -->"
	readme = bytes.ReplaceAll(readme, []byte("\r\n"), []byte("\n"))
	code = bytes.ReplaceAll(code, []byte("\r\n"), []byte("\n"))
	i, j := bytes.Index(readme, []byte(start)), bytes.Index(readme, []byte(end))
	if i < 0 || j < i {
		t.Fatal("README has no quickstart block between the markers")
	}
	if shown := readme[i+len(start) : j]; !bytes.Equal(shown, code) {
		t.Fatal("README's quickstart differs from examples/quickstart/main.go: copy main.go into the README block")
	}
}
