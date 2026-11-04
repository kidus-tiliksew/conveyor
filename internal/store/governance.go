package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// GovernanceForRepository resolves one deterministic authority snapshot for a
// review claim or an unclaimed prompt preview.
func GovernanceForRepository(ctx context.Context, st Store, repository string) (core.GovernanceSnapshot, error) {
	snapshot := core.GovernanceSnapshot{
		Designs:                make([]core.GovernanceDesignContext, 0),
		Decisions:              make([]core.Decision, 0),
		PendingDesignProposals: make([]core.PendingSystemDesignProposal, 0),
	}
	designs, err := st.ListGovernanceDesigns(ctx, repository)
	if err != nil {
		return snapshot, err
	}
	snapshot.Designs = append(snapshot.Designs, designs...)
	decisions, err := st.ListDecisions(ctx)
	if err != nil {
		return snapshot, err
	}
	for _, decision := range decisions {
		if decision.Status == core.DecisionConfirmed || decision.Status == core.DecisionSuperseded {
			snapshot.Decisions = append(snapshot.Decisions, decision)
		}
	}
	sort.Slice(snapshot.Designs, func(i, j int) bool { return snapshot.Designs[i].ID < snapshot.Designs[j].ID })
	sort.Slice(snapshot.Decisions, func(i, j int) bool { return snapshot.Decisions[i].ID < snapshot.Decisions[j].ID })
	return snapshot, nil
}

// GovernanceForTask merges repository-glob authority with the task's pinned
// design attachments. Document identity deduplicates overlaps deterministically.
func GovernanceForTask(ctx context.Context, st Store, taskID, repository string) (core.GovernanceSnapshot, error) {
	snapshot, err := GovernanceForRepository(ctx, st, repository)
	if err != nil {
		return snapshot, err
	}
	attached, err := TaskContextForTask(ctx, st, taskID)
	if err != nil {
		return snapshot, err
	}
	byID := map[string]core.GovernanceDesignContext{}
	for _, design := range snapshot.Designs {
		byID[design.ID] = design
	}
	for _, item := range attached.Designs {
		document, getErr := st.GetSystemDesign(ctx, item.ID)
		if getErr != nil {
			return snapshot, getErr
		}
		version, getErr := st.GetSystemDesignVersion(ctx, item.ID, item.Version)
		if getErr != nil {
			return snapshot, getErr
		}
		pinnedAtAttachment := false
		if repositoryAuthority, exists := byID[item.ID]; exists && repositoryAuthority.Version > version.Version {
			pinnedAtAttachment = true
		}
		byID[item.ID] = core.GovernanceDesignContext{ID: item.ID, Title: document.Title, Category: document.Category,
			Version: version.Version, Content: version.Content, Governs: append([]core.GovernedScope(nil), version.Governs...), PinnedAtAttachment: pinnedAtAttachment,
			Archived: document.Archived, SupersededBy: append([]string(nil), document.SupersededBy...)}
	}
	snapshot.Designs = snapshot.Designs[:0]
	for _, design := range byID {
		snapshot.Designs = append(snapshot.Designs, design)
	}
	sort.Slice(snapshot.Designs, func(i, j int) bool { return snapshot.Designs[i].ID < snapshot.Designs[j].ID })
	return snapshot, nil
}

// EncodeGovernanceSnapshot returns the lowercase hex SHA-256 of the canonical
// JSON already produced for work-order rows, plus those bytes. A nil snapshot
// has no digest and is not stored.
func EncodeGovernanceSnapshot(snapshot *core.GovernanceSnapshot) (digest string, content []byte) {
	if snapshot == nil {
		return "", nil
	}
	content = core.JSONPayload(snapshot)
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), content
}

// PinClaimGovernance applies the claim-time governance pin. A missing pin or
// a verify repin replaces the whole snapshot; otherwise the frozen snapshot
// stays exactly as pinned (component-work-orders).
func PinClaimGovernance(order *core.WorkOrder, governance *core.GovernanceSnapshot, repin bool) {
	if governance == nil {
		return
	}
	if order.GovernanceSnapshot == nil || repin {
		copy := *governance
		copy.Designs = append([]core.GovernanceDesignContext(nil), governance.Designs...)
		copy.Decisions = append([]core.Decision(nil), governance.Decisions...)
		copy.PendingDesignProposals = append([]core.PendingSystemDesignProposal(nil), governance.PendingDesignProposals...)
		copy.ResolutionNotes = append([]string(nil), governance.ResolutionNotes...)
		order.GovernanceSnapshot = &copy
	}
}
