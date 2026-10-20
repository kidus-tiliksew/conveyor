package main

// Read-only pre-checks for `conveyor plan push` (feature-local-planning-drafts
// LP-5 pre-check table; req-local-planning AC-3.2, AC-3.8, AC-3.11, AC-3.12,
// AC-3.15). Every rule reads the factory and posts nothing; any refusal stops
// the whole layer before its first post.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var planDecisionTokenPattern = regexp.MustCompile(`\bDEC-[1-9][0-9]*\b`)

// planIntakeKeyLimit is the factory's intake key bound
// (conveyor:internal/httpapi/task_intake.go#createTaskRecord).
const planIntakeKeyLimit = 200

func (p *planPusher) refuse(itemID, rule, format string, args ...any) {
	p.refusals = append(p.refusals, PlanDiagnostic{ItemID: itemID, Rule: rule, Reason: fmt.Sprintf(format, args...)})
}

// loadRequirements lists every requirement, archived ones included, once.
func (p *planPusher) loadRequirements(ctx context.Context) (map[string]core.Requirement, error) {
	if p.requirements != nil {
		return p.requirements, nil
	}
	listed, err := p.api.requirements(ctx)
	if err != nil {
		return nil, fmt.Errorf("list requirements: %w", err)
	}
	p.requirements = map[string]core.Requirement{}
	for _, requirement := range listed {
		p.requirements[requirement.ID] = requirement
	}
	return p.requirements, nil
}

// loadSystemDesigns lists every System Design document, archived ones
// included, once.
func (p *planPusher) loadSystemDesigns(ctx context.Context) (map[string]core.SystemDesign, error) {
	if p.designs != nil {
		return p.designs, nil
	}
	listed, err := p.api.systemDesigns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list system designs: %w", err)
	}
	p.designs = map[string]core.SystemDesign{}
	for _, design := range listed {
		p.designs[design.ID] = design
	}
	return p.designs, nil
}

// loadDecisions lists every decision. refresh rereads the listing, which a
// decision ID collision needs (LP-5 decision row).
func (p *planPusher) loadDecisions(ctx context.Context, refresh bool) (map[string]core.Decision, error) {
	if p.decisions != nil && !refresh {
		return p.decisions, nil
	}
	listed, err := p.api.decisions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	p.decisions = map[string]core.Decision{}
	for _, decision := range listed {
		p.decisions[decision.ID] = decision
		if number, ok := planDecisionNumber(decision.ID); ok && number > p.highestDecision {
			p.highestDecision = number
		}
	}
	return p.decisions, nil
}

func planDecisionNumber(id string) (int, bool) {
	if !planDecisionIDPattern.MatchString(id) {
		return 0, false
	}
	var number int
	if _, err := fmt.Sscanf(id, "DEC-%d", &number); err != nil {
		return 0, false
	}
	return number, true
}

// checkEarlierLayers applies AC-3.8 and AC-3.14: every item of an earlier
// layer that is still in the draft is posted and confirmed. A removed item
// has no draft item, so it no longer blocks.
func (p *planPusher) checkEarlierLayers(ctx context.Context, rank int) error {
	for _, item := range p.draft.Items {
		if planLayerRank(item.Layer) >= rank {
			continue
		}
		record, ok := p.draft.State.Pushes[item.ID]
		switch {
		case !ok:
			p.refuse(item.ID, "AC-3.8", "the %s item is unpushed; run conveyor plan push --layer %s first", item.Layer, item.Layer)
			continue
		case record.Phase != PlanPushPosted:
			p.refuse(item.ID, "AC-3.8", "the %s item's push did not finish (phase %s); rerun conveyor plan push --layer %s", item.Layer, record.Phase, item.Layer)
			continue
		}
		state, err := p.proposalState(ctx, item, record)
		if err != nil {
			return err
		}
		switch state.State {
		case planProposalConfirmed, planProposalCreated:
		case planProposalPending:
			p.refuse(item.ID, "AC-3.8", "the %s item's proposal %s is pending; it must be confirmed first", item.Layer, state.Detail)
		default:
			remedy := "change and push it again"
			if item.Mode == PlanModeCreate {
				remedy = "give it a new document ID and title and push its layer again"
			}
			p.refuse(item.ID, "AC-3.8", "the %s item's proposal %s is %s; %s, or remove the item from the draft", item.Layer, state.Detail, state.State, remedy)
		}
	}
	return nil
}

// checkCitations applies AC-3.2 to one item that will post: every
// requirement, decision, and System Design document it cites outside the
// requested layer is confirmed on the server. Draft items of earlier layers
// are covered by AC-3.8 and are skipped here.
func (p *planPusher) checkCitations(ctx context.Context, item PlanItem) error {
	text := planCitationText(item)
	requirements, err := p.loadRequirements(ctx)
	if err != nil {
		return err
	}
	designs, err := p.loadSystemDesigns(ctx)
	if err != nil {
		return err
	}
	decisions, err := p.loadDecisions(ctx, false)
	if err != nil {
		return err
	}
	draftIDs := map[string]bool{}
	for _, other := range p.draft.Items {
		draftIDs[other.ID] = true
	}

	var citedRequirements []string
	for _, match := range planRequirementDocPattern.FindAllStringIndex(text, -1) {
		if planTokenBoundary(text, match[0], match[1]) {
			citedRequirements = append(citedRequirements, text[match[0]:match[1]])
		}
	}
	citedRequirements = append(citedRequirements, item.Task.RequirementIDs...)
	citedRequirements = append(citedRequirements, planHeldTokens(text, planSortedKeys(requirements))...)
	for _, id := range planUnique(citedRequirements) {
		if draftIDs[id] {
			continue
		}
		requirement, held := requirements[id]
		switch {
		case !held:
			p.refuse(item.ID, "AC-3.2", "cites requirement %s, which the server does not hold", id)
		case requirement.CurrentVersion == 0:
			p.refuse(item.ID, "AC-3.2", "cites requirement %s, which has no confirmed version", id)
		}
	}

	citedDesigns := append([]string(nil), item.Task.SystemDesignIDs...)
	citedDesigns = append(citedDesigns, planHeldTokens(text, planSortedKeys(designs))...)
	for _, id := range planUnique(citedDesigns) {
		if draftIDs[id] {
			continue
		}
		design, held := designs[id]
		switch {
		case !held:
			p.refuse(item.ID, "AC-3.2", "cites System Design document %s, which the server does not hold", id)
		case design.CurrentVersion == 0:
			p.refuse(item.ID, "AC-3.2", "cites System Design document %s, which has no confirmed version", id)
		}
	}

	for _, id := range planUnique(planDecisionTokenPattern.FindAllString(text, -1)) {
		decision, held := decisions[id]
		switch {
		case !held:
			p.refuse(item.ID, "AC-3.2", "cites decision %s, which the server does not hold", id)
		case decision.Status != core.DecisionConfirmed:
			p.refuse(item.ID, "AC-3.2", "cites decision %s, which is %s, not confirmed", id, decision.Status)
		}
	}

	if item.Kind == PlanKindDecision {
		supersedes, present, err := planSupersedes(item.Decision.Supersedes)
		if err == nil && present {
			decision, held := decisions[supersedes]
			switch {
			case !held:
				p.refuse(item.ID, "AC-3.15", "supersedes %s, which the server does not hold", supersedes)
			case decision.Status != core.DecisionConfirmed:
				p.refuse(item.ID, "AC-3.15", "supersedes %s, which is %s, not confirmed", supersedes, decision.Status)
			}
		}
	}
	return nil
}

// planHeldTokens returns the IDs that occur in text as whole tokens.
func planHeldTokens(text string, ids []string) []string {
	var found []string
	for _, id := range ids {
		if len(planTokenPositions(text, id)) > 0 {
			found = append(found, id)
		}
	}
	return found
}

func planSortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func planUnique(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// checkDocument applies AC-3.11 and AC-3.12 to a requirement or System
// Design item that will post.
func (p *planPusher) checkDocument(ctx context.Context, item PlanItem) error {
	if item.Mode == PlanModeCreate {
		return p.checkCreatedDocument(ctx, item)
	}
	if item.Kind == PlanKindRequirement {
		requirement, found, err := p.api.requirement(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("read requirement %s: %w", item.ID, err)
		}
		if !p.checkRevisedDocument(item, found, requirement.Archived, requirement.CurrentVersion) {
			return nil
		}
		versions, err := p.api.requirementVersions(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("read requirement %s versions: %w", item.ID, err)
		}
		contents := map[int]string{}
		var issued []string
		for _, version := range versions {
			contents[version.Version] = version.Content
			for _, statement := range version.Statements {
				issued = append(issued, statement.ID)
				for _, criterion := range statement.AcceptanceCriteria {
					issued = append(issued, criterion.ID)
				}
			}
		}
		p.checkBaseContent(item, contents)
		document, err := planRequirementContent(item.Content)
		if err != nil {
			p.refuse(item.ID, "AC-3.12", "%v", err)
			return nil
		}
		if err := core.ValidateRequirementRevision(requirement.StatementHighWaterMark, issued, document.Statements); err != nil {
			p.refuse(item.ID, "AC-3.12", "%v", err)
		}
		return nil
	}
	design, found, err := p.api.systemDesign(ctx, item.ID)
	if err != nil {
		return fmt.Errorf("read system design %s: %w", item.ID, err)
	}
	if !p.checkRevisedDocument(item, found, design.Archived, design.CurrentVersion) {
		return nil
	}
	versions, err := p.api.systemDesignVersions(ctx, item.ID)
	if err != nil {
		return fmt.Errorf("read system design %s versions: %w", item.ID, err)
	}
	contents := map[int]string{}
	for _, version := range versions {
		contents[version.Version] = version.Content
	}
	p.checkBaseContent(item, contents)
	return nil
}

// checkRevisedDocument applies the AC-3.12 missing and archived rules and the
// AC-3.11 current-version rule; it reports whether the base content check
// can run.
func (p *planPusher) checkRevisedDocument(item PlanItem, found, archived bool, current int) bool {
	noun := planDocumentNoun(item.Kind)
	switch {
	case !found:
		p.refuse(item.ID, "AC-3.12", "revises %s %s, which the server does not hold", noun, item.ID)
		return false
	case archived:
		p.refuse(item.ID, "AC-3.12", "revises %s %s, which is archived", noun, item.ID)
		return false
	case current != item.BaseVersion:
		currentText := "none"
		if current > 0 {
			currentText = fmt.Sprintf("v%d", current)
		}
		p.refuse(item.ID, "AC-3.11", "base version is v%d but the current confirmed version is %s", item.BaseVersion, currentText)
		return false
	}
	return true
}

// checkBaseContent applies the AC-3.11 content rule: the server's base
// version equals the item's `.base.md` copy after the factory normalization.
func (p *planPusher) checkBaseContent(item PlanItem, contents map[int]string) {
	content, ok := contents[item.BaseVersion]
	if !ok {
		p.refuse(item.ID, "AC-3.11", "base version v%d is not on the server; the current confirmed version is v%d", item.BaseVersion, item.BaseVersion)
		return
	}
	if planNormalizeContent(item.Kind, content) != planNormalizeContent(item.Kind, planItemBaseText(item)) {
		p.refuse(item.ID, "AC-3.11", "the server's base version v%d differs from %s/%s%s; the current confirmed version is v%d", item.BaseVersion, item.Layer, item.Name, planBaseSuffix, item.BaseVersion)
	}
}

// checkCreatedDocument applies AC-3.12 to a created document: neither its ID
// nor the slug of its title is held, archived documents included
// (conveyor:internal/core/requirement.go#RequirementSlug).
func (p *planPusher) checkCreatedDocument(ctx context.Context, item PlanItem) error {
	type held struct{ id, slug string }
	var documents []held
	title := item.Title
	if item.Kind == PlanKindRequirement {
		requirements, err := p.loadRequirements(ctx)
		if err != nil {
			return err
		}
		for _, id := range planSortedKeys(requirements) {
			documents = append(documents, held{id, requirements[id].Slug})
		}
		document, err := planRequirementContent(item.Content)
		if err == nil {
			title, _ = core.DocumentHeadingTitle(document.Markdown)
		}
	} else {
		designs, err := p.loadSystemDesigns(ctx)
		if err != nil {
			return err
		}
		for _, id := range planSortedKeys(designs) {
			documents = append(documents, held{id, designs[id].Slug})
		}
	}
	noun := planDocumentNoun(item.Kind)
	slug := core.RequirementSlug(title)
	for _, document := range documents {
		if document.id == item.ID {
			p.refuse(item.ID, "AC-3.12", "creates %s %s, but the server already holds that ID", noun, item.ID)
		}
		if document.slug == slug {
			p.refuse(item.ID, "AC-3.12", "creates %s with title slug %q, which %s %s already holds", noun, slug, noun, document.id)
		}
	}
	return nil
}

func planDocumentNoun(kind PlanItemKind) string {
	if kind == PlanKindRequirement {
		return "requirement"
	}
	return "System Design document"
}

// checkIntakeKey refuses a task whose intake key exceeds the factory bound.
func (p *planPusher) checkIntakeKey(item PlanItem, key string) {
	if len(key) > planIntakeKeyLimit {
		p.refuse(item.ID, "LP-5", "intake key %q is %d characters; the factory accepts at most %d, so shorten the task file name", key, len(key), planIntakeKeyLimit)
	}
}
