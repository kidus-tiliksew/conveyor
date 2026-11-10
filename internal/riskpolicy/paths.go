package riskpolicy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/governedpath"
)

// DeclarationState is what a caller found when it read the declaration at a
// pull request's base SHA. The states mirror the documentation-policy
// discovery states (feature-risk-based-review RR-2).
type DeclarationState string

const (
	DeclarationPresent     DeclarationState = "present"
	DeclarationAbsent      DeclarationState = "absent"
	DeclarationMalformed   DeclarationState = "malformed"
	DeclarationUnavailable DeclarationState = "unavailable"
	DeclarationUnknown     DeclarationState = "unknown"
)

// DeclarationInput carries the declaration a caller read at the base SHA.
// Bytes are read only when State is present. Reason describes any other
// state but absent.
type DeclarationInput struct {
	State  DeclarationState
	Bytes  []byte
	Reason string
}

// PathInput holds every input of a head's path minimums. ChangedPathsComplete
// is false when the caller could not read the changed-path list in full.
type PathInput struct {
	Declaration          DeclarationInput
	ChangedPaths         []string
	ChangedPathsComplete bool
	// ChangedPathsReason describes why the list is incomplete.
	ChangedPathsReason string
}

// RuleSource names where a matched rule comes from.
type RuleSource string

const (
	RuleBuiltin  RuleSource = "builtin"
	RuleDeclared RuleSource = "declaration"
)

// RuleMatch is the evidence that one rule matched at least one changed path.
type RuleMatch struct {
	Source            RuleSource `json:"source"`
	Index             int        `json:"index"`
	Description       string     `json:"description"`
	MinTier           Tier       `json:"min_tier"`
	MinHumanApprovals int        `json:"min_human_approvals"`
	Paths             []string   `json:"paths"`
}

// PathMinimum is a head's path minimum: the lowest tier and the fewest human
// approvals its changed paths allow, the rules that matched, and the reason
// for a high fallback.
type PathMinimum struct {
	Tier           Tier        `json:"tier"`
	HumanApprovals int         `json:"human_approvals"`
	Matches        []RuleMatch `json:"matches"`
	Reasons        []string    `json:"reasons,omitempty"`
}

// PathMinimums calculates a head's path minimums. Built-in minimums always
// apply (AC-3.3, AC-3.4). A declaration that is malformed, unavailable, or in
// an unknown state, or a changed-path list that is not complete, gives high
// with a recorded reason; it never refuses anything (AC-3.5, AC-5.4;
// component-risk-review invariant 7). The result is deterministic over its
// inputs (invariant 3).
func PathMinimums(in PathInput) PathMinimum {
	result := PathMinimum{Tier: TierLow, Matches: []RuleMatch{}}
	paths := normalizedPaths(in.ChangedPaths)
	apply := func(source RuleSource, rules []Rule) {
		for index, rule := range rules {
			matched := matchRule(rule, paths)
			if len(matched) == 0 {
				continue
			}
			human := 0
			if rule.MinHumanApprovals != nil {
				human = *rule.MinHumanApprovals
			}
			result.Matches = append(result.Matches, RuleMatch{
				Source: source, Index: index, Description: rule.Description,
				MinTier: rule.MinTier, MinHumanApprovals: human, Paths: matched,
			})
			result.Tier = MaxTier(result.Tier, rule.MinTier)
			result.HumanApprovals = max(result.HumanApprovals, human)
		}
	}
	apply(RuleBuiltin, BuiltinRules())
	switch in.Declaration.State {
	case DeclarationAbsent:
	case DeclarationPresent:
		declaration, err := ParseDeclaration(in.Declaration.Bytes)
		if err != nil {
			result.fallback(fmt.Sprintf("declaration is malformed: %v", err))
		} else {
			apply(RuleDeclared, declaration.Rules)
		}
	case DeclarationMalformed:
		result.fallback(describe("declaration is malformed", in.Declaration.Reason))
	case DeclarationUnavailable:
		result.fallback(describe("declaration could not be read", in.Declaration.Reason))
	default:
		result.fallback(describe("declaration state is unknown", in.Declaration.Reason))
	}
	if !in.ChangedPathsComplete {
		result.fallback(describe("changed paths could not be read in full", in.ChangedPathsReason))
	}
	return result
}

func (p *PathMinimum) fallback(reason string) {
	p.Tier = TierHigh
	p.Reasons = append(p.Reasons, reason)
}

func describe(base, reason string) string {
	if reason = strings.TrimSpace(reason); reason != "" {
		return base + ": " + reason
	}
	return base
}

func normalizedPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func matchRule(rule Rule, paths []string) []string {
	matchers := make([]governedpath.Matcher, 0, len(rule.Paths))
	for _, glob := range rule.Paths {
		matcher, err := governedpath.Compile(glob)
		if err != nil {
			continue
		}
		matchers = append(matchers, matcher)
	}
	var matched []string
	for _, path := range paths {
		for _, matcher := range matchers {
			if matcher.Match(path) {
				matched = append(matched, path)
				break
			}
		}
	}
	return matched
}
