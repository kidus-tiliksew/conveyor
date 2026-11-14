package core

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// RiskSource names where a recorded rating comes from. The list is closed:
// plan, path rule, reviewer, person, and risk agent (req-risk-based-review
// AC-5.6; component-risk-review, Extension rules). Plan and reviewer ratings
// were recorded from a plan's tier line or a review seat before risk orders;
// they stay readable and keep counting (AC-5.6).
type RiskSource string

const (
	RiskSourcePlan      RiskSource = "plan"
	RiskSourcePathRule  RiskSource = "path_rule"
	RiskSourceReviewer  RiskSource = "reviewer"
	RiskSourcePerson    RiskSource = "person"
	RiskSourceRiskAgent RiskSource = "risk_agent"
)

// Valid reports whether s is one of the five closed sources.
func (s RiskSource) Valid() bool {
	switch s {
	case RiskSourcePlan, RiskSourcePathRule, RiskSourceReviewer, RiskSourcePerson, RiskSourceRiskAgent:
		return true
	}
	return false
}

// RiskRatedEvent is the activity event kind every recorded rating appends
// (req-risk-based-review AC-5.6, AC-6.1).
const RiskRatedEvent = "risk.rated"

// MaxRiskReasonRunes bounds a stated rating reason.
const MaxRiskReasonRunes = 1000

// RiskRating is one recorded rating of a task. Ratings are append-only and
// unique per task and Key: a path rating once per head, a reviewer rating
// once per review work order (its round and seat), a plan rating once per
// plan version, a risk agent rating once per risk order, and a person raise
// once per raised tier
// (feature-risk-based-review RR-3, RR-8; component-risk-review, State and
// failure model).
type RiskRating struct {
	// Seq orders a task's ratings from 1 in the order they were recorded.
	Seq     int             `json:"seq"`
	TaskID  string          `json:"task_id"`
	Key     string          `json:"key"`
	Source  RiskSource      `json:"source"`
	Tier    riskpolicy.Tier `json:"tier"`
	HeadSHA string          `json:"head_sha,omitempty"`
	Reason  string          `json:"reason"`
	// MatchedRules and HumanApprovals carry a path rating's matched rules and
	// the largest min_human_approvals among them, kept for later approval
	// enforcement (req-risk-based-review REQ-8).
	MatchedRules   []riskpolicy.RuleMatch `json:"matched_rules"`
	HumanApprovals int                    `json:"human_approvals,omitempty"`
	// BaseSHA is the pull request base a path rating read its declaration at,
	// and DeclarationState is what that read found (AC-3.2).
	BaseSHA          string                      `json:"base_sha,omitempty"`
	DeclarationState riskpolicy.DeclarationState `json:"declaration_state,omitempty"`
	// Reviewer fields name the seat that stated a reviewer rating and the
	// model its session reported (AC-5.3).
	ReviewWorkOrderID string `json:"review_work_order_id,omitempty"`
	ReviewRound       int    `json:"review_round,omitempty"`
	ReviewSeat        int    `json:"review_seat,omitempty"`
	ReviewerModel     string `json:"reviewer_model,omitempty"`
	// PlanVersion names the approved plan version of a plan rating, or the
	// plan version a risk agent rated.
	PlanVersion int `json:"plan_version,omitempty"`
	// Risk agent fields name the completed risk order, the model its session
	// reported, and for a head the changed paths the rating covered
	// (req-risk-based-review AC-5.6, AC-12.2; feature-risk-based-review RR-11).
	// A head rating also carries HeadSHA, BaseSHA, and MatchedRules.
	RiskWorkOrderID string   `json:"risk_work_order_id,omitempty"`
	RiskModel       string   `json:"risk_model,omitempty"`
	CoveredPaths    []string `json:"covered_paths,omitempty"`
	// PlanPending marks a risk agent rating of a plan version that is not
	// approved. It is derived on every read from the task's plan versions and
	// never decides anything stored: such a rating stays readable and counts
	// toward the tier only once that version is approved, in either order
	// (req-risk-based-review AC-4.3, AC-5.5).
	PlanPending bool `json:"plan_pending,omitempty"`
	// ActorID names the person of a raise, and PreviousTier the task's tier
	// before it (AC-6.1).
	ActorID      string          `json:"actor_id,omitempty"`
	PreviousTier riskpolicy.Tier `json:"previous_tier,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// PathRiskKey is the uniqueness key of a head's path rating.
func PathRiskKey(headSHA string) string { return "path_rule:" + headSHA }

// ReviewerRiskKey is the uniqueness key of one review seat's rating. The
// review work order identifies its round and seat, so a seat number reused in
// a later round never collides.
func ReviewerRiskKey(reviewWorkOrderID string) string { return "reviewer:" + reviewWorkOrderID }

// PersonRiskKey is the uniqueness key of a person's raise to tier. A raise is
// strictly upward, so each tier is raised to at most once per task.
func PersonRiskKey(tier riskpolicy.Tier) string { return "person:" + string(tier) }

// PlanRiskKey is the uniqueness key of an approved plan version's rating.
func PlanRiskKey(version int) string { return fmt.Sprintf("plan:%d", version) }

// RiskAgentKey is the uniqueness key of a completed risk order's rating. One
// risk order records at most one rating.
func RiskAgentKey(riskWorkOrderID string) string { return "risk_agent:" + riskWorkOrderID }

// ValidateRiskReason checks a stated rating reason: one non-blank line of at
// most MaxRiskReasonRunes runes (req-risk-based-review AC-5.2, AC-6.1).
func ValidateRiskReason(field, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%s must be a non-blank line", field)
	}
	if strings.ContainsAny(reason, "\r\n  ") {
		return fmt.Errorf("%s must be one line", field)
	}
	if !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) {
		return fmt.Errorf("%s must be valid UTF-8 text", field)
	}
	if utf8.RuneCountInString(reason) > MaxRiskReasonRunes {
		return fmt.Errorf("%s must be at most %d characters", field, MaxRiskReasonRunes)
	}
	return nil
}

// ValidateStatedRisk checks a stated tier and its reason. Both are absent or
// both are present; a present tier is low, medium, or high.
func ValidateStatedRisk(tierField, reasonField, tier, reason string) error {
	if tier == "" && reason == "" {
		return nil
	}
	if tier == "" {
		return fmt.Errorf("%s is required when %s is stated", tierField, reasonField)
	}
	if _, err := riskpolicy.ParseTier(tier); err != nil {
		return fmt.Errorf("%s must be low, medium, or high", tierField)
	}
	return ValidateRiskReason(reasonField, reason)
}

// Validate checks a rating before a store records it.
func (r RiskRating) Validate() error {
	if strings.TrimSpace(r.TaskID) == "" {
		return errors.New("risk rating task_id is required")
	}
	if !r.Source.Valid() {
		return errors.New("risk rating source must be plan, path_rule, reviewer, person, or risk_agent")
	}
	if !r.Tier.Valid() {
		return errors.New("risk rating tier must be low, medium, or high")
	}
	if strings.TrimSpace(r.Key) == "" {
		return errors.New("risk rating key is required")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return errors.New("risk rating reason is required")
	}
	switch r.Source {
	case RiskSourcePathRule:
		if r.HeadSHA == "" || r.Key != PathRiskKey(r.HeadSHA) {
			return errors.New("path risk rating requires its head")
		}
	case RiskSourceReviewer:
		if r.ReviewWorkOrderID == "" || r.Key != ReviewerRiskKey(r.ReviewWorkOrderID) {
			return errors.New("reviewer risk rating requires its review work order")
		}
	case RiskSourcePerson:
		if r.ActorID == "" || r.Key != PersonRiskKey(r.Tier) {
			return errors.New("person risk rating requires its actor")
		}
	case RiskSourcePlan:
		if r.PlanVersion <= 0 || r.Key != PlanRiskKey(r.PlanVersion) {
			return errors.New("plan risk rating requires its plan version")
		}
	case RiskSourceRiskAgent:
		if r.RiskWorkOrderID == "" || r.Key != RiskAgentKey(r.RiskWorkOrderID) {
			return errors.New("risk agent rating requires its risk order")
		}
		if (r.PlanVersion > 0) == (r.HeadSHA != "") {
			return errors.New("risk agent rating rates exactly one plan version or one head")
		}
	}
	return nil
}

// RiskRatedPayload is the activity payload of one recorded rating: the tier,
// its source, the head, the reason, the matched rules, and for a reviewer the
// seat and the reported model name (AC-5.6). A raise also names the person
// and the old and new tier (AC-6.1).
func RiskRatedPayload(r RiskRating) map[string]any {
	matched := r.MatchedRules
	if matched == nil {
		matched = []riskpolicy.RuleMatch{}
	}
	payload := map[string]any{
		"tier": r.Tier, "source": r.Source, "head_sha": r.HeadSHA, "reason": r.Reason,
		"matched_rules": matched, "rating_key": r.Key, "seq": r.Seq,
	}
	switch r.Source {
	case RiskSourcePathRule:
		payload["base_sha"] = r.BaseSHA
		payload["declaration_state"] = r.DeclarationState
		payload["human_approvals"] = r.HumanApprovals
	case RiskSourceReviewer:
		payload["review_work_order_id"] = r.ReviewWorkOrderID
		payload["review_round"] = r.ReviewRound
		payload["review_seat"] = r.ReviewSeat
		payload["reviewer_model"] = r.ReviewerModel
	case RiskSourcePerson:
		payload["actor_id"] = r.ActorID
		payload["previous_tier"] = r.PreviousTier
		payload["new_tier"] = r.Tier
	case RiskSourcePlan:
		payload["plan_version"] = r.PlanVersion
	case RiskSourceRiskAgent:
		payload["risk_work_order_id"] = r.RiskWorkOrderID
		payload["risk_model"] = r.RiskModel
		if r.PlanVersion > 0 {
			payload["plan_version"] = r.PlanVersion
		} else {
			covered := r.CoveredPaths
			if covered == nil {
				covered = []string{}
			}
			payload["base_sha"] = r.BaseSHA
			payload["covered_paths"] = covered
		}
	}
	return payload
}

// RiskTierOf returns a task's tier over its recorded ratings and the rating
// that set it: the earliest rating at the highest tier. A task with no rating
// has no tier, and ok is false (req-risk-based-review AC-5.5). A risk agent
// rating of a plan version that is not approved does not count
// (req-risk-based-review AC-4.3).
func RiskTierOf(ratings []RiskRating) (RiskRating, bool) {
	var setter RiskRating
	found := false
	for _, rating := range ratings {
		if !rating.Tier.Valid() || rating.PlanPending {
			continue
		}
		if !found || rating.Tier.Rank() > setter.Tier.Rank() {
			setter, found = rating, true
		}
	}
	return setter, found
}

// RiskTierNotRaisedError refuses a tier request at or below the task's
// current tier and names that tier (req-risk-based-review AC-6.2).
type RiskTierNotRaisedError struct {
	Current riskpolicy.Tier
}

func (e *RiskTierNotRaisedError) Error() string {
	return fmt.Sprintf("risk tier can only be raised: the task's current tier is %s", e.Current)
}

// TaskRisk is the task read field risk (component-risk-review, Public
// surfaces; req-risk-based-review REQ-11, AC-11.2). REST, MCP, the CLI, and
// the task page read the same value. Tier, Source, and Reason are empty while
// the task has no rating. The required counts are the requirements in effect,
// which stay at the frozen seat minimum and merge gate while ratings are only
// recorded (AC-1.6); the calculated counts show what the tier and matched path
// rules would require. RecordedApprovals and AuthorApprovalNeeded count the
// approvals of the reviewed head of an enforcing task and stay zero for every
// other task.
type TaskRisk struct {
	Mode                     riskpolicy.Mode `json:"mode"`
	Tier                     riskpolicy.Tier `json:"tier,omitempty"`
	Source                   RiskSource      `json:"source,omitempty"`
	Reason                   string          `json:"reason,omitempty"`
	Head                     string          `json:"head,omitempty"`
	RequiredAgentReviews     int             `json:"required_agent_reviews"`
	RequiredHumanApprovals   int             `json:"required_human_approvals"`
	CalculatedAgentReviews   int             `json:"calculated_agent_reviews"`
	CalculatedHumanApprovals int             `json:"calculated_human_approvals"`
	RecordedApprovals        int             `json:"recorded_approvals"`
	AuthorApprovalNeeded     bool            `json:"author_approval_needed"`
	// The approval fields below are set for an enforcing task from its
	// approvals of the reviewed head (req-risk-based-review REQ-8, REQ-9,
	// AC-11.1, AC-11.2): the approvals still needed, whether an untrusted
	// author calls for a non-author's approval, the head the count binds to,
	// and whether the count and every author condition are met.
	RemainingApprovals      int  `json:"remaining_approvals"`
	NonAuthorApprovalNeeded bool `json:"non_author_approval_needed"`
	// AuthorWaiverNeeded is true when a trusted author's approval is
	// required but an untrusted coauthor keeps every author's approval from
	// counting, so only a non-author's waiver meets the condition.
	AuthorWaiverNeeded bool         `json:"author_waiver_needed"`
	UntrustedAuthor    bool         `json:"untrusted_author"`
	ApprovalHead       string       `json:"approval_head,omitempty"`
	ApprovalsSatisfied bool         `json:"approvals_satisfied"`
	Ratings            []RiskRating `json:"ratings"`
	// Orders lists the task's risk orders, oldest first, with what each
	// rates and its state, so every task read names the same orders
	// (req-risk-based-review AC-11.2; feature-risk-based-review RR-16).
	Orders []TaskRiskOrder `json:"orders"`
}

// TaskRiskOrder is one risk order on the task read: the order, its state,
// what it rates, and the model its claiming session reported
// (req-risk-based-review AC-5.6, AC-11.2).
type TaskRiskOrder struct {
	WorkOrderID string           `json:"work_order_id"`
	State       WorkOrderState   `json:"state"`
	Claimable   bool             `json:"claimable"`
	Subject     RiskOrderSubject `json:"subject"`
	Model       string           `json:"model,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
}

// TaskRiskOrders projects a task's risk orders for the task read. It returns
// an empty list, never nil, for a task with none.
func TaskRiskOrders(orders []RiskOrder) []TaskRiskOrder {
	out := make([]TaskRiskOrder, 0, len(orders))
	for _, order := range orders {
		out = append(out, TaskRiskOrder{WorkOrderID: order.Order.ID, State: order.Order.State, Claimable: order.Order.Claimable,
			Subject: order.Subject, Model: order.Order.Model, CreatedAt: order.CreatedAt})
	}
	return out
}
