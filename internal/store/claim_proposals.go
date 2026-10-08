package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The claim-blocking predicate of req-260810-70ce2f REQ-1 (AC-1.1–AC-1.4),
// stated once over raw versions. Work-order listing, the service's claim
// precheck, each backend's locked claim, and the HTTP claim-wait projection
// all apply it (component-work-orders; component-document-corpus). Decisions,
// task-context proposals, and operator, session, drift, or other-task origins
// never block. A pending version of an archived document still blocks.

// Claim-blocking proposal tiers.
const (
	ClaimBlockingTierSystemDesign = "system_design"
	ClaimBlockingTierRequirement  = "requirement"
)

// ClaimBlockingProposal identifies one undecided task-authored version that
// withholds its task's verify and review claims.
type ClaimBlockingProposal struct {
	Tier    string
	ID      string
	Version int
}

// RequirementVersionWithholdsClaims reports whether an unresolved
// implementation-origin requirement version authored by the task blocks its
// verify and review claims.
func RequirementVersionWithholdsClaims(taskID string, version core.RequirementVersion) bool {
	return version.Origin == core.RequirementOriginImplementation && version.OriginTaskID == taskID && !version.Confirmed && !version.Retired
}

// SystemDesignVersionWithholdsClaims reports whether an unresolved
// implementation-origin System Design version authored by the task blocks its
// verify and review claims.
func SystemDesignVersionWithholdsClaims(taskID string, version core.SystemDesignVersion) bool {
	return version.Origin == core.SystemDesignOriginImplementation && version.OriginTaskID == taskID && !version.Confirmed && !version.Dismissed
}

// SortClaimBlockingProposals orders System Design versions first, then
// requirement versions, each by document ID and version. Every backend
// returns this order, so a claim names the same first proposal everywhere.
func SortClaimBlockingProposals(items []ClaimBlockingProposal) {
	rank := func(tier string) int {
		if tier == ClaimBlockingTierSystemDesign {
			return 0
		}
		return 1
	}
	sort.SliceStable(items, func(i, j int) bool {
		if rank(items[i].Tier) != rank(items[j].Tier) {
			return rank(items[i].Tier) < rank(items[j].Tier)
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		return items[i].Version < items[j].Version
	})
}

// ClaimBlockingProposalError is the refusal a verify or review claim returns
// while the named proposal is undecided.
func ClaimBlockingProposalError(taskID string, proposal ClaimBlockingProposal) error {
	tier := "requirement"
	if proposal.Tier == ClaimBlockingTierSystemDesign {
		tier = "System Design"
	}
	return fmt.Errorf("review for task %s is waiting on task-authored %s proposal %s v%d", taskID, tier, proposal.ID, proposal.Version)
}

func (m *memory) ListClaimBlockingProposalsForTask(ctx context.Context, taskID string) ([]ClaimBlockingProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.claimBlockingProposalsLocked(workspaceOrDefault(ctx, ""), taskID), nil
}

// claimBlockingProposalsLocked applies the predicates to every raw version in
// the workspace, archived documents included, exactly as the claim does.
func (m *memory) claimBlockingProposalsLocked(workspace, taskID string) []ClaimBlockingProposal {
	out := []ClaimBlockingProposal{}
	if workspace == "" {
		return out
	}
	for key, versions := range m.systemDesignVersions {
		if key.workspace != workspace {
			continue
		}
		for _, version := range versions {
			if SystemDesignVersionWithholdsClaims(taskID, version) {
				out = append(out, ClaimBlockingProposal{Tier: ClaimBlockingTierSystemDesign, ID: version.DocumentID, Version: version.Version})
			}
		}
	}
	for key, versions := range m.requirementVersions {
		if key.workspace != workspace {
			continue
		}
		for _, version := range versions {
			if RequirementVersionWithholdsClaims(taskID, version) {
				out = append(out, ClaimBlockingProposal{Tier: ClaimBlockingTierRequirement, ID: version.RequirementID, Version: version.Version})
			}
		}
	}
	SortClaimBlockingProposals(out)
	return out
}
