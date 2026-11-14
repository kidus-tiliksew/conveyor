package riskpolicy

// RiskSubjectKind names what a risk order rates: a plan version or a head
// with its base (feature-risk-based-review RR-11).
type RiskSubjectKind string

const (
	RiskSubjectPlan RiskSubjectKind = "plan"
	RiskSubjectHead RiskSubjectKind = "head"
)

// Valid reports whether k is one of the two subject kinds.
func (k RiskSubjectKind) Valid() bool { return k == RiskSubjectPlan || k == RiskSubjectHead }

// RiskOrderNeedInput holds the values that decide whether a plan version or a
// head needs a risk order (component-risk-review invariant 12). The caller
// reads them; this package performs no I/O.
//
//   - Mode is the task's frozen risk mode.
//   - Tier is the task's tier as it stands. An empty tier means the task has
//     no counting rating yet.
//   - SubjectKind is RiskSubjectPlan or RiskSubjectHead.
//   - ChangedPaths are the changed paths of the head. A plan ignores them.
//   - LatestCompletedHeadPaths are the changed paths that the task's latest
//     completed head risk order rated. Nil means no such order exists. A
//     non-nil empty slice means that order rated a head that changed no path.
//
// A caller that cannot read a head's changed paths in full records high as
// that head's path minimum before it asks, so the tier rule answers no
// (component-risk-review invariant 7; req-risk-based-review AC-5.4).
type RiskOrderNeedInput struct {
	Mode                     Mode
	Tier                     Tier
	SubjectKind              RiskSubjectKind
	ChangedPaths             []string
	LatestCompletedHeadPaths *[]string
}

// NeedsRiskOrder answers whether a plan version or a head needs a risk order
// (component-risk-review invariant 12; req-risk-based-review AC-4.7, AC-5.8,
// AC-5.9, AC-12.3). The answer is no when the mode is off or not a known
// mode, when the tier is high, and for a head when every changed path is
// among the paths that the task's latest completed head risk order rated.
// Paths compare as sets: order and duplicates do not matter. The same inputs
// always give the same answer.
func NeedsRiskOrder(in RiskOrderNeedInput) bool {
	if in.Mode != ModeRecord && in.Mode != ModeEnforce {
		return false
	}
	if in.Tier == TierHigh {
		return false
	}
	switch in.SubjectKind {
	case RiskSubjectPlan:
		return true
	case RiskSubjectHead:
		if in.LatestCompletedHeadPaths == nil {
			return true
		}
		rated := make(map[string]struct{}, len(*in.LatestCompletedHeadPaths))
		for _, path := range *in.LatestCompletedHeadPaths {
			rated[path] = struct{}{}
		}
		for _, path := range in.ChangedPaths {
			if _, ok := rated[path]; !ok {
				return true
			}
		}
		return false
	}
	return false
}
