package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The claim-blocking predicate of req-260810-70ce2f REQ-1 (AC-1.1–AC-1.4),
// stated once over raw versions. Work-order listing, the service's claim
// precheck, each backend's locked claim, and the claim-wait selection behind
// next-action row 8 and the HTTP claim-wait reads all apply it
// (component-work-orders; component-document-corpus). Decisions,
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

// ClaimWaitWindowTasks reports the tasks whose verify or review claim is
// pending or in flight: a submitted implementation, which stays submitted until
// its review round is terminal, or a verify or review order not yet terminal.
func ClaimWaitWindowTasks(orders []core.WorkOrder) map[string]bool {
	result := make(map[string]bool)
	for _, order := range orders {
		switch {
		case order.Stage == core.StageImplement && order.State == core.WorkOrderSubmitted:
			result[order.TaskID] = true
		case (order.Stage == core.StageVerify || order.Stage == core.StageReview) &&
			(order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted):
			result[order.TaskID] = true
		}
	}
	return result
}

// ClaimBlockingProposalReader is the one read the claim-wait selection needs.
type ClaimBlockingProposalReader interface {
	ListClaimBlockingProposalsForTask(ctx context.Context, taskID string) ([]ClaimBlockingProposal, error)
}

// ClaimWaitingProposals is the one claim-wait selection: for each task inside
// its claim window, the versions that withhold its verify and review claims,
// read through ListClaimBlockingProposalsForTask, the read listing and the
// claim precheck use. Next-action row 8 and the proposal_claim_waiting and
// waiting_proposals reads all call it, so they agree with the claim gate
// (req-260810-70ce2f AC-1.1, AC-1.3; req-operator-next-action AC-1.7;
// component-work-orders, Next action). Tasks outside the window cost no read,
// tasks without a withholding version are absent, and a read failure is
// returned.
func ClaimWaitingProposals(ctx context.Context, st ClaimBlockingProposalReader, orders []core.WorkOrder) (map[string][]ClaimBlockingProposal, error) {
	window := ClaimWaitWindowTasks(orders)
	ids := make([]string, 0, len(window))
	for id := range window {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make(map[string][]ClaimBlockingProposal)
	for _, id := range ids {
		items, err := st.ListClaimBlockingProposalsForTask(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			result[id] = items
		}
	}
	return result, nil
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
