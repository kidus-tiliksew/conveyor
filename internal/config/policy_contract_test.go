package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFrozenPolicyJSONContainsNoExecutionDetail(t *testing.T) {
	contract := PolicyContract{
		MaxBounces:    7,
		StageTimeouts: PolicyStageTimeouts{Spec: "30m", Implement: "4h", Review: "1h"},
		Review:        ReviewPanel{Seats: []ReviewSeat{{}}},
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
			var decoded PolicyContract
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
			var again PolicyContract
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
	cfg := &Config{Execution: ExecutionPolicy{VerifyStage: true}, Routing: Routing{Stages: map[string]StageRoute{"verify": {TimeoutText: "47m"}}}}
	frozen := cfg.FreezePolicy()
	data, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-model") || strings.Contains(string(data), "local") {
		t.Fatalf("execution leaked: %s", data)
	}
	var restored PolicyContract
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
	if got := (&Config{}).FreezePolicy().StageTimeouts.Verify; got != "1h" {
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
		var restored PolicyContract
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
	if legacy.WithPolicy(PolicyContract{}).Review.FailFast {
		t.Fatal("legacy contract inherited workspace fail_fast")
	}
}

// case: FF-001
// case: FF-003
// The operator's workspace toggle survives the stored-document round trip
// and is what the next task freezes.
func TestReviewFailFastSurvivesWorkspaceDocument(t *testing.T) {
	base := validConfig()
	normalized, err := normalizeDeployment(base, "test")
	if err != nil {
		t.Fatal(err)
	}
	document := normalized.WorkspaceDocument()
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
}

// component-runtime: a task row written in the earlier full-setup encoding
// still decodes, keeps only its policy members, counts as frozen when it
// named a setup, and re-encodes in the unchanged policy_contract shape.
func TestPolicyContractDecodesLegacySetupRows(t *testing.T) {
	legacy := `{"name":"backend","verify_stage":true,"max_bounces":4,"refresh_review":"full",
		"execution_settings":{"spec":{"harness":"codex","model":"m","effort":"high","timeout":"25m"},
		"implementation":{"harness":"codex","model":"m","model_policy":"bogus","timeout":"3h"},
		"review":{"execution":"mcp","fallback_harness":"codex","timeout":"50m"},"verify":{"timeout":"40m"}},
		"review":{"seats":[{"model":"a","harness":"codex"},{"model":"b"}],"fail_fast":true}}`
	var decoded PolicyContract
	if err := json.Unmarshal([]byte(legacy), &decoded); err != nil {
		t.Fatal(err)
	}
	want := PolicyStageTimeouts{Spec: "25m", Implement: "3h", Review: "50m", Verify: "40m"}
	if !decoded.VerifyStage || decoded.MaxBounces != 4 || decoded.RefreshReview != RefreshReviewFull || decoded.StageTimeouts != want ||
		len(decoded.Review.Seats) != 2 || decoded.Review.Seats[0] != (ReviewSeat{}) || !decoded.Review.FailFast || !decoded.HasFrozenPolicy() {
		t.Fatalf("legacy contract decoded as %+v", decoded)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	const shape = `{"verify_stage":true,"max_bounces":4,"stage_timeouts":{"implement":"3h","review":"50m","spec":"25m","verify":"40m"},"review":{"seats":[{},{}],"fail_fast":true},"refresh_review":"full"}`
	if string(encoded) != shape {
		t.Fatalf("encoded %s, want %s", encoded, shape)
	}
	var named PolicyContract
	if err := json.Unmarshal([]byte(`{"name":"kept"}`), &named); err != nil {
		t.Fatal(err)
	}
	if !named.HasFrozenPolicy() {
		t.Fatal("a legacy row that named its setup lost its frozen status")
	}
	var zero PolicyContract
	if err := json.Unmarshal([]byte(`{}`), &zero); err != nil {
		t.Fatal(err)
	}
	if zero.HasFrozenPolicy() {
		t.Fatalf("an empty legacy row decoded as frozen: %+v", zero)
	}
}

// The workspace distinct-models setting freezes onto the task contract at
// intake and a later workspace edit changes neither direction; an absent key
// decodes false, so an earlier frozen task keeps its behavior
// (req-review-gates-evidence AC-2.9).
func TestReviewDistinctModelsFreezesAndRoundTrips(t *testing.T) {
	if (&Config{}).FreezePolicy().Review.DistinctModels {
		t.Fatal("distinct_models must default off")
	}
	for _, frozenValue := range []bool{true, false} {
		cfg := &Config{Review: ReviewPanel{Seats: []ReviewSeat{{}, {}}, DistinctModels: frozenValue}}
		data, err := json.Marshal(cfg.FreezePolicy())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "distinct_models") != frozenValue {
			t.Fatalf("frozen value %t encoded as %s", frozenValue, data)
		}
		var restored PolicyContract
		if err := json.Unmarshal(data, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Review.DistinctModels != frozenValue {
			t.Fatalf("frozen review policy %s restored as %+v", data, restored.Review)
		}
		cfg.Review.DistinctModels = !frozenValue
		if got := cfg.WithPolicy(restored).Review.DistinctModels; got != frozenValue {
			t.Fatalf("workspace change to %t overrode frozen distinct_models %t", !frozenValue, frozenValue)
		}
	}
	var earlier PolicyContract
	if err := json.Unmarshal([]byte(`{"max_bounces":3,"stage_timeouts":{"review":"1h"},"review":{"seats":[{}],"fail_fast":true}}`), &earlier); err != nil {
		t.Fatal(err)
	}
	if earlier.Review.DistinctModels || !earlier.Review.FailFast {
		t.Fatalf("earlier contract decoded as %+v", earlier.Review)
	}
	workspaceOn := &Config{Review: ReviewPanel{DistinctModels: true}}
	if workspaceOn.WithPolicy(earlier).Review.DistinctModels {
		t.Fatal("earlier contract inherited workspace distinct_models")
	}
}

func TestReviewDistinctModelsSurvivesWorkspaceDocument(t *testing.T) {
	base := validConfig()
	normalized, err := normalizeDeployment(base, "test")
	if err != nil {
		t.Fatal(err)
	}
	document := normalized.WorkspaceDocument()
	document.Review.DistinctModels = true
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseWorkspaceDocument(raw, base, "distinct-models")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Review.DistinctModels || !parsed.FreezePolicy().Review.DistinctModels {
		t.Fatalf("parsed distinct_models=%t frozen=%t", parsed.Review.DistinctModels, parsed.FreezePolicy().Review.DistinctModels)
	}
	if !parsed.WorkspaceDocument().Review.DistinctModels || !parsed.PolicyDocument().Review.DistinctModels {
		t.Fatal("re-emitted workspace document dropped distinct_models")
	}
}

// promoteLegacyDefaultSetup replaces the review block with the setup's. The
// file's top-level distinct_models survives that projection, and a value the
// setup states wins (req-review-gates-evidence AC-2.9).
func TestLegacyDefaultSetupKeepsTopLevelDistinctModels(t *testing.T) {
	seats := []any{map[string]any{}, map[string]any{}}
	for name, tc := range map[string]struct {
		setup any
		want  map[string]any
	}{
		"setup review without the key": {map[string]any{"seats": seats}, map[string]any{"seats": seats, "distinct_models": true}},
		"setup review states the key":  {map[string]any{"seats": seats, "distinct_models": false}, map[string]any{"seats": seats, "distinct_models": false}},
		"setup without a review":       {nil, map[string]any{"distinct_models": true}},
	} {
		t.Run(name, func(t *testing.T) {
			setup := map[string]any{"name": "default"}
			if tc.setup != nil {
				setup["review"] = tc.setup
			}
			root := map[string]any{"default_setup": "default", "setups": []any{setup}, "review": map[string]any{"distinct_models": true}}
			promoteLegacyDefaultSetup(root)
			if !reflect.DeepEqual(root["review"], tc.want) {
				t.Fatalf("review=%v, want %v", root["review"], tc.want)
			}
		})
	}
	root := map[string]any{"default_setup": "default", "setups": []any{map[string]any{"name": "default"}}, "review": map[string]any{"seats": seats}}
	promoteLegacyDefaultSetup(root)
	if _, kept := root["review"]; kept {
		t.Fatalf("a file without the key keeps the earlier projection: %v", root["review"])
	}
}
