package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// RiskOrderSubject names what one risk order rates: exactly one plan version,
// or one head with its base (feature-risk-based-review RR-11). A task has at
// most one risk order per subject.
type RiskOrderSubject struct {
	PlanVersion int    `json:"plan_version,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	BaseSHA     string `json:"base_sha,omitempty"`
}

// Kind returns the subject's kind. An invalid subject returns the empty kind.
func (s RiskOrderSubject) Kind() riskpolicy.RiskSubjectKind {
	if s.Validate() != nil {
		return ""
	}
	if s.PlanVersion > 0 {
		return riskpolicy.RiskSubjectPlan
	}
	return riskpolicy.RiskSubjectHead
}

// Validate refuses a subject that names neither or both a plan version and a
// head, a head without its base, or a value that cannot form its key.
func (s RiskOrderSubject) Validate() error {
	head, base := strings.TrimSpace(s.HeadSHA), strings.TrimSpace(s.BaseSHA)
	switch {
	case s.PlanVersion < 0:
		return errors.New("risk order plan version must be positive")
	case s.PlanVersion > 0 && (head != "" || base != "" || s.HeadSHA != "" || s.BaseSHA != ""):
		return errors.New("risk order rates exactly one plan version or one head, not both")
	case s.PlanVersion > 0:
		return nil
	case head == "":
		return errors.New("risk order requires a plan version or a head")
	case base == "":
		return errors.New("risk order head requires its base")
	case head != s.HeadSHA || base != s.BaseSHA || strings.ContainsAny(s.HeadSHA+s.BaseSHA, "@:\x00 \t\r\n"):
		return errors.New("risk order head and base must be commit identifiers")
	}
	return nil
}

// Key is the subject's uniqueness key within a task: "plan:<version>" or
// "head:<head>@<base>".
func (s RiskOrderSubject) Key() string {
	if s.PlanVersion > 0 {
		return fmt.Sprintf("plan:%d", s.PlanVersion)
	}
	return "head:" + s.HeadSHA + "@" + s.BaseSHA
}

// RiskOrder is a risk-stage work order with what it rates. ChangedPaths and
// MatchedRules are copied at creation for a head and empty for a plan
// version. The order's completion reads them, never the completing call
// (feature-risk-based-review RR-11; req-risk-based-review AC-12.5).
type RiskOrder struct {
	Order        WorkOrder              `json:"order"`
	Subject      RiskOrderSubject       `json:"subject"`
	ChangedPaths []string               `json:"changed_paths"`
	MatchedRules []riskpolicy.RuleMatch `json:"matched_rules"`
	CreatedAt    time.Time              `json:"created_at"`
}

// Open reports whether the order is still queued or claimed, or waits in a
// recoverable state, so a later act may cancel it.
func (o RiskOrder) Open() bool {
	return o.Order.State != WorkOrderCompleted && o.Order.State != WorkOrderCancelled
}

// Clone returns a deep copy of the risk order's own lists.
func (o RiskOrder) Clone() RiskOrder {
	o.ChangedPaths = slices.Clone(o.ChangedPaths)
	if o.ChangedPaths == nil {
		o.ChangedPaths = []string{}
	}
	o.MatchedRules = slices.Clone(o.MatchedRules)
	if o.MatchedRules == nil {
		o.MatchedRules = []riskpolicy.RuleMatch{}
	}
	return o
}

// RiskOrderID is the work order and job ID of a task's n-th risk order.
func RiskOrderID(taskID string, n int) string { return fmt.Sprintf("%s-risk-%d", taskID, n) }
