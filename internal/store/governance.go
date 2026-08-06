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
	designs, err := st.ListSystemDesigns(ctx)
	if err != nil {
		return snapshot, err
	}
	for _, document := range designs {
		if document.CurrentVersion == 0 {
			continue
		}
		version, getErr := st.GetSystemDesignVersion(ctx, document.ID, document.CurrentVersion)
		if getErr != nil {
			return snapshot, getErr
		}
		for _, scope := range version.Governs {
			if scope.Repository != repository {
				continue
			}
			snapshot.Designs = append(snapshot.Designs, core.GovernanceDesignContext{
				ID: document.ID, Title: document.Title, Category: document.Category,
				Version: version.Version, Content: version.Content,
				Governs: append([]core.GovernedScope(nil), version.Governs...),
			})
			break
		}
	}
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
		byID[item.ID] = core.GovernanceDesignContext{ID: item.ID, Title: document.Title, Category: document.Category,
			Version: version.Version, Content: version.Content, Governs: append([]core.GovernedScope(nil), version.Governs...)}
	}
	snapshot.Designs = snapshot.Designs[:0]
	for _, design := range byID {
		snapshot.Designs = append(snapshot.Designs, design)
	}
	sort.Slice(snapshot.Designs, func(i, j int) bool { return snapshot.Designs[i].ID < snapshot.Designs[j].ID })
	snapshot.PendingDesignProposals, err = pendingSystemDesignProposalsForTask(ctx, st, taskID)
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func pendingSystemDesignProposalsForTask(ctx context.Context, st Store, taskID string) ([]core.PendingSystemDesignProposal, error) {
	versionsByDocument, err := st.ListSystemDesignVersionsByDocument(ctx)
	if err != nil {
		return nil, err
	}
	eventsByDocument, err := st.ListSystemDesignEventsByDocument(ctx)
	if err != nil {
		return nil, err
	}
	proposalEvents := make(map[string]int64)
	for documentID, events := range eventsByDocument {
		for _, event := range events {
			if event.Kind != "system_design.version_proposed" {
				continue
			}
			var payload struct {
				Version      int    `json:"version"`
				OriginTaskID string `json:"origin_task_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.OriginTaskID == taskID {
				proposalEvents[documentID+":"+strconv.Itoa(payload.Version)] = event.ID
			}
		}
	}
	out := make([]core.PendingSystemDesignProposal, 0)
	for documentID, versions := range versionsByDocument {
		for _, version := range versions {
			if version.Origin != core.SystemDesignOriginImplementation || version.OriginTaskID != taskID || version.Confirmed || version.Dismissed {
				continue
			}
			eventID := proposalEvents[documentID+":"+strconv.Itoa(version.Version)]
			if eventID == 0 {
				return nil, fmt.Errorf("pending system design %s v%d has no proposal event", documentID, version.Version)
			}
			out = append(out, core.PendingSystemDesignProposal{
				DocumentID: documentID, Version: version.Version,
				ProposalEventID: eventID, OriginTaskID: taskID,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocumentID != out[j].DocumentID {
			return out[i].DocumentID < out[j].DocumentID
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}
