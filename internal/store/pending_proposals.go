package store

import (
	"context"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (m *memory) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listPendingProposalsLocked(workspaceOrDefault(ctx, ""), nil), nil
}

// ListPendingProposalsForDocuments selects the pending requirement and System
// Design versions of the named documents whatever their origin: the only
// proposals next-action projection reads are those an authority-conflict
// citation names. Empty IDs select nothing, never the workspace queue.
func (m *memory) ListPendingProposalsForDocuments(ctx context.Context, documentIDs []string) ([]core.PendingProposal, error) {
	if len(documentIDs) == 0 {
		return []core.PendingProposal{}, nil
	}
	documents := make(map[string]bool, len(documentIDs))
	for _, id := range documentIDs {
		documents[id] = true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.listPendingProposalsLocked(workspaceOrDefault(ctx, ""), documents), nil
}

// listPendingProposalsLocked selects the workspace's pending proposals; a
// non-nil documents map keeps only requirement and System Design versions of
// those documents.
func (m *memory) listPendingProposalsLocked(workspace string, documents map[string]bool) []core.PendingProposal {
	out := make([]core.PendingProposal, 0)
	for key, versions := range m.systemDesignVersions {
		if key.workspace != workspace || (documents != nil && !documents[key.id]) {
			continue
		}
		document := m.systemDesigns[key]
		if document.Archived {
			continue
		}
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
		if key.workspace != workspace || (documents != nil && !documents[key.id]) {
			continue
		}
		document := m.requirements[key]
		if document.Archived {
			continue
		}
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
		if documents != nil || key.workspace != workspace || decision.Status != core.DecisionProposed {
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
		if documents != nil || proposal.Workspace != workspace || proposal.State != core.TaskContextProposalProposed {
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
	return out
}

func (m *memory) PendingProposalsProjection(ctx context.Context) (PendingProposalsProjection, error) {
	proposals, err := m.ListPendingProposals(ctx)
	if err != nil {
		return PendingProposalsProjection{}, err
	}
	return PendingProposalsProjection{Items: proposals}, nil
}

func (m *memory) CountOperatorAttention(ctx context.Context) (int, error) {
	return CountOperatorAttention(ctx, m)
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

// PendingProposalsProjection is the bounded store read used by the workspace
// attention badge. Age remains an HTTP concern so the store returns durable
// proposal timestamps unchanged.
type PendingProposalsProjection struct {
	Items []core.PendingProposal
}
