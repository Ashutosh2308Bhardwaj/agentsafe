package trace

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
)

// Every golden log (every format ever written, see FORMAT.md) still turns into a trace.
func TestGoldenLogsStillTrace(t *testing.T) {
	sealed := agentsafe.AESGCM{Keys: map[string][]byte{"env": bytes.Repeat([]byte{0x42}, 32)}, Current: "env"}
	files, _ := filepath.Glob("../testdata/golden_*.jsonl")
	if len(files) < 5 {
		t.Fatalf("want the golden logs, found %d", len(files))
	}
	for _, f := range files {
		events, err := (&agentsafe.FileLog{Path: f, Codec: sealed}).Read(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if _, err := Build(events, "golden"); err != nil {
			t.Fatalf("%s: a golden log must trace: %v", f, err)
		}
	}
}
