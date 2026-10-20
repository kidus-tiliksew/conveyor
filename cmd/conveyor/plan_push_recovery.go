package main

// Push records, proposal state, and intent recovery for `conveyor plan push`
// (feature-local-planning-drafts LP-2, LP-5 recovery column; req-local-planning
// AC-3.6, AC-3.13; DEC-81(1)). No factory route takes an idempotency key for
// proposals, so an interrupted post is resolved by reading factory state back.

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Push record phases (LP-2).
const (
	PlanPushIntent = "intent"
	PlanPushPosted = "posted"
)

// Proposal states push reports (AC-3.6). A task has no proposal; once posted
// it is created.
const (
	planProposalPending   = "pending"
	planProposalConfirmed = "confirmed"
	planProposalDismissed = "dismissed"
	planProposalCreated   = "created"
)

// planProposalState is the factory state of one posted record.
type planProposalState struct {
	State  string
	Detail string
}

// proposalState reads the factory state of a posted record.
func (p *planPusher) proposalState(ctx context.Context, item PlanItem, record PlanPushRecord) (planProposalState, error) {
	detail := fmt.Sprintf("%s v%d", record.ServerID, record.Version)
	switch item.Kind {
	case PlanKindRequirement:
		requirement, found, err := p.api.requirement(ctx, record.ServerID)
		if err != nil {
			return planProposalState{}, fmt.Errorf("read requirement %s: %w", record.ServerID, err)
		}
		if !found {
			return planProposalState{}, fmt.Errorf("%s: the server no longer holds pushed requirement %s", item.ID, record.ServerID)
		}
		versions, err := p.api.requirementVersions(ctx, record.ServerID)
		if err != nil {
			return planProposalState{}, fmt.Errorf("read requirement %s versions: %w", record.ServerID, err)
		}
		for _, version := range versions {
			if version.Version != record.Version {
				continue
			}
			switch {
			case version.Confirmed:
				return planProposalState{State: planProposalConfirmed, Detail: detail}, nil
			case version.Retired && version.RetiredByVersion > 0:
				return planProposalState{State: planProposalDismissed, Detail: fmt.Sprintf("%s (retired by confirmed v%d)", detail, version.RetiredByVersion)}, nil
			case version.Retired:
				return planProposalState{State: planProposalDismissed, Detail: detail}, nil
			case requirement.CurrentVersion > version.Version:
				return planProposalState{State: planProposalDismissed, Detail: fmt.Sprintf("%s (superseded by confirmed v%d)", detail, requirement.CurrentVersion)}, nil
			}
			return planProposalState{State: planProposalPending, Detail: detail}, nil
		}
		return planProposalState{}, fmt.Errorf("%s: the server holds no version v%d of requirement %s", item.ID, record.Version, record.ServerID)
	case PlanKindSystemDesign:
		design, found, err := p.api.systemDesign(ctx, record.ServerID)
		if err != nil {
			return planProposalState{}, fmt.Errorf("read system design %s: %w", record.ServerID, err)
		}
		if !found {
			return planProposalState{}, fmt.Errorf("%s: the server no longer holds pushed System Design document %s", item.ID, record.ServerID)
		}
		versions, err := p.api.systemDesignVersions(ctx, record.ServerID)
		if err != nil {
			return planProposalState{}, fmt.Errorf("read system design %s versions: %w", record.ServerID, err)
		}
		for _, version := range versions {
			if version.Version != record.Version {
				continue
			}
			switch {
			case version.Confirmed:
				return planProposalState{State: planProposalConfirmed, Detail: detail}, nil
			case version.Dismissed:
				return planProposalState{State: planProposalDismissed, Detail: detail}, nil
			case design.CurrentVersion > version.Version:
				return planProposalState{State: planProposalDismissed, Detail: fmt.Sprintf("%s (superseded by confirmed v%d)", detail, design.CurrentVersion)}, nil
			}
			return planProposalState{State: planProposalPending, Detail: detail}, nil
		}
		return planProposalState{}, fmt.Errorf("%s: the server holds no version v%d of System Design document %s", item.ID, record.Version, record.ServerID)
	case PlanKindDecision:
		decision, found, err := p.api.decision(ctx, record.ServerID)
		if err != nil {
			return planProposalState{}, fmt.Errorf("read decision %s: %w", record.ServerID, err)
		}
		if !found {
			return planProposalState{}, fmt.Errorf("%s: the server no longer holds pushed decision %s", item.ID, record.ServerID)
		}
		switch decision.Status {
		case core.DecisionConfirmed:
			return planProposalState{State: planProposalConfirmed, Detail: decision.ID}, nil
		case core.DecisionSuperseded:
			return planProposalState{State: planProposalConfirmed, Detail: fmt.Sprintf("%s (superseded by %s)", decision.ID, decision.SupersededBy)}, nil
		case core.DecisionDismissed:
			return planProposalState{State: planProposalDismissed, Detail: decision.ID}, nil
		}
		return planProposalState{State: planProposalPending, Detail: decision.ID}, nil
	case PlanKindTask:
		task, found, err := p.api.task(ctx, record.ServerID)
		if err != nil {
			return planProposalState{}, fmt.Errorf("read task %s: %w", record.ServerID, err)
		}
		if !found {
			return planProposalState{}, fmt.Errorf("%s: the server no longer holds pushed task %s", item.ID, record.ServerID)
		}
		return planProposalState{State: planProposalCreated, Detail: fmt.Sprintf("task %s (%s)", task.ID, task.State)}, nil
	}
	return planProposalState{}, fmt.Errorf("%s: unknown item kind %q", item.ID, item.Kind)
}

// planNormalizeContent applies the factory's content normalization for the
// kind: the requirement parser's edge trimming, or
// conveyor:internal/core/system_design.go#NormalizeSystemDesignContent.
func planNormalizeContent(kind PlanItemKind, content string) string {
	if kind == PlanKindRequirement {
		if document, err := planRequirementContent(content); err == nil {
			return document.Markdown
		}
		return strings.TrimSpace(strings.ReplaceAll(content, "\r\n", "\n"))
	}
	return core.NormalizeSystemDesignContent(content)
}

// planDocumentRecovery is the read-back outcome for a document intent.
type planDocumentRecovery struct {
	// Adopted holds the matching version; zero means post again.
	Adopted int
	// Held reports a created document's ID held with no matching version.
	Held bool
}

// recoverDocument resolves a requirement or System Design intent record by
// the LP-5 recovery column. A create adopts the newest version whose
// normalized content equals the request; a 404 posts again. A revise adopts
// the newest such version above base_version; none posts again.
func (p *planPusher) recoverDocument(ctx context.Context, item PlanItem, content string) (planDocumentRecovery, error) {
	type version struct {
		number  int
		content string
	}
	var versions []version
	switch item.Kind {
	case PlanKindRequirement:
		_, found, err := p.api.requirement(ctx, item.ID)
		if err != nil {
			return planDocumentRecovery{}, fmt.Errorf("read requirement %s: %w", item.ID, err)
		}
		if !found {
			return planDocumentRecovery{}, nil
		}
		listed, err := p.api.requirementVersions(ctx, item.ID)
		if err != nil {
			return planDocumentRecovery{}, fmt.Errorf("read requirement %s versions: %w", item.ID, err)
		}
		for _, entry := range listed {
			versions = append(versions, version{entry.Version, entry.Content})
		}
	case PlanKindSystemDesign:
		_, found, err := p.api.systemDesign(ctx, item.ID)
		if err != nil {
			return planDocumentRecovery{}, fmt.Errorf("read system design %s: %w", item.ID, err)
		}
		if !found {
			return planDocumentRecovery{}, nil
		}
		listed, err := p.api.systemDesignVersions(ctx, item.ID)
		if err != nil {
			return planDocumentRecovery{}, fmt.Errorf("read system design %s versions: %w", item.ID, err)
		}
		for _, entry := range listed {
			versions = append(versions, version{entry.Version, entry.Content})
		}
	}
	want := planNormalizeContent(item.Kind, content)
	adopted := 0
	for _, candidate := range versions {
		if item.Mode == PlanModeRevise && candidate.number <= item.BaseVersion {
			continue
		}
		if candidate.number > adopted && planNormalizeContent(item.Kind, candidate.content) == want {
			adopted = candidate.number
		}
	}
	if adopted > 0 {
		return planDocumentRecovery{Adopted: adopted}, nil
	}
	return planDocumentRecovery{Held: item.Mode == PlanModeCreate}, nil
}

// planDecisionEqual compares a server decision with the fields push sends.
func planDecisionEqual(decision core.Decision, body map[string]any) bool {
	field := func(name string) string {
		value, _ := body[name].(string)
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(decision.Statement) == field("statement") &&
		strings.TrimSpace(decision.Context) == field("context") &&
		strings.TrimSpace(decision.AlternativesRejected) == field("alternatives_rejected") &&
		strings.TrimSpace(decision.Supersedes) == field("supersedes")
}

// planIntakeKey is the task intake key `plan:<draft_id>:<item-id>:<digest>`
// (LP-5 task row).
func planIntakeKey(draftID, itemID, digest string) string {
	return "plan:" + draftID + ":" + itemID + ":" + digest
}
