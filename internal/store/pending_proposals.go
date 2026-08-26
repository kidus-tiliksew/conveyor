package store

import (
	"context"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (m *memory) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace := workspaceOrDefault(ctx, "")
	out := make([]core.PendingProposal, 0)
	for key, versions := range m.systemDesignVersions {
		if key.workspace != workspace {
			continue
		}
		document := m.systemDesigns[key]
		for _, version := range versions {
			if version.Confirmed || version.Dismissed {
				continue
			}
			originType, originID := "operator", ""
			if version.OriginTaskID != "" {
				originType, originID = "task", version.OriginTaskID
			} else if version.OriginSessionID != "" {
				originType, originID = "session", version.OriginSessionID
			}
			out = append(out, core.PendingProposal{ID: key.id, Title: document.Title, Tier: "system_design", Version: version.Version, OriginType: originType, OriginID: originID, ProposedAt: version.CreatedAt})
		}
	}
	for key, versions := range m.requirementVersions {
		if key.workspace != workspace {
			continue
		}
		document := m.requirements[key]
		for _, version := range versions {
			// Older unconfirmed revisions become unactionable once intent moves
			// past them; a newer still-pending revision remains independently
			// visible (AC-1.3).
			if version.Confirmed || version.Retired || version.Version <= document.CurrentVersion {
				continue
			}
			originType, originID := string(version.Origin), ""
			if version.OriginTaskID != "" {
				originType, originID = "task", version.OriginTaskID
			} else if version.OriginSessionID != "" {
				originType, originID = "session", version.OriginSessionID
			} else if version.OriginDriftID != "" {
				originType, originID = "drift", version.OriginDriftID
			}
			out = append(out, core.PendingProposal{ID: key.id, Title: document.Title, Tier: "requirement", Version: version.Version, OriginType: originType, OriginID: originID, ProposedAt: version.CreatedAt})
		}
	}
	for key, decision := range m.decisions {
		if key.workspace != workspace || decision.Status != core.DecisionProposed {
			continue
		}
		originType, originID := "operator", ""
		if decision.OriginTaskID != "" {
			originType, originID = "task", decision.OriginTaskID
		} else if decision.OriginSessionID != "" {
			originType, originID = "session", decision.OriginSessionID
		}
		out = append(out, core.PendingProposal{ID: decision.ID, Title: decision.Statement, Tier: "decision", OriginType: originType, OriginID: originID, ProposedAt: decision.CreatedAt})
	}
	for _, proposal := range m.taskContextProposals {
		if proposal.Workspace != workspace || proposal.State != core.TaskContextProposalProposed {
			continue
		}
		task, ok := m.tasks[proposal.TaskID]
		if !ok || core.TaskTerminal(task.State) {
			continue
		}
		out = append(out, core.PendingProposal{ID: proposal.TargetID, Title: proposal.TargetTitle, Tier: "task_context", OriginType: "task", OriginID: proposal.TaskID,
			TargetKind: string(proposal.TargetKind), Justification: proposal.Justification, ProposedAt: proposal.CreatedAt})
	}
	sortPendingProposals(out)
	return out, nil
}

func (m *memory) PendingProposalsProjection(ctx context.Context) (PendingProposalsProjection, error) {
	proposals, err := m.ListPendingProposals(ctx)
	if err != nil {
		return PendingProposalsProjection{}, err
	}
	tasks, err := m.ListTasks(ctx)
	if err != nil {
		return PendingProposalsProjection{}, err
	}
	markers, err := m.ListActivityMarkers(ctx)
	if err != nil {
		return PendingProposalsProjection{}, err
	}
	orders, err := m.ListWorkOrders(ctx)
	if err != nil {
		return PendingProposalsProjection{}, err
	}
	pendingAuthority := pendingAuthorityTaskIDs(orders, proposals)
	contextAttention := pendingTaskContextIDs(proposals)
	byTask := make(map[string]ActivityMarker, len(markers))
	for _, marker := range markers {
		byTask[marker.TaskID] = marker
	}
	workspace := workspaceOrDefault(ctx, "")
	count := 0
	for _, task := range tasks {
		if task.Workspace != workspace {
			continue
		}
		if core.BlueprintAnchor(task) {
			continue
		}
		marker := byTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		if TaskNeedsAttention(task, marker, pendingAuthority[task.ID], contextAttention[task.ID]) {
			count++
		}
	}
	return PendingProposalsProjection{Items: proposals, TaskCount: count}, nil
}

func pendingTaskContextIDs(proposals []core.PendingProposal) map[string]bool {
	result := make(map[string]bool)
	for _, proposal := range proposals {
		if proposal.Tier == "task_context" && proposal.OriginType == "task" && proposal.OriginID != "" {
			result[proposal.OriginID] = true
		}
	}
	return result
}

func pendingAuthorityTaskIDs(orders []core.WorkOrder, proposals []core.PendingProposal) map[string]bool {
	origins := make(map[string]bool)
	for _, proposal := range proposals {
		if proposal.OriginType == "task" && proposal.OriginID != "" {
			origins[proposal.OriginID] = true
		}
	}
	result := make(map[string]bool)
	for _, order := range orders {
		if !origins[order.TaskID] {
			continue
		}
		if order.Stage == core.StageImplement && order.State == core.WorkOrderSubmitted ||
			order.Stage == core.StageReview && (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted) {
			result[order.TaskID] = true
		}
	}
	return result
}

func sortPendingProposals(items []core.PendingProposal) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].ProposedAt.Equal(items[j].ProposedAt) {
			return items[i].ProposedAt.Before(items[j].ProposedAt)
		}
		if items[i].Tier != items[j].Tier {
			return items[i].Tier < items[j].Tier
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		return items[i].Version < items[j].Version
	})
}
