package inprocess

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIRunMetersUsageAndRedactsTranscript(t *testing.T) {
	t.Parallel()
	const key = "sk-test-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Fatalf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"gpt-5.6-luna","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":17,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}},"debug":"%s"}`, key)
	}))
	defer server.Close()

	result, err := (&OpenAI{APIKey: key, BaseURL: server.URL, Client: server.Client()}).Run(context.Background(), "gpt-5.6-luna", "work")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "done" || result.TokensIn != 17 || result.TokensOut != 3 {
		t.Fatalf("result = %+v", result)
	}
	wantCost := (15*1.0 + 2*.1 + 3*6.0) / 1_000_000
	if result.CostUSD != wantCost {
		t.Fatalf("cost = %.9f, want %.9f", result.CostUSD, wantCost)
	}
	if strings.Contains(string(result.Transcript), key) || result.Redactions.Total() == 0 {
		t.Fatalf("unredacted transcript: %s", result.Transcript)
	}
	if !strings.Contains(string(result.Transcript), `"input":"work"`) {
		t.Fatalf("request missing from transcript: %s", result.Transcript)
	}
}

func TestEstimateOpenAICostLongContextMultiplier(t *testing.T) {
	t.Parallel()
	cost, err := estimateOpenAICost("gpt-5.4-2026-03-05", 300_000, 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	want := (300_000*2.5*2 + 10_000*15*1.5) / 1_000_000
	if cost != want {
		t.Fatalf("cost = %f, want %f", cost, want)
	}
}
