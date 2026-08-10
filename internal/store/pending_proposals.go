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
			if version.Confirmed || version.Version <= document.CurrentVersion {
				continue
			}
			originType, originID := string(version.Origin), ""
			if version.OriginSessionID != "" {
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
	sortPendingProposals(out)
	return out, nil
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
