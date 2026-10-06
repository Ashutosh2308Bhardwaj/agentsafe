package gemini_test

import (
	"context"

	"github.com/Ashutosh2308Bhardwaj/agentsafe"
	"github.com/Ashutosh2308Bhardwaj/agentsafe/gemini"
	"google.golang.org/genai"
)

// Gemini as the agent's model. The key comes from GEMINI_API_KEY; pass a current model id.
// (Calls the API, so this example is compiled, not run.)
func ExampleNew() {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendGeminiAPI})
	if err != nil {
		panic(err)
	}
	r, err := agentsafe.New(gemini.New(client, "gemini-3-flash-preview"), &agentsafe.FileLog{Path: "payouts.jsonl"})
	if err != nil {
		panic(err)
	}
	_, _ = r.Start(ctx, "You reconcile payouts.", "Reconcile today's batch.")
}
