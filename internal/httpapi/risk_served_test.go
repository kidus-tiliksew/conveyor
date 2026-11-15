package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// requireServedRiskRatings walks one JSON answer and checks every rating in
// every `risk.ratings` list it holds: no rating carries `covered_paths`, a
// risk agent's rating of a head carries `covered_path_count`, and every
// matched rule carries `path_count` and no `paths` (component-risk-review,
// Public surfaces). It returns the ratings and matched rules it checked, so a
// caller can require that the check saw something.
func requireServedRiskRatings(t *testing.T, body []byte) (ratings, rules int) {
	t.Helper()
	var value any
	requireOKHTTP(t, json.Unmarshal(body, &value))
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			// The task's frozen policy also has a risk object, with no
			// ratings; the task read field risk always has a list.
			if risk, ok := typed["risk"].(map[string]any); ok && risk["ratings"] != nil {
				list, ok := risk["ratings"].([]any)
				if !ok {
					t.Fatalf("risk ratings is not a list: %v", risk)
				}
				for _, item := range list {
					rating, _ := item.(map[string]any)
					rules += requireServedRiskRating(t, rating)
					ratings++
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return ratings, rules
}

// requireServedRiskRating checks one served rating object and returns its
// matched rule count.
func requireServedRiskRating(t *testing.T, rating map[string]any) int {
	t.Helper()
	if rating == nil {
		t.Fatal("served rating is not an object")
	}
	if _, ok := rating["covered_paths"]; ok {
		t.Fatalf("served rating carries covered_paths: %v", rating)
	}
	if head, _ := rating["head_sha"].(string); rating["source"] == string(core.RiskSourceRiskAgent) && head != "" {
		if _, ok := rating["covered_path_count"].(float64); !ok {
			t.Fatalf("served risk agent head rating has no covered_path_count: %v", rating)
		}
	}
	rules, ok := rating["matched_rules"].([]any)
	if !ok {
		t.Fatalf("served rating has no matched_rules list: %v", rating)
	}
	for _, item := range rules {
		rule, _ := item.(map[string]any)
		if _, ok := rule["paths"]; ok {
			t.Fatalf("served matched rule carries paths: %v", rule)
		}
		if _, ok := rule["path_count"].(float64); !ok {
			t.Fatalf("served matched rule has no path_count: %v", rule)
		}
	}
	return len(rules)
}

const (
	sizeHeads         = 10
	sizePathsPerHead  = 299
	sizeWorkflowPaths = 10
	sizeDescription   = "Store and persistence code"
)

// sizeChangedPath is changed path j of head h: deterministic, unique per
// head, and 69 to 70 bytes long, so a list of them is large in JSON.
func sizeChangedPath(h, j int) string {
	if j < sizeWorkflowPaths {
		return fmt.Sprintf(".github/workflows/head-%02d-workflow-%03d-release-and-deploy-pipeline.yml", h, j)
	}
	return fmt.Sprintf("internal/store/postgres/head-%02d/persistence_component_%03d_handler.go", h, j)
}

// sizeHead is head h's 40-character commit ID.
func sizeHead(h int) string { return fmt.Sprintf("%040x", 0x5100+h) }

// addSizedHead records what normal history records for one head: the path
// rating that submission records with its matched rules, then the head's risk
// order with the same changed paths and rules, claimed and rated through
// MCP by its own risk agent session. Each step uses a real store command or
// MCP tool. description is the second rule's description. afterPathRating
// runs between the two ratings.
func (f *riskOrderFixture) addSizedHead(t *testing.T, h int, description string, afterPathRating func()) {
	t.Helper()
	paths := make([]string, sizePathsPerHead)
	for j := range paths {
		paths[j] = sizeChangedPath(h, j)
	}
	rules := []riskpolicy.RuleMatch{
		{Source: riskpolicy.RuleBuiltin, Index: 0, Description: "GitHub Actions workflows", MinTier: riskpolicy.TierMedium, MinHumanApprovals: 1, Paths: append([]string(nil), paths[:sizeWorkflowPaths]...)},
		{Source: riskpolicy.RuleDeclared, Index: 0, Description: description, MinTier: riskpolicy.TierMedium, MinHumanApprovals: 1, Paths: append([]string(nil), paths[sizeWorkflowPaths:]...)},
	}
	head := sizeHead(h)
	if _, created, err := f.store.RecordRiskRating(f.ctx, core.RiskRating{TaskID: riskOrderTaskID, Key: core.PathRiskKey(head), Source: core.RiskSourcePathRule,
		Tier: riskpolicy.TierMedium, HeadSHA: head, BaseSHA: riskBase, DeclarationState: riskpolicy.DeclarationPresent, Reason: "Matched path rules: workflows and store code.",
		MatchedRules: rules, HumanApprovals: 1}); err != nil || !created {
		t.Fatalf("record path rating for head %d: created=%t err=%v", h, created, err)
	}
	if afterPathRating != nil {
		afterPathRating()
	}
	order := f.createRiskOrder(t, store.RiskOrderCreateRequest{TaskID: riskOrderTaskID, Subject: core.RiskOrderSubject{HeadSHA: head, BaseSHA: riskBase},
		ChangedPaths: paths, MatchedRules: rules})
	session := fmt.Sprintf("size-rater-%d", h)
	f.claim(t, order, session, "rater-model")
	mcpToolText(t, f.handler, "operator-token", "submit_risk_rating", map[string]any{"workspace_id": "alpha", "work_order_id": order, "session_id": session,
		"tier": "medium", "reason": "The head changes release workflows and store code."})
}

// sizedRiskFixture is the risk order fixture with heads 0 to heads-1 added
// through addSizedHead.
func sizedRiskFixture(t *testing.T, heads int, description string) *riskOrderFixture {
	t.Helper()
	f := newRiskOrderFixture(t)
	for h := range heads {
		f.addSizedHead(t, h, description, nil)
	}
	return f
}

// getTaskPage calls MCP get_task on the fixture task. It returns the page
// text, which is the exact JSON that the read compares with mcpReadMaxBytes,
// and whether the call failed.
func (f *riskOrderFixture) getTaskPage(t *testing.T) (string, bool) {
	t.Helper()
	return mcpToolCall(t, f.handler, "operator-token", "get_task", map[string]any{"workspace_id": "alpha", "task_id": riskOrderTaskID})
}

// TestGetTaskWithTenLargeRatedHeadsStaysUnderTheReadBudget: a task with 10
// rated heads of 299 changed paths each, each with its path rating and its
// risk agent rating, answers MCP get_task under the 65,536-byte read budget.
// The served ratings carry exact counts and no path list, and the stored
// ratings keep every path (component-risk-review, Public surfaces;
// component-work-orders invariant 65).
func TestGetTaskWithTenLargeRatedHeadsStaysUnderTheReadBudget(t *testing.T) {
	f := sizedRiskFixture(t, sizeHeads, sizeDescription)
	text, failed := f.getTaskPage(t)
	if failed {
		t.Fatalf("get_task failed: %s", text)
	}
	if len(text) >= mcpReadMaxBytes {
		t.Fatalf("get_task page is %d bytes, want under %d", len(text), mcpReadMaxBytes)
	}
	ratings, rules := requireServedRiskRatings(t, []byte(text))
	if ratings != 2*sizeHeads || rules != 4*sizeHeads {
		t.Fatalf("checked %d ratings and %d matched rules, want %d and %d", ratings, rules, 2*sizeHeads, 4*sizeHeads)
	}
	var page struct {
		Items []struct {
			Risk *core.TaskRisk `json:"risk"`
		} `json:"items"`
	}
	requireOKHTTP(t, json.Unmarshal([]byte(text), &page))
	if len(page.Items) != 1 || page.Items[0].Risk == nil {
		t.Fatalf("get_task page = %s", text)
	}
	for _, rating := range page.Items[0].Risk.Ratings {
		if len(rating.MatchedRules) != 2 || rating.MatchedRules[0].PathCount != sizeWorkflowPaths || rating.MatchedRules[1].PathCount != sizePathsPerHead-sizeWorkflowPaths {
			t.Fatalf("served rating %d matched rules = %+v", rating.Seq, rating.MatchedRules)
		}
		switch rating.Source {
		case core.RiskSourceRiskAgent:
			if rating.CoveredPathCount == nil || *rating.CoveredPathCount != sizePathsPerHead {
				t.Fatalf("served risk agent rating %d covered_path_count = %v", rating.Seq, rating.CoveredPathCount)
			}
		case core.RiskSourcePathRule:
			if rating.CoveredPathCount != nil {
				t.Fatalf("served path rating %d carries covered_path_count", rating.Seq)
			}
		default:
			t.Fatalf("unexpected rating %+v", rating)
		}
	}

	// The stored ratings keep both lists. Served in full, as before, they
	// would carry the page far past the budget.
	stored, err := f.store.ListTaskRiskRatings(f.ctx, riskOrderTaskID)
	requireOKHTTP(t, err)
	for _, rating := range stored {
		if rating.Source == core.RiskSourceRiskAgent && len(rating.CoveredPaths) != sizePathsPerHead {
			t.Fatalf("stored risk agent rating %d keeps %d covered paths", rating.Seq, len(rating.CoveredPaths))
		}
		if len(rating.MatchedRules) != 2 || len(rating.MatchedRules[0].Paths)+len(rating.MatchedRules[1].Paths) != sizePathsPerHead {
			t.Fatalf("stored rating %d matched rules = %d", rating.Seq, len(rating.MatchedRules))
		}
	}
	// The figure for the old shape re-encodes the page with the stored
	// ratings in place of the served ones. Re-encoding the page as read
	// gives back its exact size, so the two figures compare.
	var generic map[string]any
	requireOKHTTP(t, json.Unmarshal([]byte(text), &generic))
	reencoded, err := json.Marshal(generic)
	requireOKHTTP(t, err)
	if len(reencoded) != len(text) {
		t.Fatalf("re-encoded page is %d bytes, read %d", len(reencoded), len(text))
	}
	storedJSON, err := json.Marshal(stored)
	requireOKHTTP(t, err)
	var storedList []any
	requireOKHTTP(t, json.Unmarshal(storedJSON, &storedList))
	generic["items"].([]any)[0].(map[string]any)["risk"].(map[string]any)["ratings"] = storedList
	withPaths, err := json.Marshal(generic)
	requireOKHTTP(t, err)
	if len(withPaths) <= mcpReadMaxBytes {
		t.Fatalf("the page with path lists is %d bytes, want over the budget to show the fixture's reach", len(withPaths))
	}
	t.Logf("get_task page: %d bytes for %d heads x %d changed paths (%d ratings); with path lists it would be %d bytes",
		len(text), sizeHeads, sizePathsPerHead, len(stored), len(withPaths))
}

// TestGetTaskReadBudgetCrossings measures the two dimensions the served shape
// leaves unbounded, on the fixture of
// TestGetTaskWithTenLargeRatedHeadsStaysUnderTheReadBudget. It finds the
// longest rule description that still fits with 10 heads, and the most
// ratings that still fit with the fixed description, and checks that one
// byte or one rating more fails with the budget refusal. It adds no bound and
// asserts no fixed crossing size; the logged figures are the measurement.
func TestGetTaskReadBudgetCrossings(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	budgetRefusal := func(text string) bool { return strings.Contains(text, "read exceeds 65536-byte output budget") }
	fits := func(description string) (int, bool, string) {
		f := sizedRiskFixture(t, sizeHeads, description)
		text, failed := f.getTaskPage(t)
		return len(text), !failed, text
	}

	// Rule descriptions: 10 heads, each rating with two matched rules, so
	// the second rule's description appears in 20 ratings.
	low, high := len(sizeDescription), 0
	if size, ok, text := fits(sizeDescription); !ok {
		t.Fatalf("baseline description does not fit: %s", text)
	} else {
		t.Logf("description experiment baseline: %d-byte description, page %d bytes", low, size)
	}
	for step := 1024; ; step *= 2 {
		if _, ok, _ := fits(strings.Repeat("d", step)); !ok {
			high = step
			break
		}
		low = step
		if step > mcpReadMaxBytes {
			t.Fatal("description never crossed the budget")
		}
	}
	for high-low > 1 {
		mid := (low + high) / 2
		if _, ok, _ := fits(strings.Repeat("d", mid)); ok {
			low = mid
		} else {
			high = mid
		}
	}
	lastSize, ok, _ := fits(strings.Repeat("d", low))
	if !ok {
		t.Fatalf("description of %d bytes no longer fits", low)
	}
	_, ok, text := fits(strings.Repeat("d", high))
	if ok || !budgetRefusal(text) {
		t.Fatalf("description of %d bytes: ok=%t text=%s", high, ok, text)
	}
	t.Logf("description experiment: %d heads, 20 ratings, 40 matched rules (20 with the varied description); last fit %d-byte description, page %d bytes; first over %d-byte description",
		sizeHeads, low, lastSize, high)

	// Rating count: the fixed description, and heads added one at a time.
	// Each head adds its path rating, then its risk agent rating and risk
	// order entry; the page is read after each rating.
	f := sizedRiskFixture(t, sizeHeads, sizeDescription)
	lastFit, lastFitSize, over := 0, 0, 0
	read := func() bool {
		text, failed := f.getTaskPage(t)
		ratings, err := f.store.ListTaskRiskRatings(f.ctx, riskOrderTaskID)
		requireOKHTTP(t, err)
		if failed {
			if !budgetRefusal(text) {
				t.Fatalf("get_task with %d ratings failed: %s", len(ratings), text)
			}
			over = len(ratings)
			return false
		}
		lastFit, lastFitSize = len(ratings), len(text)
		return true
	}
	if !read() {
		t.Fatal("the 10-head fixture does not fit")
	}
	baseRatings, baseSize := lastFit, lastFitSize
	for h := sizeHeads; over == 0; h++ {
		if h > 1000 {
			t.Fatal("rating count never crossed the budget")
		}
		pathFits := true
		f.addSizedHead(t, h, sizeDescription, func() { pathFits = read() })
		if pathFits {
			read()
		}
	}
	t.Logf("rating count experiment: %d-byte description, 2 matched rules per rating; from %d ratings at %d bytes, last fit %d ratings at %d bytes; first over %d ratings",
		len(sizeDescription), baseRatings, baseSize, lastFit, lastFitSize, over)
}
