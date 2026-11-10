package github

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// DiscoverRiskDeclaration reads .conveyor/risk.yaml at the pull request's base
// SHA with the workspace App token (req-risk-based-review AC-3.2;
// feature-risk-based-review RR-2; component-risk-review invariant 6). It never
// reads the head. The bytes are parsed by internal/riskpolicy, not here: a
// present file is returned as read, an absent file means only the built-in
// minimums apply (AC-3.4), and a malformed, unavailable, or unknown read gives
// high with the returned reason (AC-3.5; invariant 7).
func (c *AppClient) DiscoverRiskDeclaration(ctx context.Context, st AppStore, workspace, slug, revision string) riskpolicy.DeclarationInput {
	found := c.readConveyorFile(ctx, st, workspace, slug, revision, "risk.yaml", riskpolicy.MaxDeclarationBytes)
	switch found.state {
	case "present":
		return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationPresent, Bytes: found.data}
	case "absent":
		return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationAbsent}
	case "malformed":
		return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationMalformed, Reason: found.reason}
	case "unavailable":
		return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationUnavailable, Reason: found.reason}
	}
	return riskpolicy.DeclarationInput{State: riskpolicy.DeclarationUnknown, Reason: found.reason}
}
