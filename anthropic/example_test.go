package anthropic_test

import (
	"context"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/anthropic"
	sdk "github.com/anthropics/anthropic-sdk-go"
)

// Claude as the agent's model. Credentials come from ANTHROPIC_API_KEY or an `ant auth login` profile.
// (Calls the API, so this example is compiled, not run.)
func ExampleNew() {
	m := anthropic.New(sdk.NewClient()) // claude-opus-5-5, adaptive thinking, effort "high"
	r, err := agentsafe.New(m, &agentsafe.FileLog{Path: "payouts.jsonl"})
	if err != nil {
		panic(err)
	}
	_, _ = r.Start(context.Background(), "You reconcile payouts.", "Reconcile today's batch.")
}
