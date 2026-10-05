package agentsafe

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Golden logs: real runs committed to testdata/. Every future version of this library must keep rebuilding
// them to exactly this state. If this test fails, you broke compatibility with logs already on disk:
// add an upgrade step and bump FormatVersion instead (see FORMAT.md).
var golden = []struct {
	file                         string
	version                      int
	events, step, effects, msgs  int
	status                       Status
	stop                         string
	approvedOps, refusedOrMissed int
}{
	// A real gpt-oss-120b run (week 3) written before format versions existed: a grounding refusal at
	// step 5, an approval, and a kill -9 right after the gateway charged, then a resume.
	{file: "testdata/golden_v0_real_gateway_crash.jsonl", version: 0,
		events: 29, step: 9, effects: 5, msgs: 18, status: StatusFinished, stop: "stop", approvedOps: 1},
	// The scripted example with an approval, written in format v1.
	{file: "testdata/golden_v1_scripted_approved.jsonl", version: 1,
		events: 26, step: 8, effects: 5, msgs: 16, status: StatusFinished, stop: "stop", approvedOps: 1},
	// The same flow written with the hash chain ("prev" on every line): freezes the chain's on-disk form.
	{file: "testdata/golden_v1_chained_approved.jsonl", version: 1,
		events: 26, step: 8, effects: 5, msgs: 16, status: StatusFinished, stop: "stop", approvedOps: 1},
}

func TestGoldenLogsStillRebuild(t *testing.T) {
	for _, g := range golden {
		t.Run(g.file, func(t *testing.T) {
			events, err := (&FileLog{Path: g.file}).Read()
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.V != g.version {
					t.Fatalf("golden file should be entirely v%d, event %d is v%d", g.version, e.Seq, e.V)
				}
			}
			st, err := Rebuild(events)
			if err != nil {
				t.Fatalf("a log already on disk no longer rebuilds: %v", err)
			}
			approved := 0
			for _, d := range st.ByKey {
				if d == "approved" {
					approved++
				}
			}
			if st.Events != g.events || st.Step != g.step || len(st.Effects) != g.effects || len(st.Messages) != g.msgs ||
				st.Status != g.status || st.Stop != g.stop || approved != g.approvedOps {
				t.Fatalf("rebuilt state changed: events=%d step=%d effects=%d msgs=%d status=%s stop=%s approved=%d",
					st.Events, st.Step, len(st.Effects), len(st.Messages), st.Status, st.Stop, approved)
			}
			if _, err := BuildTrace(events, "golden"); err != nil {
				t.Fatalf("trace from a golden log: %v", err)
			}
		})
	}
}

func TestNewerFormatIsRefusedNotGuessed(t *testing.T) {
	_, err := Rebuild([]Event{{V: FormatVersion + 1, Type: EvRunStarted, Task: "t", MaxSteps: 1}})
	if !errors.Is(err, ErrNewerLogFormat) {
		t.Fatalf("an event from a newer library must be refused, got %v", err)
	}
	rep := Reconcile(nil, nil, []Event{{V: FormatVersion + 1}}, nil)
	if rep.Pass() || !strings.Contains(rep.String(), "can't be read") {
		t.Fatalf("the checker must flag an unreadable log, got:\n%s", rep)
	}
}

func TestMixedVersionsInOneRun(t *testing.T) {
	// Started by an old library (v0), resumed by this one (v1): each line is upgraded on its own.
	_, err := Rebuild([]Event{
		{V: 0, Type: EvRunStarted, System: "s", Task: "t", MaxSteps: 4},
		{V: 1, Type: EvModelDecided, Step: 1, Message: &Message{Role: RoleAssistant, Content: Str("done")}, FinishReason: "stop"},
		{V: 1, Type: EvRunFinished, Stop: "stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmitStampsTheCurrentVersion(t *testing.T) {
	r, _, _ := newRun(t)
	if _, err := r.Start(context.Background(), "sys", "task"); err != nil {
		t.Fatal(err)
	}
	events, _ := r.Log.Read()
	for _, e := range events {
		if e.V != FormatVersion {
			t.Fatalf("event %d written as v%d, want v%d", e.Seq, e.V, FormatVersion)
		}
	}
}

func TestUpgradeIsIdempotent(t *testing.T) {
	e := Event{V: 0, Seq: 3, Type: EvRunFinished, Stop: "stop"}
	once, err := Upgrade(e)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Upgrade(once)
	if err != nil || twice != once || once.V != FormatVersion {
		t.Fatalf("upgrading an up-to-date event must change nothing: %+v %+v %v", once, twice, err)
	}
}
