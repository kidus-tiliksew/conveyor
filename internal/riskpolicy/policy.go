// Package riskpolicy holds risk review's pure rules: the workspace risk
// policy and its validation, the .conveyor/risk.yaml declaration, path
// minimums, tier calculation, and requirement calculation. It performs no
// I/O: it reads no store, forge, clock, file, or model. Callers load every
// input and pass it in (component-risk-review, Boundary).
package riskpolicy

import (
	"errors"
	"fmt"
)

// Mode is a risk policy mode (req-risk-based-review REQ-1).
type Mode string

const (
	ModeOff     Mode = "off"
	ModeRecord  Mode = "record"
	ModeEnforce Mode = "enforce"
)

// Valid reports whether m is one of the three closed modes.
func (m Mode) Valid() bool {
	return m == ModeOff || m == ModeRecord || m == ModeEnforce
}

// Tier is a risk tier. Tiers order low, medium, high, and no fourth tier
// exists (component-risk-review invariant 1).
type Tier string

const (
	TierLow    Tier = "low"
	TierMedium Tier = "medium"
	TierHigh   Tier = "high"
)

// Tiers lists every tier from lowest to highest.
var Tiers = []Tier{TierLow, TierMedium, TierHigh}

// Rank orders tiers: low 1, medium 2, high 3. An unknown tier ranks 0.
func (t Tier) Rank() int {
	switch t {
	case TierLow:
		return 1
	case TierMedium:
		return 2
	case TierHigh:
		return 3
	}
	return 0
}

// Valid reports whether t is one of the three closed tiers.
func (t Tier) Valid() bool { return t.Rank() > 0 }

// ParseTier validates a tier name.
func ParseTier(value string) (Tier, error) {
	tier := Tier(value)
	if !tier.Valid() {
		return "", fmt.Errorf("tier must be low, medium, or high")
	}
	return tier, nil
}

// MaxTier returns the higher of two tiers. An invalid tier never wins over a
// valid one.
func MaxTier(a, b Tier) Tier {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// Requirement is one tier's row: the agent reviews and human approvals it
// requires (req-risk-based-review REQ-1).
type Requirement struct {
	AgentReviews   int `json:"agent_reviews" yaml:"agent_reviews"`
	HumanApprovals int `json:"human_approvals" yaml:"human_approvals"`
}

// Table holds one requirement row per tier.
type Table struct {
	Low    Requirement `json:"low" yaml:"low"`
	Medium Requirement `json:"medium" yaml:"medium"`
	High   Requirement `json:"high" yaml:"high"`
}

// Row returns the requirement for tier, and false for an unknown tier.
func (t Table) Row(tier Tier) (Requirement, bool) {
	switch tier {
	case TierLow:
		return t.Low, true
	case TierMedium:
		return t.Medium, true
	case TierHigh:
		return t.High, true
	}
	return Requirement{}, false
}

// DefaultTable is the table a policy turned on without a stated table uses
// (req-risk-based-review AC-1.3).
func DefaultTable() Table {
	return Table{
		Low:    Requirement{AgentReviews: 1, HumanApprovals: 0},
		Medium: Requirement{AgentReviews: 1, HumanApprovals: 1},
		High:   Requirement{AgentReviews: 2, HumanApprovals: 2},
	}
}

// Validate applies req-risk-based-review AC-1.2: every tier has at least one
// agent review and zero or more human approvals, and no value is lower than
// the same value in a lower tier.
func (t Table) Validate() error {
	rows := []Requirement{t.Low, t.Medium, t.High}
	for i, row := range rows {
		name := Tiers[i]
		if row.AgentReviews < 1 {
			return fmt.Errorf("risk_policy.tiers.%s.agent_reviews must be at least 1", name)
		}
		if row.HumanApprovals < 0 {
			return fmt.Errorf("risk_policy.tiers.%s.human_approvals must be zero or more", name)
		}
		if i == 0 {
			continue
		}
		lower := rows[i-1]
		if row.AgentReviews < lower.AgentReviews {
			return fmt.Errorf("risk_policy.tiers.%s.agent_reviews must not be lower than %s", name, Tiers[i-1])
		}
		if row.HumanApprovals < lower.HumanApprovals {
			return fmt.Errorf("risk_policy.tiers.%s.human_approvals must not be lower than %s", name, Tiers[i-1])
		}
	}
	return nil
}

// MaxHumanApprovals returns the largest human approval count in the table.
func (t Table) MaxHumanApprovals() int {
	return max(t.Low.HumanApprovals, t.Medium.HumanApprovals, t.High.HumanApprovals)
}

// Policy is a validated risk policy: the value a task's frozen policy
// contract holds by value from intake (req-risk-based-review REQ-1, AC-1.5).
// It holds no pointer, so a copy never aliases the workspace value.
type Policy struct {
	Mode  Mode  `json:"mode"`
	Tiers Table `json:"tiers"`
}

// Enabled reports whether the policy rates tasks: mode record or enforce.
// A nil policy is the absent key and rates nothing (AC-1.1).
func (p *Policy) Enabled() bool {
	return p != nil && (p.Mode == ModeRecord || p.Mode == ModeEnforce)
}

// Enforcing reports whether the policy's mode is enforce.
func (p *Policy) Enforcing() bool { return p != nil && p.Mode == ModeEnforce }

// Validate checks a policy value built in code.
func (p Policy) Validate() error {
	if !p.Mode.Valid() {
		return errors.New("risk_policy.mode must be off, record, or enforce")
	}
	return p.Tiers.Validate()
}

// Settings is the workspace configuration key risk_policy as an operator
// writes it. Every row and value is a pointer so a missing value is told apart
// from zero: an omitted table takes the AC-1.3 defaults, and an explicit table
// with any missing row or value is refused.
type Settings struct {
	Mode  Mode           `json:"mode" yaml:"mode"`
	Tiers *SettingsTable `json:"tiers,omitempty" yaml:"tiers,omitempty"`
}

// SettingsTable is an explicit table as written.
type SettingsTable struct {
	Low    *SettingsRow `json:"low" yaml:"low"`
	Medium *SettingsRow `json:"medium" yaml:"medium"`
	High   *SettingsRow `json:"high" yaml:"high"`
}

// SettingsRow is one explicit tier row as written.
type SettingsRow struct {
	AgentReviews   *int `json:"agent_reviews" yaml:"agent_reviews"`
	HumanApprovals *int `json:"human_approvals" yaml:"human_approvals"`
}

// Resolve validates the settings and returns the policy they state. Mode off
// keeps a stated table so turning the mode on later reuses it; an omitted
// table takes DefaultTable (AC-1.2, AC-1.3).
func (s Settings) Resolve() (Policy, error) {
	if !s.Mode.Valid() {
		return Policy{}, errors.New("risk_policy.mode must be off, record, or enforce")
	}
	if s.Tiers == nil {
		return Policy{Mode: s.Mode, Tiers: DefaultTable()}, nil
	}
	var table Table
	rows := []struct {
		tier Tier
		row  *SettingsRow
		out  *Requirement
	}{{TierLow, s.Tiers.Low, &table.Low}, {TierMedium, s.Tiers.Medium, &table.Medium}, {TierHigh, s.Tiers.High, &table.High}}
	for _, entry := range rows {
		if entry.row == nil {
			return Policy{}, fmt.Errorf("risk_policy.tiers.%s is required when a table is stated", entry.tier)
		}
		if entry.row.AgentReviews == nil {
			return Policy{}, fmt.Errorf("risk_policy.tiers.%s.agent_reviews is required", entry.tier)
		}
		if entry.row.HumanApprovals == nil {
			return Policy{}, fmt.Errorf("risk_policy.tiers.%s.human_approvals is required", entry.tier)
		}
		*entry.out = Requirement{AgentReviews: *entry.row.AgentReviews, HumanApprovals: *entry.row.HumanApprovals}
	}
	if err := table.Validate(); err != nil {
		return Policy{}, err
	}
	return Policy{Mode: s.Mode, Tiers: table}, nil
}

// SettingsFor returns the explicit settings that state p, so a saved
// configuration always shows the table in effect.
func SettingsFor(p Policy) *Settings {
	row := func(r Requirement) *SettingsRow {
		agent, human := r.AgentReviews, r.HumanApprovals
		return &SettingsRow{AgentReviews: &agent, HumanApprovals: &human}
	}
	return &Settings{Mode: p.Mode, Tiers: &SettingsTable{Low: row(p.Tiers.Low), Medium: row(p.Tiers.Medium), High: row(p.Tiers.High)}}
}

// Clone returns an independent copy of s, or nil for nil.
func (s *Settings) Clone() *Settings {
	if s == nil {
		return nil
	}
	clone := Settings{Mode: s.Mode}
	if s.Tiers == nil {
		return &clone
	}
	cloneInt := func(v *int) *int {
		if v == nil {
			return nil
		}
		next := *v
		return &next
	}
	cloneRow := func(r *SettingsRow) *SettingsRow {
		if r == nil {
			return nil
		}
		return &SettingsRow{AgentReviews: cloneInt(r.AgentReviews), HumanApprovals: cloneInt(r.HumanApprovals)}
	}
	clone.Tiers = &SettingsTable{Low: cloneRow(s.Tiers.Low), Medium: cloneRow(s.Tiers.Medium), High: cloneRow(s.Tiers.High)}
	return &clone
}

// HumanApprovalWarning is the AC-1.4 warning: a saved table needs more human
// approvals than the workspace has people who hold operate_gates.
type HumanApprovalWarning struct {
	Required  int `json:"required"`
	Available int `json:"available"`
}

// CheckHumanApprovals returns the AC-1.4 warning when the policy's largest
// human approval count exceeds the people available, and nil otherwise. The
// policy is saved either way. A policy in mode off rates nothing and warns
// nothing.
func CheckHumanApprovals(p *Policy, available int) *HumanApprovalWarning {
	if !p.Enabled() {
		return nil
	}
	required := p.Tiers.MaxHumanApprovals()
	if required <= available {
		return nil
	}
	return &HumanApprovalWarning{Required: required, Available: available}
}
