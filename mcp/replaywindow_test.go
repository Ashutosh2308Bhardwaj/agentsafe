package mcp_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The same charge for a second order, once the window has passed, is a new charge with a new key: a deliberate
// repeat, not a retry. Within the window it's still a retry, answered from the log.
func TestAChargeRepeatedAfterTheReplayWindowIsANewCharge(t *testing.T) {
	up := &fakeUpstream{}
	policy := mcp.Policy{Identity: []string{mcp.AllArguments}, Key: mcp.KeyArgument, KeyArgument: "idempotency_key",
		ReplayWindow: mcp.Duration(200 * time.Millisecond)}
	r := newPolicyRig(t, filepath.Join(t.TempDir(), "p.jsonl"), up, map[string]mcp.Policy{"charge": policy})
	const order = `{"ticket_id":"T-1","amount":"10.00"}`

	r.call(t, "charge", order)
	if again := r.call(t, "charge", order); again.Meta[mcp.MetaPrefix+"replayed"] != true || up.charges != 1 {
		t.Fatalf("within the window it's a retry: charges=%d %v", up.charges, again.Meta)
	}
	time.Sleep(300 * time.Millisecond)
	later := r.call(t, "charge", order)
	if later.Meta[mcp.MetaPrefix+"replayed"] != nil || up.charges != 2 || resultText(later) != "charged 10.00 (charge 2)" {
		t.Fatalf("after it, a new charge: charges=%d %q", up.charges, resultText(later))
	}
	if len(up.keys) != 2 || up.keys[0] == up.keys[1] {
		t.Fatalf("with its own key, or the server would answer it with the first charge: %v", up.keys)
	}
}

// ["*"] is only a guess at what makes two calls one operation, so it's remembered for DefaultReplayWindow unless the
// policy says otherwise. Named fields are a real identity: remembered for the life of the log.
func TestTheDefaultReplayWindow(t *testing.T) {
	up := &fakeUpstream{}
	for _, c := range []struct {
		policy mcp.Policy
		want   time.Duration
	}{
		{mcp.Policy{Identity: []string{mcp.AllArguments}}, mcp.DefaultReplayWindow},
		{mcp.Policy{Identity: []string{mcp.AllArguments}, ReplayWindow: mcp.Duration(24 * time.Hour)}, 24 * time.Hour},
		{mcp.Policy{Identity: []string{"ticket_id"}}, 0},
		{mcp.Policy{Identity: []string{"ticket_id"}, ReplayWindow: mcp.Duration(time.Hour)}, time.Hour},
	} {
		tool, err := mcp.ProxiedTool(context.Background(), upstreamOf(t, up), "charge", c.policy)
		if err != nil {
			t.Fatal(err)
		}
		rw, ok := tool.(agentsafe.ReplayWindowTool)
		if !ok || rw.ReplayWindow() != c.want {
			t.Errorf("%+v: replay window %v, want %v", c.policy, rw, c.want)
		}
	}
}

func TestAReplayWindowNeedsAProtectedTool(t *testing.T) {
	up := &fakeUpstream{}
	for _, p := range []mcp.Policy{
		{Pass: true, ReplayWindow: mcp.Duration(time.Minute)},
		{Identity: []string{mcp.AllArguments}, ReplayWindow: mcp.Duration(-time.Minute)},
	} {
		_, err := mcp.ProxiedTool(context.Background(), upstreamOf(t, up), "charge", p)
		if err == nil || !strings.Contains(err.Error(), "replay") {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

// upstreamOf is a client session to up's server, closed when the test ends.
func upstreamOf(t *testing.T, up *fakeUpstream) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := up.server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	upstream, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	return upstream
}
