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
// plan, path rule, reviewer, and person (req-risk-based-review AC-5.6;
// component-risk-review, Extension rules).
type RiskSource string

const (
	RiskSourcePlan     RiskSource = "plan"
	RiskSourcePathRule RiskSource = "path_rule"
	RiskSourceReviewer RiskSource = "reviewer"
	RiskSourcePerson   RiskSource = "person"
)

// Valid reports whether s is one of the four closed sources.
func (s RiskSource) Valid() bool {
	switch s {
	case RiskSourcePlan, RiskSourcePathRule, RiskSourceReviewer, RiskSourcePerson:
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
// plan version, and a person raise once per raised tier
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
	// PlanVersion names the approved plan version of a plan rating.
	PlanVersion int `json:"plan_version,omitempty"`
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
		return errors.New("risk rating source must be plan, path_rule, reviewer, or person")
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
	}
	return payload
}

// RiskTierOf returns a task's tier over its recorded ratings and the rating
// that set it: the earliest rating at the highest tier. A task with no rating
// has no tier, and ok is false (req-risk-based-review AC-5.5).
func RiskTierOf(ratings []RiskRating) (RiskRating, bool) {
	var setter RiskRating
	found := false
	for _, rating := range ratings {
		if !rating.Tier.Valid() {
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
// rules would require. RecordedApprovals and AuthorApprovalNeeded report no
// approval records, which a later delivery adds.
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
	Ratings                  []RiskRating    `json:"ratings"`
}
