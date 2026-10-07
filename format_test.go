package agentsafe

import (
	"bytes"
	"context"
	"errors"
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
	denials                      int
	codec                        Codec // for sealed logs
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
	// v2: an approval refused twice by the Authorizer (not on the approver list), then approved.
	{file: "testdata/golden_v2_denied_then_approved.jsonl", version: 2,
		events: 28, step: 8, effects: 5, msgs: 16, status: StatusFinished, stop: "stop", approvedOps: 1, denials: 2},
	// v3: the same flow sealed at rest (AES-256-GCM, test key 0x42 x 32, id "env"); one denial.
	{file: "testdata/golden_v3_sealed.jsonl", version: 3, codec: AESGCM{Keys: map[string][]byte{"env": bytes.Repeat([]byte{0x42}, 32)}, Current: "env"},
		events: 27, step: 8, effects: 5, msgs: 16, status: StatusFinished, stop: "stop", approvedOps: 1, denials: 1},
	// v4: the scripted flow with an approval; run_started records key_bits (128), so its keys are 32 characters.
	{file: "testdata/golden_v4_scripted_approved.jsonl", version: 4,
		events: 26, step: 8, effects: 5, msgs: 16, status: StatusFinished, stop: "stop", approvedOps: 1},
}

func TestGoldenLogsStillRebuild(t *testing.T) {
	for _, g := range golden {
		t.Run(g.file, func(t *testing.T) {
			events, err := (&FileLog{Path: g.file, Codec: g.codec}).Read(context.Background())
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
			// Runs started before v4 used 64-bit keys and must keep them; every key in the log has that length.
			wantBits := 64
			if g.version >= 4 {
				wantBits = 128
			}
			if st.KeyBits != wantBits {
				t.Fatalf("a v%d run must read as %d-bit keys, got %d", g.version, wantBits, st.KeyBits)
			}
			assertKeyLength(t, events, wantBits/4)
			want := goldenShape{g.events, g.step, g.effects, g.msgs, g.status, g.stop, g.approvedOps, g.denials}
			if got := shapeOf(st); got != want {
				t.Fatalf("rebuilt state changed:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// goldenShape is what a golden log must rebuild to.
type goldenShape struct {
	Events, Step, Effects, Msgs int
	Status                      Status
	Stop                        string
	ApprovedOps, Denials        int
}

func shapeOf(st State) goldenShape {
	approved := 0
	for _, d := range st.ByKey {
		if d == "approved" {
			approved++
		}
	}
	return goldenShape{st.Events, st.Step, len(st.Effects), len(st.Messages), st.Status, st.Stop, approved, st.Denials}
}

func TestNewerFormatIsRefusedNotGuessed(t *testing.T) {
	_, err := Rebuild([]Event{{V: FormatVersion + 1, Type: EvRunStarted, Task: "t", MaxSteps: 1}})
	if !errors.Is(err, ErrNewerLogFormat) {
		t.Fatalf("an event from a newer library must be refused, got %v", err)
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
	events, _ := r.Log.Read(context.Background())
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
