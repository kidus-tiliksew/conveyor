package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFrozenPolicyJSONContainsNoExecutionDetail(t *testing.T) {
	contract := ExecutionSetup{
		Name:       "legacy-name",
		MaxBounces: 7,
		ExecutionSettings: ContextualExecutionSettings{
			Spec:           ImplementationSettings{Harness: "codex", Model: "spec-model", Effort: "high", TimeoutText: "30m"},
			Implementation: ImplementationSettings{Harness: "claude", Model: "implementation-model", Effort: "high", TimeoutText: "4h"},
			Review:         ReviewExecutionSettings{FallbackModel: "review-model", FallbackHarness: "codex", TimeoutText: "1h"},
		},
		Review:        ReviewPanel{Seats: []ReviewSeat{{Model: "review-model", Harness: "codex", Effort: "high"}}},
		RefreshReview: RefreshReviewDelta,
	}
	data, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"legacy-name", "harness", "model", "effort", "execution_settings"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("policy contract contains %q: %s", forbidden, data)
		}
	}
	if !strings.Contains(string(data), `"max_bounces":7`) || !strings.Contains(string(data), `"implement":"4h"`) || !strings.Contains(string(data), `"seats":[{}]`) {
		t.Fatalf("policy projection lost timeout or review shape: %s", data)
	}
}

// DEC-43: the intake projection is immutable policy, not local execution data.
func TestVerifyPolicyFreezesAndRoundTrips(t *testing.T) {
	cfg := &Config{Execution: ExecutionPolicy{VerifyStage: true}, Routing: Routing{Stages: map[string]StageRoute{"verify": {TimeoutText: "47m", Harness: "local", Model: "private-model", Effort: "high"}}}}
	frozen := cfg.FreezePolicy()
	data, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-model") || strings.Contains(string(data), "local") {
		t.Fatalf("execution leaked: %s", data)
	}
	var restored ExecutionSetup
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	cfg.Execution.VerifyStage = false
	cfg.Routing.Stages["verify"] = StageRoute{TimeoutText: "2h"}
	projected := cfg.WithPolicy(restored)
	if !projected.Execution.VerifyStage || projected.Routing.Stages["verify"].TimeoutText != "47m" {
		t.Fatalf("frozen policy lost: %+v", restored)
	}
	if (&Config{}).FreezePolicy().VerifyStage {
		t.Fatal("verify must default off")
	}
	if got := (&Config{}).FreezePolicy().ExecutionSettings.Verify.TimeoutText; got != "1h" {
		t.Fatalf("default timeout = %q", got)
	}
}
