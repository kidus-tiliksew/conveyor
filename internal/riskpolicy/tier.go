package riskpolicy

// TierInput holds every input of a task's tier (req-risk-based-review AC-5.5):
// the path minimum at the current head, each approved plan version's tier,
// each tier a review seat stated, each tier a person raised it to, and the
// tier already recorded for the task. An empty or unknown tier in any input
// adds nothing.
type TierInput struct {
	Current      Tier
	PathMinimum  Tier
	PlanTiers    []Tier
	HeadRatings  []Tier
	PersonRaises []Tier
}

// TaskTier returns the largest tier among its inputs, and low when no input
// states one. The current tier is an input, so no later rating, plan, or act
// lowers a task's tier (component-risk-review invariants 3 and 4).
func TaskTier(in TierInput) Tier {
	tier := MaxTier(TierLow, in.Current)
	tier = MaxTier(tier, in.PathMinimum)
	for _, group := range [][]Tier{in.PlanTiers, in.HeadRatings, in.PersonRaises} {
		for _, rated := range group {
			tier = MaxTier(tier, rated)
		}
	}
	return tier
}

// Floors are the requirements a task's frozen pipeline policy gives without
// risk: the frozen minimum review seat count and the frozen merge gate.
type Floors struct {
	SeatMinimum int
	MergeGate   bool
}

// Requirements counts the agent reviews and distinct human approvals a task
// needs.
type Requirements struct {
	AgentReviews   int `json:"agent_reviews"`
	HumanApprovals int `json:"human_approvals"`
}

// RequirementInput holds the inputs of a task's requirements: its frozen risk
// policy (nil when absent or off), its tier, the human approval minimum its
// matched path rules state, and its frozen floors.
type RequirementInput struct {
	Policy             *Policy
	Tier               Tier
	PathHumanApprovals int
	Floors             Floors
}

// RequirementResult separates the requirements in effect from the
// requirements the tier calculates. Effective equals Calculated only when the
// frozen mode is enforce; under off and record the requirements in effect stay
// at the frozen floors (AC-1.1, AC-1.6).
type RequirementResult struct {
	Effective  Requirements `json:"effective"`
	Calculated Requirements `json:"calculated"`
	Enforced   bool         `json:"enforced"`
}

// CalculateRequirements returns a task's requirements. Every count is the
// largest of what its frozen gates and seat minimum require and what its tier
// and matched path rules require, so a rating only adds review
// (req-risk-based-review REQ-2, AC-2.1; component-risk-review invariant 2).
// The function reads no plan state, queue position, or claim (AC-2.2, AC-2.3).
func CalculateRequirements(in RequirementInput) RequirementResult {
	floor := Requirements{AgentReviews: max(in.Floors.SeatMinimum, 0)}
	if in.Floors.MergeGate {
		floor.HumanApprovals = 1
	}
	result := RequirementResult{Effective: floor, Calculated: floor}
	if !in.Policy.Enabled() {
		return result
	}
	row, ok := in.Policy.Tiers.Row(in.Tier)
	if !ok {
		// An unknown tier never counts as low (component-risk-review
		// invariant 7).
		row = in.Policy.Tiers.High
	}
	result.Calculated = Requirements{
		AgentReviews:   max(floor.AgentReviews, row.AgentReviews),
		HumanApprovals: max(floor.HumanApprovals, row.HumanApprovals, in.PathHumanApprovals),
	}
	if in.Policy.Enforcing() {
		result.Effective = result.Calculated
		result.Enforced = true
	}
	return result
}
