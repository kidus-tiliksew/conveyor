package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFrozenPolicyJSONContainsNoExecutionDetail(t *testing.T) {
	contract := ExecutionSetup{
		Name: "legacy-name",
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
	if !strings.Contains(string(data), `"implement":"4h"`) || !strings.Contains(string(data), `"seats":[{}]`) {
		t.Fatalf("policy projection lost timeout or review shape: %s", data)
	}
}
