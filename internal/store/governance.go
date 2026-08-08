package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

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
			Version: version.Version, Content: version.Content, Governs: append([]core.GovernedScope(nil), version.Governs...), PinnedAtAttachment: pinnedAtAttachment}
	}
	snapshot.Designs = snapshot.Designs[:0]
	for _, design := range byID {
		snapshot.Designs = append(snapshot.Designs, design)
	}
	sort.Slice(snapshot.Designs, func(i, j int) bool { return snapshot.Designs[i].ID < snapshot.Designs[j].ID })
	snapshot.PendingDesignProposals, snapshot.ResolutionNotes, err = SystemDesignProposalEvidenceForTask(ctx, st, taskID)
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

// SystemDesignProposalEvidenceForTask resolves non-authoritative task-origin
// proposal observations independently from pinned governance authority.
func SystemDesignProposalEvidenceForTask(ctx context.Context, st Store, taskID string) ([]core.PendingSystemDesignProposal, []string, error) {
	versions, err := st.ListSystemDesignProposalVersionsForTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	events, err := st.ListSystemDesignProposalEventsForTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	proposalEvents := make(map[string]int64)
	notes := make([]string, 0)
	for _, event := range events {
		var payload struct {
			DocumentID   string `json:"document_id"`
			Version      int    `json:"version"`
			OriginTaskID string `json:"origin_task_id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.DocumentID == "" || payload.Version < 1 || payload.OriginTaskID != taskID {
			notes = append(notes, fmt.Sprintf("proposal event %d was malformed and omitted", event.ID))
			continue
		}
		proposalEvents[payload.DocumentID+":"+strconv.Itoa(payload.Version)] = event.ID
	}
	out := make([]core.PendingSystemDesignProposal, 0)
	for _, version := range versions {
		eventID := proposalEvents[version.DocumentID+":"+strconv.Itoa(version.Version)]
		if eventID == 0 {
			notes = append(notes, fmt.Sprintf("pending proposal %s v%d has no valid proposal event and was omitted", version.DocumentID, version.Version))
			continue
		}
		out = append(out, core.PendingSystemDesignProposal{
			DocumentID: version.DocumentID, Version: version.Version,
			ProposalEventID: eventID, OriginTaskID: taskID, Confirmed: version.Confirmed,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocumentID != out[j].DocumentID {
			return out[i].DocumentID < out[j].DocumentID
		}
		return out[i].Version < out[j].Version
	})
	sort.Strings(notes)
	return out, notes, nil
}
