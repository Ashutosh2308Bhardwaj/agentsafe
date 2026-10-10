package main

import (
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The demo's numbers are the point: twice straight to the server, once in every scene through agentsafe-mcp.
func TestTheDemoChargesOnceThroughAgentsafe(t *testing.T) {
	dir := t.TempDir()
	bin := func(name, pkg string) string {
		out := filepath.Join(dir, name)
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	d := demo{out: io.Discard, proxy: bin("agentsafe-mcp", "../cmd/agentsafe-mcp"),
		billing: bin("billing-mcp", "../internal/fakeupstream"), work: t.TempDir()}
	got, err := d.run()
	if err != nil {
		t.Fatal(err)
	}
	if want := (charges{direct: 2, proxied: 1, crashNoKey: 1, crashKey: 1}); got != want {
		t.Fatalf("charges %+v, want %+v", got, want)
	}
}
