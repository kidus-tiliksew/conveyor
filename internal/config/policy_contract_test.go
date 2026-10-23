package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// A task with no frozen seat count keeps the workspace panel; a contract
// frozen to zero seats stays frozen. Absent and null seats decode as no
// frozen count, an empty array as a frozen zero, and encoding preserves the
// distinction without a null that migration 099 cannot read as an array.
func TestFrozenSeatCountEncodings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   string
		frozen bool
	}{
		{"absent", `{"max_bounces":0,"stage_timeouts":{},"review":{}}`, false},
		{"null", `{"max_bounces":0,"stage_timeouts":{},"review":{"seats":null}}`, false},
		{"empty", `{"max_bounces":0,"stage_timeouts":{},"review":{"seats":[]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoded ExecutionSetup
			if err := json.Unmarshal([]byte(tc.data), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.HasFrozenPolicy() != tc.frozen || (decoded.Review.Seats != nil) != tc.frozen || len(decoded.Review.Seats) != 0 {
				t.Fatalf("decoded frozen=%t seats=%#v, want frozen=%t", decoded.HasFrozenPolicy(), decoded.Review.Seats, tc.frozen)
			}
			encoded, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), `"seats":null`) || strings.Contains(string(encoded), `"seats":[]`) != tc.frozen {
				t.Fatalf("encoded %s, want frozen=%t", encoded, tc.frozen)
			}
			var again ExecutionSetup
			if err := json.Unmarshal(encoded, &again); err != nil {
				t.Fatal(err)
			}
			if again.HasFrozenPolicy() != tc.frozen || (again.Review.Seats != nil) != tc.frozen {
				t.Fatalf("round trip of %s lost the frozen seat count: %#v", encoded, again.Review.Seats)
			}
		})
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

// case: FF-001
// case: FF-003
// The workspace fail-fast setting freezes onto the task contract at intake;
// a later workspace change in either direction never alters the projection
// a task's lifecycle reads, and a legacy contract never fails fast.
func TestReviewFailFastFreezesAndRoundTrips(t *testing.T) {
	if (&Config{}).FreezePolicy().Review.FailFast {
		t.Fatal("fail_fast must default off")
	}
	for _, frozenValue := range []bool{true, false} {
		cfg := &Config{Review: ReviewPanel{Seats: []ReviewSeat{{}, {}}, FailFast: frozenValue}}
		data, err := json.Marshal(cfg.FreezePolicy())
		if err != nil {
			t.Fatal(err)
		}
		var restored ExecutionSetup
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Review.FailFast != frozenValue || len(restored.Review.Seats) != 2 {
			t.Fatalf("frozen review policy %s restored as %+v", data, restored.Review)
		}
		cfg.Review.FailFast = !frozenValue
		if got := cfg.WithPolicy(restored).Review.FailFast; got != frozenValue {
			t.Fatalf("workspace change to %t overrode frozen fail_fast %t", !frozenValue, frozenValue)
		}
	}
	legacy := &Config{Review: ReviewPanel{FailFast: true}}
	if legacy.WithPolicy(ExecutionSetup{}).Review.FailFast {
		t.Fatal("legacy contract inherited workspace fail_fast")
	}
}

// case: FF-001
// case: FF-003
// The operator's workspace toggle survives the stored-document round trip,
// including configurations whose review seats come from execution setups,
// and is what the next task freezes.
func TestReviewFailFastSurvivesWorkspaceDocument(t *testing.T) {
	base := validConfig()
	normalized, err := normalize(base, "test")
	if err != nil {
		t.Fatal(err)
	}
	document := normalized.WorkspaceDocument()
	if len(document.Setups) == 0 {
		t.Fatal("fixture must exercise setup-owned review seats")
	}
	document.Review.FailFast = true
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseWorkspaceDocument(raw, base, "fail-fast")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Review.FailFast || !parsed.FreezePolicy().Review.FailFast {
		t.Fatalf("parsed fail_fast=%t frozen=%t", parsed.Review.FailFast, parsed.FreezePolicy().Review.FailFast)
	}
	if !parsed.WorkspaceDocument().Review.FailFast || !parsed.PolicyDocument().Review.FailFast {
		t.Fatal("re-emitted workspace document dropped fail_fast")
	}
	if !parsed.WithSetup(parsed.Setups[0]).FreezePolicy().Review.FailFast {
		t.Fatal("selecting an execution setup dropped workspace fail_fast")
	}
}
