package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parseReviewScopeDocument parses the policy projection of validConfig with
// the two review scope members set as stated; an empty value omits the key.
func parseReviewScopeDocument(t *testing.T, laterRound, refresh string) (*Config, error) {
	t.Helper()
	base := validConfig()
	normalized, err := normalizeDeployment(base, "test")
	if err != nil {
		t.Fatal(err)
	}
	document := normalized.PolicyDocument()
	document.LaterRoundReview, document.RefreshReview = laterRound, refresh
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return ParseWorkspaceDocument(raw, base, "review-scope")
}

// feature-delta-review-rounds DR-1; req-review-gates-evidence AC-4.3,
// AC-12.1: each member accepts its two values, an absent member takes its
// default, and any other value is refused with the member named and the
// value withheld. refresh_review none is valid only in a frozen contract.
func TestWorkspaceReviewScopeValuesDefaultsAndRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, laterRound, refresh         string
		wantLaterRound, wantRefresh, fail string
	}{
		{name: "absent", wantLaterRound: "full", wantRefresh: "delta"},
		{name: "full and delta", laterRound: "full", refresh: "delta", wantLaterRound: "full", wantRefresh: "delta"},
		{name: "delta and full", laterRound: "delta", refresh: "full", wantLaterRound: "delta", wantRefresh: "full"},
		{name: "later round only", laterRound: "delta", wantLaterRound: "delta", wantRefresh: "delta"},
		{name: "refresh only", refresh: "full", wantLaterRound: "full", wantRefresh: "full"},
		{name: "later round unknown", laterRound: "partial-secret", fail: "later_round_review"},
		{name: "later round none", laterRound: "none", fail: "later_round_review"},
		{name: "later round case", laterRound: "Delta", fail: "later_round_review"},
		{name: "refresh none", refresh: "none", fail: "refresh_review"},
		{name: "refresh unknown", refresh: "partial-secret", fail: "refresh_review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseReviewScopeDocument(t, tc.laterRound, tc.refresh)
			if tc.fail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fail) {
					t.Fatalf("err = %v, want a refusal naming %s", err, tc.fail)
				}
				if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "Delta") {
					t.Fatalf("refusal repeats the value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parsed.LaterRoundReview != tc.wantLaterRound || parsed.RefreshReview != tc.wantRefresh {
				t.Fatalf("parsed later_round_review=%q refresh_review=%q, want %q %q", parsed.LaterRoundReview, parsed.RefreshReview, tc.wantLaterRound, tc.wantRefresh)
			}
			for name, document := range map[string]WorkspaceDocument{"workspace": parsed.WorkspaceDocument(), "policy": parsed.PolicyDocument()} {
				if document.LaterRoundReview != tc.wantLaterRound || document.RefreshReview != tc.wantRefresh {
					t.Fatalf("%s document later_round_review=%q refresh_review=%q", name, document.LaterRoundReview, document.RefreshReview)
				}
			}
		})
	}
}

// The policy-only JSON projection the workspace API serves and the canonical
// persisted document both carry the two members, and a stored row reloads
// with them.
func TestWorkspaceReviewScopesSurvivePolicyProjectionAndPersistence(t *testing.T) {
	parsed, err := parseReviewScopeDocument(t, "delta", "full")
	if err != nil {
		t.Fatal(err)
	}
	served, err := json.Marshal(parsed.PolicyDocument())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(served), `"later_round_review":"delta"`) || !strings.Contains(string(served), `"refresh_review":"full"`) {
		t.Fatalf("policy projection = %s", served)
	}
	var projection map[string]any
	if err := json.Unmarshal(served, &projection); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"execution_settings", "routing", "planning_models"} {
		if _, ok := projection[retired]; ok {
			t.Fatalf("policy projection carries %s: %s", retired, served)
		}
	}
	stored, err := MarshalPolicyDocument(parsed)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := ParseStoredWorkspaceDocument(stored, validConfig(), "stored")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LaterRoundReview != "delta" || reloaded.RefreshReview != "full" {
		t.Fatalf("stored row reloaded later_round_review=%q refresh_review=%q from %s", reloaded.LaterRoundReview, reloaded.RefreshReview, stored)
	}
	decoded, err := DecodeStoredWorkspaceDocument(stored)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.LaterRoundReview != "delta" || decoded.RefreshReview != "full" {
		t.Fatalf("decoded stored row later_round_review=%q refresh_review=%q", decoded.LaterRoundReview, decoded.RefreshReview)
	}
}

// Both freeze paths copy the two workspace values by value, so a later
// workspace edit changes no task's contract (req-review-gates-evidence
// AC-4.3, AC-12.1).
func TestWorkspaceReviewScopesFreezePolicy(t *testing.T) {
	parsed, err := parseReviewScopeDocument(t, "delta", "full")
	if err != nil {
		t.Fatal(err)
	}
	defaults, ok := parsed.DefaultPolicy()
	if !ok || defaults.LaterRoundReview != "delta" || defaults.RefreshReview != "full" {
		t.Fatalf("DefaultPolicy = %+v ok=%t", defaults, ok)
	}
	frozen := parsed.FreezePolicy()
	if frozen.LaterRoundReview != "delta" || frozen.RefreshReview != "full" || frozen.LaterRoundReviewScope() != "delta" {
		t.Fatalf("FreezePolicy later_round_review=%q refresh_review=%q", frozen.LaterRoundReview, frozen.RefreshReview)
	}
	parsed.LaterRoundReview, parsed.RefreshReview = "full", "delta"
	if frozen.LaterRoundReview != "delta" || frozen.RefreshReview != "full" {
		t.Fatalf("a workspace edit changed a frozen contract: %+v", frozen)
	}
	if again := parsed.FreezePolicy(); again.LaterRoundReview != "full" || again.RefreshReview != "delta" {
		t.Fatalf("a task filed after the edit froze %+v", again)
	}
	// A value with no execution settings and no stage routes takes the
	// fallback path, which copies the same members.
	bare := &Config{LaterRoundReview: "delta", RefreshReview: "full"}
	if _, ok := bare.DefaultPolicy(); ok {
		t.Fatal("a bare value reported a default policy")
	}
	if got := bare.FreezePolicy(); got.LaterRoundReview != "delta" || got.RefreshReview != "full" {
		t.Fatalf("fallback FreezePolicy = %+v", got)
	}
	// A value that never parsed a workspace document freezes the defaults.
	if got := (&Config{}).FreezePolicy(); got.LaterRoundReview != "full" || got.RefreshReview != "delta" {
		t.Fatalf("default FreezePolicy later_round_review=%q refresh_review=%q", got.LaterRoundReview, got.RefreshReview)
	}
}

// A stored contract written before later_round_review existed reads as full in
// both the policy shape and the earlier full-setup shape; explicit values
// survive encoding; and a stored refresh_review of none still decodes
// unchanged (feature-delta-review-rounds DR-1; req-review-gates-evidence
// AC-4.2, AC-12.1).
func TestPolicyContractLaterRoundReviewEncodings(t *testing.T) {
	for name, data := range map[string]string{
		"policy shape":       `{"max_bounces":3,"stage_timeouts":{"review":"1h"},"review":{"seats":[{}]},"refresh_review":"delta"}`,
		"full-setup shape":   `{"name":"backend","max_bounces":3,"refresh_review":"full","review":{"seats":[{}]}}`,
		"refresh none shape": `{"max_bounces":3,"stage_timeouts":{"review":"1h"},"review":{"seats":[{}]},"refresh_review":"none"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var decoded PolicyContract
			if err := json.Unmarshal([]byte(data), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.LaterRoundReviewScope() != LaterRoundReviewFull || decoded.LaterRoundReview != "" {
				t.Fatalf("contract without later_round_review read as %q (stored %q)", decoded.LaterRoundReviewScope(), decoded.LaterRoundReview)
			}
			encoded, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "later_round_review") {
				t.Fatalf("re-encoding added the member to an earlier contract: %s", encoded)
			}
		})
	}
	var none PolicyContract
	if err := json.Unmarshal([]byte(`{"max_bounces":3,"stage_timeouts":{"review":"1h"},"review":{},"refresh_review":"none"}`), &none); err != nil {
		t.Fatal(err)
	}
	if none.RefreshReview != RefreshReviewNone || !none.HasFrozenPolicy() {
		t.Fatalf("stored refresh_review none decoded as %+v", none)
	}
	for _, value := range []string{LaterRoundReviewFull, LaterRoundReviewDelta} {
		contract := PolicyContract{MaxBounces: 2, Review: ReviewPanel{Seats: []ReviewSeat{{}}}, RefreshReview: RefreshReviewNone, LaterRoundReview: value}
		encoded, err := json.Marshal(contract)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"later_round_review":"`+value+`"`) || !strings.Contains(string(encoded), `"refresh_review":"none"`) {
			t.Fatalf("encoded %s", encoded)
		}
		var restored PolicyContract
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.LaterRoundReview != value || restored.LaterRoundReviewScope() != value || restored.RefreshReview != RefreshReviewNone {
			t.Fatalf("restored %+v from %s", restored, encoded)
		}
	}
	var legacyDelta PolicyContract
	if err := json.Unmarshal([]byte(`{"name":"backend","later_round_review":"delta"}`), &legacyDelta); err != nil {
		t.Fatal(err)
	}
	if legacyDelta.LaterRoundReviewScope() != LaterRoundReviewDelta {
		t.Fatalf("full-setup shape lost later_round_review: %+v", legacyDelta)
	}
	var onlyLaterRound PolicyContract
	if err := json.Unmarshal([]byte(`{"max_bounces":0,"stage_timeouts":{},"review":{},"later_round_review":"delta"}`), &onlyLaterRound); err != nil {
		t.Fatal(err)
	}
	if !onlyLaterRound.HasFrozenPolicy() {
		t.Fatal("a contract holding only later_round_review decoded as unfrozen")
	}
}
