package core

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TestValidateRiskReason covers the one-line reason rules that a person's
// raise and a risk order's completion share (req-risk-based-review AC-6.1,
// AC-12.2).
func TestValidateRiskReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		ok     bool
	}{
		{"small change", true},
		{strings.Repeat("x", MaxRiskReasonRunes), true},
		{"", false},
		{" \t", false},
		{"two\nlines", false},
		{"carriage\rreturn", false},
		{"line\u2028separator", false},
		{"nul\x00byte", false},
		{strings.Repeat("x", MaxRiskReasonRunes+1), false},
	} {
		if err := ValidateRiskReason("reason", tc.reason); (err == nil) != tc.ok {
			t.Fatalf("reason=%q err=%v, want ok=%t", tc.reason, err, tc.ok)
		}
	}
}

// TestRiskTierOf: the tier is the highest recorded rating and the earliest
// rating at that tier set it; no rating means no tier (AC-5.5).
func TestRiskTierOf(t *testing.T) {
	if _, ok := RiskTierOf[RiskRating](nil); ok {
		t.Fatal("no rating produced a tier")
	}
	setter, ok := RiskTierOf([]RiskRating{
		{Seq: 1, Tier: riskpolicy.TierLow, Source: RiskSourcePathRule},
		{Seq: 2, Tier: riskpolicy.TierMedium, Source: RiskSourceReviewer, ReviewSeat: 1},
		{Seq: 3, Tier: riskpolicy.TierMedium, Source: RiskSourceReviewer, ReviewSeat: 2},
		{Seq: 4, Tier: riskpolicy.TierLow, Source: RiskSourceReviewer, ReviewSeat: 3},
		{Seq: 5, Tier: "unknown"},
	})
	if !ok || setter.Seq != 2 || setter.Tier != riskpolicy.TierMedium {
		t.Fatalf("setter=%+v ok=%t", setter, ok)
	}
}

// TestServeRiskRatingCountsPathLists: a served rating carries every non-path
// field of the stored rating, covered_path_count in place of a head rating's
// covered paths, and path_count in place of each matched rule's paths. The
// stored rating and its nested rules stay unchanged (component-risk-review,
// Public surfaces; component-work-orders invariant 65).
func TestServeRiskRatingCountsPathLists(t *testing.T) {
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	rules := []riskpolicy.RuleMatch{
		{Source: riskpolicy.RuleBuiltin, Index: 0, Description: "GitHub Actions workflows", MinTier: riskpolicy.TierMedium, MinHumanApprovals: 1, Paths: []string{".github/workflows/ci.yml", ".github/workflows/release.yml"}},
		{Source: riskpolicy.RuleDeclared, Index: 3, Description: "Empty match", MinTier: riskpolicy.TierLow, Paths: []string{}},
	}
	head := RiskRating{Seq: 4, TaskID: "t", Key: RiskAgentKey("t-risk-2"), Source: RiskSourceRiskAgent, Tier: riskpolicy.TierMedium, HeadSHA: "head", Reason: "workflow",
		MatchedRules: rules, HumanApprovals: 1, BaseSHA: "base", DeclarationState: riskpolicy.DeclarationPresent, RiskWorkOrderID: "t-risk-2", RiskModel: "rater-model",
		CoveredPaths: []string{".github/workflows/ci.yml", ".github/workflows/release.yml", "internal/risk.go"}, CreatedAt: at}
	before, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	served := ServeRiskRating(head)
	after, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || len(head.CoveredPaths) != 3 || len(head.MatchedRules[0].Paths) != 2 || head.MatchedRules[1].Paths == nil {
		t.Fatalf("conversion changed the stored rating:\nbefore %s\nafter  %s", before, after)
	}
	if served.CoveredPathCount == nil || *served.CoveredPathCount != 3 {
		t.Fatalf("covered_path_count = %v, want 3", served.CoveredPathCount)
	}
	want := []ServedRuleMatch{
		{Source: riskpolicy.RuleBuiltin, Index: 0, Description: "GitHub Actions workflows", MinTier: riskpolicy.TierMedium, MinHumanApprovals: 1, PathCount: 2},
		{Source: riskpolicy.RuleDeclared, Index: 3, Description: "Empty match", MinTier: riskpolicy.TierLow, PathCount: 0},
	}
	if !reflect.DeepEqual(served.MatchedRules, want) {
		t.Fatalf("matched rules = %+v, want %+v", served.MatchedRules, want)
	}
	if served.Seq != 4 || served.TaskID != "t" || served.Key != head.Key || served.Source != RiskSourceRiskAgent || served.Tier != riskpolicy.TierMedium ||
		served.HeadSHA != "head" || served.Reason != "workflow" || served.HumanApprovals != 1 || served.BaseSHA != "base" || served.DeclarationState != riskpolicy.DeclarationPresent ||
		served.RiskWorkOrderID != "t-risk-2" || served.RiskModel != "rater-model" || !served.CreatedAt.Equal(at) {
		t.Fatalf("served metadata = %+v", served)
	}

	// The served keys are the stored keys with each path list replaced by
	// its count.
	requireServedKeys(t, head, served)

	// A risk agent head rating with no covered paths serves a zero count, and
	// a rating without matched rules serves an empty list, never null.
	empty := ServeRiskRating(RiskRating{Source: RiskSourceRiskAgent, Tier: riskpolicy.TierLow, HeadSHA: "h", RiskWorkOrderID: "o", Key: RiskAgentKey("o")})
	text, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `"covered_path_count":0`) || !strings.Contains(string(text), `"matched_rules":[]`) {
		t.Fatalf("empty head rating = %s", text)
	}

	// Plan, legacy plan and reviewer, person, and pending plan ratings keep
	// every field and carry no covered path count.
	for _, rating := range []RiskRating{
		{Seq: 1, Key: PlanRiskKey(1), Source: RiskSourcePlan, Tier: riskpolicy.TierHigh, Reason: "plan line", PlanVersion: 1, CreatedAt: at},
		{Seq: 2, Key: ReviewerRiskKey("r-1"), Source: RiskSourceReviewer, Tier: riskpolicy.TierLow, HeadSHA: "h", Reason: "seat", ReviewWorkOrderID: "r-1", ReviewRound: 2, ReviewSeat: 3, ReviewerModel: "m", CreatedAt: at},
		{Seq: 3, Key: PersonRiskKey(riskpolicy.TierHigh), Source: RiskSourcePerson, Tier: riskpolicy.TierHigh, Reason: "raise", ActorID: "user:a", PreviousTier: riskpolicy.TierLow, CreatedAt: at},
		{Seq: 5, Key: RiskAgentKey("p-1"), Source: RiskSourceRiskAgent, Tier: riskpolicy.TierHigh, Reason: "plan", PlanVersion: 2, RiskWorkOrderID: "p-1", RiskModel: "m", PlanPending: true, CreatedAt: at},
		{Seq: 6, Key: PathRiskKey("h"), Source: RiskSourcePathRule, Tier: riskpolicy.TierMedium, HeadSHA: "h", Reason: "rules", MatchedRules: rules, HumanApprovals: 1, BaseSHA: "b", DeclarationState: riskpolicy.DeclarationAbsent, CreatedAt: at},
	} {
		served := ServeRiskRating(rating)
		if served.CoveredPathCount != nil {
			t.Fatalf("%s rating serves covered_path_count %d", rating.Source, *served.CoveredPathCount)
		}
		requireServedKeys(t, rating, served)
	}
	if got := ServeRiskRatings(nil); got == nil || len(got) != 0 {
		t.Fatalf("ServeRiskRatings(nil) = %#v, want an empty list", got)
	}
}

// requireServedKeys compares the JSON of a stored rating and its served
// shape: every stored value except the two path lists appears unchanged, and
// the served value adds only the counts.
func requireServedKeys(t *testing.T, stored RiskRating, served ServedRiskRating) {
	t.Helper()
	var storedJSON, servedJSON map[string]any
	for value, into := range map[any]*map[string]any{&stored: &storedJSON, &served: &servedJSON} {
		text, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(text, into); err != nil {
			t.Fatal(err)
		}
	}
	storedRules, _ := storedJSON["matched_rules"].([]any)
	servedRules, _ := servedJSON["matched_rules"].([]any)
	if storedRules == nil {
		storedRules = []any{}
	}
	if len(servedRules) != len(storedRules) {
		t.Fatalf("served %d matched rules, stored %d", len(servedRules), len(storedRules))
	}
	for i := range storedRules {
		storedRule, servedRule := storedRules[i].(map[string]any), servedRules[i].(map[string]any)
		if _, ok := servedRule["paths"]; ok {
			t.Fatalf("served matched rule %d carries paths: %v", i, servedRule)
		}
		if servedRule["path_count"] != float64(len(storedRule["paths"].([]any))) {
			t.Fatalf("served matched rule %d path_count = %v, stored paths %v", i, servedRule["path_count"], storedRule["paths"])
		}
		delete(storedRule, "paths")
		delete(servedRule, "path_count")
		if !reflect.DeepEqual(storedRule, servedRule) {
			t.Fatalf("served matched rule %d = %v, stored %v", i, servedRule, storedRule)
		}
	}
	if _, ok := servedJSON["covered_paths"]; ok {
		t.Fatalf("served rating carries covered_paths: %v", servedJSON)
	}
	if covered, ok := storedJSON["covered_paths"].([]any); ok && servedJSON["covered_path_count"] != float64(len(covered)) {
		t.Fatalf("covered_path_count = %v, stored %d paths", servedJSON["covered_path_count"], len(covered))
	}
	for _, key := range []string{"matched_rules", "covered_paths", "covered_path_count"} {
		delete(storedJSON, key)
		delete(servedJSON, key)
	}
	if !reflect.DeepEqual(storedJSON, servedJSON) {
		storedKeys, servedKeys := mapKeys(storedJSON), mapKeys(servedJSON)
		t.Fatalf("served fields differ from stored fields:\nstored %v %v\nserved %v %v", storedKeys, storedJSON, servedKeys, servedJSON)
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// TestRiskTierOfServedRatings: the served ratings of a task read select the
// same setter as the stored ratings they came from, and a pending plan rating
// does not count (req-risk-based-review AC-4.3, AC-5.5).
func TestRiskTierOfServedRatings(t *testing.T) {
	stored := []RiskRating{
		{Seq: 1, Tier: riskpolicy.TierMedium, Source: RiskSourcePathRule, HeadSHA: "h"},
		{Seq: 2, Tier: riskpolicy.TierHigh, Source: RiskSourceRiskAgent, PlanVersion: 3, RiskWorkOrderID: "p", PlanPending: true},
		{Seq: 3, Tier: riskpolicy.TierHigh, Source: RiskSourceRiskAgent, HeadSHA: "h", RiskWorkOrderID: "o", RiskModel: "m", CoveredPaths: []string{"a"}},
		{Seq: 4, Tier: riskpolicy.TierHigh, Source: RiskSourcePerson, ActorID: "user:a"},
	}
	want, ok := RiskTierOf(stored)
	got, servedOK := RiskTierOf(ServeRiskRatings(stored))
	if !ok || !servedOK || want.Seq != 3 || got.Seq != 3 || got.RiskWorkOrderID != "o" || got.RiskModel != "m" {
		t.Fatalf("stored setter=%+v served setter=%+v", want, got)
	}
	if _, ok := RiskTierOf([]ServedRiskRating{{Tier: riskpolicy.TierHigh, PlanPending: true}}); ok {
		t.Fatal("a pending plan rating set the served tier")
	}
}
