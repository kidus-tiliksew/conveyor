package main

// `conveyor plan push` (feature-local-planning-drafts LP-5; req-local-planning
// REQ-3 AC-3.1–AC-3.15; DEC-69, DEC-81). Push runs the offline check, then
// every read-only pre-check for one layer, and only then posts that layer's
// items in dependency order as unconfirmed proposals and tasks. Each post is
// bracketed by an intent record and a posted record so a rerun recovers an
// interrupted post by reading factory state back. Push calls no confirm or
// dismiss route (AC-3.4).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// planPushOptions selects the layer and the dry-run mode.
type planPushOptions struct {
	Layer  string
	DryRun bool
	// Now stamps intent records; tests replace it.
	Now func() time.Time
}

type planPushAction int

const (
	// planActionRefused marks an item a pre-check refused; it runs no
	// further check.
	planActionRefused planPushAction = iota
	// planActionPost posts a new proposal or task.
	planActionPost
	// planActionReport reports an unchanged posted item and posts nothing.
	planActionReport
	// planActionAdopt records a version that read-back recovery found.
	planActionAdopt
)

// planPushEntry is one item of the requested layer after the pre-checks.
type planPushEntry struct {
	Item      PlanItem
	Digest    string
	Request   PlanItemRequest
	Action    planPushAction
	Record    PlanPushRecord
	HasRecord bool
	// Adopted is the version recovery found for planActionAdopt.
	Adopted int
	// State is the reported state for planActionReport.
	State planProposalState
}

type planPusher struct {
	api      *planAPI
	draft    *PlanDraft
	opts     planPushOptions
	out      io.Writer
	refusals []PlanDiagnostic

	requirements    map[string]core.Requirement
	designs         map[string]core.SystemDesign
	decisions       map[string]core.Decision
	highestDecision int
	// edges are the same-layer prerequisites of each item.
	edges map[string][]string
	// simulated holds dry-run server IDs for items posted earlier in the
	// same dry run, so later payloads show the replacement.
	simulated map[string]string
}

// runPlanPush runs `conveyor plan push` for one layer of the draft at dir.
func runPlanPush(ctx context.Context, dir string, opts planPushOptions, api *planAPI, out io.Writer) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	rank := planLayerRank(opts.Layer)
	if rank >= len(PlanLayers) {
		return fmt.Errorf("plan push: --layer must be one of %s, not %q", strings.Join(PlanLayers, ", "), opts.Layer)
	}
	draft, diagnostics, err := ReadPlanDraft(ctx, dir)
	if err != nil {
		return err
	}
	checked, err := CheckPlanDraft(ctx, draft)
	if err != nil {
		return err
	}
	failures := 0
	for _, diagnostic := range append(diagnostics, checked...) {
		if !diagnostic.Warning {
			failures++
		}
		if _, err := fmt.Fprintln(out, diagnostic.String()); err != nil {
			return err
		}
	}
	if failures > 0 {
		return fmt.Errorf("plan push: plan check found %d failure(s); nothing posted", failures)
	}
	p := &planPusher{api: api, draft: draft, opts: opts, out: out, simulated: map[string]string{}}
	entries, err := p.prepare(ctx, rank)
	if err != nil {
		return fmt.Errorf("plan push: %w; nothing posted", err)
	}
	if len(p.refusals) > 0 {
		for _, refusal := range p.refusals {
			if _, err := fmt.Fprintln(out, refusal.String()); err != nil {
				return err
			}
		}
		return fmt.Errorf("plan push: the %s layer is refused with %d problem(s); nothing posted", opts.Layer, len(p.refusals))
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintf(out, "plan push: the %s layer has no items\n", opts.Layer)
		return err
	}
	return p.push(ctx, planPushOrder(entries, p.edges))
}

// prepare applies every pre-check for the layer and classifies each item.
// It reads the factory and changes no file.
func (p *planPusher) prepare(ctx context.Context, rank int) ([]*planPushEntry, error) {
	var entries []*planPushEntry
	for _, item := range p.draft.Items {
		if planLayerRank(item.Layer) != rank {
			continue
		}
		digest, err := PlanItemDigest(item)
		if err != nil {
			p.refuse(item.ID, "AC-3.1", "cannot compute a digest: %v", err)
			continue
		}
		approved, ok := p.draft.State.Approvals[item.ID]
		switch {
		case !ok:
			p.refuse(item.ID, "AC-3.1", "the item is pending approval; approve %s@%s first", item.ID, digest)
			continue
		case approved != digest:
			p.refuse(item.ID, "AC-3.1", "the item changed since approval at %s; its current digest is %s", approved, digest)
			continue
		}
		request, err := BuildPlanItemRequest(item)
		if err != nil {
			p.refuse(item.ID, planRuleContent, "%v", err)
			continue
		}
		record, hasRecord := p.draft.State.Pushes[item.ID]
		entries = append(entries, &planPushEntry{Item: item, Digest: digest, Request: request, Record: record, HasRecord: hasRecord})
	}
	if p.draft.State.DraftID == "" && len(entries) > 0 {
		p.refuse(planDraftItemID, "AC-3.1", "the draft records no draft_id; approve its items with conveyor plan approve")
	}
	if err := p.checkEarlierLayers(ctx, rank); err != nil {
		return nil, err
	}
	p.edges = p.checkPostOrder(entries)
	for _, entry := range entries {
		if err := p.classify(ctx, entry); err != nil {
			return nil, err
		}
		if entry.Action != planActionPost {
			continue
		}
		if err := p.checkCitations(ctx, entry.Item); err != nil {
			return nil, err
		}
		switch entry.Item.Kind {
		case PlanKindRequirement, PlanKindSystemDesign:
			if err := p.checkDocument(ctx, entry.Item); err != nil {
				return nil, err
			}
		case PlanKindTask:
			p.checkIntakeKey(entry.Item, p.taskIntakeKey(entry))
		}
	}
	return entries, nil
}

// classify decides the action for one approved item from its push record
// (AC-3.6, AC-3.13; DEC-81(1), DEC-81(3)).
func (p *planPusher) classify(ctx context.Context, entry *planPushEntry) error {
	item, record := entry.Item, entry.Record
	switch {
	case !entry.HasRecord:
		entry.Action = planActionPost
	case record.Phase == PlanPushPosted:
		state, err := p.proposalState(ctx, item, record)
		if err != nil {
			return err
		}
		if record.Digest == entry.Digest {
			entry.Action, entry.State = planActionReport, state
			return nil
		}
		switch state.State {
		case planProposalPending:
			p.refuse(item.ID, "AC-3.13", "the item changed after push while its proposal %s is pending; confirm or dismiss %s first", state.Detail, state.Detail)
		case planProposalConfirmed:
			p.refuse(item.ID, "AC-3.13", "the item changed after its proposal %s was confirmed; start a new revision instead", state.Detail)
		case planProposalCreated:
			p.refuse(item.ID, "AC-3.13", "the item changed after %s was created", state.Detail)
		default:
			// A dismissed proposal is replaced by a new intent and a new
			// proposal once the changed item is approved again (DEC-81(2)).
			entry.Action = planActionPost
		}
	case record.Phase == PlanPushIntent:
		entry.Action = planActionPost
		if item.Kind != PlanKindRequirement && item.Kind != PlanKindSystemDesign {
			// Decisions and tasks recover at their turn to post, once the
			// draft-local IDs they cite are resolved.
			return nil
		}
		// Compare with the content push would send: draft-local IDs
		// resolved from earlier layers' records.
		body, _ := p.sendBody(entry.Request)
		content, _ := body["content"].(string)
		recovery, err := p.recoverDocument(ctx, item, content)
		if err != nil {
			return err
		}
		switch {
		case recovery.Adopted > 0:
			entry.Action, entry.Adopted = planActionAdopt, recovery.Adopted
		case recovery.Held:
			entry.Action = planActionRefused
			p.refuse(item.ID, "AC-3.12", "an earlier push recorded intent to create %s %s; the server holds that ID and no version equals this item", planDocumentNoun(item.Kind), item.ID)
		}
	default:
		p.refuse(item.ID, planRuleLayout, "push record phase %q is neither %s nor %s", record.Phase, PlanPushIntent, PlanPushPosted)
	}
	return nil
}

// taskIntakeKey reuses a recorded intent's key so the factory returns the
// original task (LP-5 task row); otherwise it derives a new one.
func (p *planPusher) taskIntakeKey(entry *planPushEntry) string {
	if entry.HasRecord && entry.Record.Phase == PlanPushIntent && entry.Record.IntakeKey != "" {
		return entry.Record.IntakeKey
	}
	return planIntakeKey(p.draft.State.DraftID, entry.Item.ID, entry.Digest)
}

// planPushPrerequisites maps each item to the items of the same layer it
// cites or depends on, itself included: their server IDs must exist before
// it posts (AC-3.5). Decisions cite by draft-local ID; tasks also list
// depends_on.
func planPushPrerequisites(entries []*planPushEntry) map[string][]string {
	inLayer := map[string]bool{}
	for _, entry := range entries {
		inLayer[entry.Item.ID] = true
	}
	edges := map[string][]string{}
	for _, entry := range entries {
		cited := append(planDraftLocalCitations(planCitationText(entry.Item)), entry.Item.Task.DependsOn...)
		for _, id := range planUnique(cited) {
			if inLayer[id] {
				edges[entry.Item.ID] = append(edges[entry.Item.ID], id)
			}
		}
	}
	return edges
}

// checkPostOrder refuses every item of a citation or dependency cycle within
// the layer, including an item that cites itself: no post order gives each
// cited item its server ID first (AC-3.5). Dry run and push share this
// check, so both refuse before any post (AC-3.3).
func (p *planPusher) checkPostOrder(entries []*planPushEntry) map[string][]string {
	edges := planPushPrerequisites(entries)
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.Item.ID)
	}
	for _, cycle := range planCycles(ids, edges) {
		members := map[string]bool{}
		for _, id := range cycle {
			members[id] = true
		}
		for _, id := range cycle {
			var cited []string
			for _, target := range edges[id] {
				if members[target] {
					cited = append(cited, target)
				}
			}
			p.refuse(id, "AC-3.5", "cites or depends on %s; citations and dependencies in the %s layer form a cycle among %s, so no item of it can post with its cited server IDs", strings.Join(cited, ", "), p.opts.Layer, strings.Join(cycle, ", "))
		}
	}
	return edges
}

// planPushOrder orders the layer topologically over the prerequisites,
// breaking ties by item ID (AC-3.5). checkPostOrder has refused every cycle
// before this runs.
func planPushOrder(entries []*planPushEntry, edges map[string][]string) []*planPushEntry {
	byID := map[string]*planPushEntry{}
	remaining := make([]string, 0, len(entries))
	for _, entry := range entries {
		byID[entry.Item.ID] = entry
		remaining = append(remaining, entry.Item.ID)
	}
	sort.Strings(remaining)
	done := map[string]bool{}
	var ordered []*planPushEntry
	for len(remaining) > 0 {
		next := 0
		for index, id := range remaining {
			ready := true
			for _, prerequisite := range edges[id] {
				if !done[prerequisite] {
					ready = false
					break
				}
			}
			if ready {
				next = index
				break
			}
		}
		id := remaining[next]
		remaining = append(remaining[:next], remaining[next+1:]...)
		done[id] = true
		ordered = append(ordered, byID[id])
	}
	return ordered
}

// resolveDraftLocal returns the server ID for a draft-local item posted
// earlier, from its push record or, in a dry run, the simulated post.
func (p *planPusher) resolveDraftLocal(token string) (string, bool) {
	if id, ok := p.simulated[token]; ok {
		return id, true
	}
	record, ok := p.draft.State.Pushes[token]
	if ok && record.Phase == PlanPushPosted && record.ServerID != "" {
		return record.ServerID, true
	}
	return "", false
}

// planReplaceDraftLocal replaces whole-token draft-local IDs in text and
// returns the tokens it could not resolve.
func planReplaceDraftLocal(text string, resolve func(string) (string, bool)) (string, []string) {
	var out strings.Builder
	var unresolved []string
	last := 0
	for _, match := range planDraftLocalTokenPattern.FindAllStringIndex(text, -1) {
		if match[0] > 0 && planWordByte(text[match[0]-1]) {
			continue
		}
		if match[1] < len(text) && (planWordByte(text[match[1]]) || text[match[1]] == '-') {
			continue
		}
		token := text[match[0]:match[1]]
		id, ok := resolve(token)
		if !ok {
			unresolved = append(unresolved, token)
			continue
		}
		out.WriteString(text[last:match[0]])
		out.WriteString(id)
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String(), unresolved
}

// sendBody copies the item's request body with draft-local IDs replaced at
// send time (AC-3.5). The approved body and digest are not changed.
func (p *planPusher) sendBody(request PlanItemRequest) (map[string]any, []string) {
	body := map[string]any{}
	var unresolved []string
	for name, value := range request.Body {
		switch typed := value.(type) {
		case string:
			replaced, missing := planReplaceDraftLocal(typed, p.resolveDraftLocal)
			body[name], unresolved = replaced, append(unresolved, missing...)
		case []any:
			list := make([]any, 0, len(typed))
			for _, element := range typed {
				text, ok := element.(string)
				if !ok {
					list = append(list, element)
					continue
				}
				replaced, missing := planReplaceDraftLocal(text, p.resolveDraftLocal)
				list, unresolved = append(list, replaced), append(unresolved, missing...)
			}
			body[name] = list
		default:
			body[name] = value
		}
	}
	return body, planUnique(unresolved)
}

// push posts or reports each ordered entry and stops at the first refusal
// or failure, keeping every earlier record (AC-3.10).
func (p *planPusher) push(ctx context.Context, ordered []*planPushEntry) error {
	posted, recovered, unchanged := 0, 0, 0
	for _, entry := range ordered {
		var err error
		switch entry.Action {
		case planActionReport:
			unchanged++
			_, err = fmt.Fprintf(p.out, "%s: unchanged since push; %s is %s\n", entry.Item.ID, entry.State.Detail, entry.State.State)
		case planActionAdopt:
			recovered++
			err = p.adopt(entry, entry.Item.ID, entry.Adopted)
		case planActionPost:
			var adopted bool
			adopted, err = p.post(ctx, entry)
			if adopted {
				recovered++
			} else {
				posted++
			}
		}
		if err != nil {
			return err
		}
	}
	verb := "posted"
	if p.opts.DryRun {
		verb = "would post"
	}
	_, err := fmt.Fprintf(p.out, "plan push: %s layer: %s %d, recovered %d, unchanged %d\n", p.opts.Layer, verb, posted, recovered, unchanged)
	return err
}

// saveRecord replaces the item's push record in one atomic state write.
func (p *planPusher) saveRecord(itemID string, record PlanPushRecord) error {
	previous, had := p.draft.State.Pushes[itemID]
	p.draft.State.Pushes[itemID] = record
	if err := SavePlanState(p.draft.Root, p.draft.State); err != nil {
		if had {
			p.draft.State.Pushes[itemID] = previous
		} else {
			delete(p.draft.State.Pushes, itemID)
		}
		return err
	}
	return nil
}

func (p *planPusher) intentRecord(entry *planPushEntry) PlanPushRecord {
	now := p.opts.Now().UTC()
	return PlanPushRecord{Digest: entry.Digest, Phase: PlanPushIntent, IntentAt: &now}
}

// adopt records a server ID found by read-back without posting (AC-3.6).
func (p *planPusher) adopt(entry *planPushEntry, serverID string, version int) error {
	label := serverID
	if version > 0 {
		label = fmt.Sprintf("%s v%d", serverID, version)
	}
	if p.opts.DryRun {
		p.simulated[entry.Item.ID] = serverID
		_, err := fmt.Fprintf(p.out, "would recover %s: the server already holds it as %s; no post\n", entry.Item.ID, label)
		return err
	}
	record := entry.Record
	record.Digest, record.Phase, record.ServerID, record.Version = entry.Digest, PlanPushPosted, serverID, version
	if err := p.saveRecord(entry.Item.ID, record); err != nil {
		return fmt.Errorf("plan push: %s: record recovered %s: %w", entry.Item.ID, label, err)
	}
	_, err := fmt.Fprintf(p.out, "recovered %s: the server already holds it as %s; no post\n", entry.Item.ID, label)
	return err
}

// post sends one item. It reports adopted when decision recovery found the
// item already on the server.
func (p *planPusher) post(ctx context.Context, entry *planPushEntry) (bool, error) {
	request := entry.Request
	if entry.Item.Kind == PlanKindDecision {
		// The decision's own draft-local ID is replaced by its chosen DEC-n,
		// not resolved as a citation.
		request.Body = maps.Clone(request.Body)
		delete(request.Body, "id")
	}
	body, unresolved := p.sendBody(request)
	if len(unresolved) > 0 && !p.opts.DryRun {
		return false, fmt.Errorf("plan push: %s: cites %s, which has no server ID yet; nothing posted for this item", entry.Item.ID, strings.Join(unresolved, ", "))
	}
	switch entry.Item.Kind {
	case PlanKindDecision:
		return p.postDecision(ctx, entry, body, unresolved)
	case PlanKindTask:
		return false, p.postTask(ctx, entry, body, unresolved)
	}
	return false, p.postDocument(ctx, entry, body)
}

func (p *planPusher) printDryRun(entry *planPushEntry, path string, multipart bool, body map[string]any, intakeKey string, unresolved []string) error {
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return err
	}
	if multipart {
		_, err = fmt.Fprintf(p.out, "would post %s: POST %s (multipart form, no attachments)\nidempotency_key: %s\ntask: %s\n", entry.Item.ID, path, intakeKey, data)
	} else {
		_, err = fmt.Fprintf(p.out, "would post %s: POST %s\n%s\n", entry.Item.ID, path, data)
	}
	if err != nil {
		return err
	}
	for _, token := range unresolved {
		if _, err := fmt.Fprintf(p.out, "unresolved: %s gets its server ID when it is posted earlier in this layer\n", token); err != nil {
			return err
		}
	}
	return nil
}

// refused reports a factory refusal and stops the layer (AC-3.10). A post
// with no factory answer keeps its intent record for recovery.
func (p *planPusher) refused(entry *planPushEntry, err error) error {
	if planAPIStatus(err) == 0 {
		if _, writeErr := fmt.Fprintf(p.out, "%s: the post got no factory answer: %v\n", entry.Item.ID, err); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("plan push: the outcome of posting %s is unknown; rerun push to recover it; later items of the %s layer were not posted", entry.Item.ID, p.opts.Layer)
	}
	if _, writeErr := fmt.Fprintf(p.out, "%s: refused by the factory: %v\n", entry.Item.ID, err); writeErr != nil {
		return writeErr
	}
	return fmt.Errorf("plan push: %s was refused; later items of the %s layer were not posted", entry.Item.ID, p.opts.Layer)
}

func (p *planPusher) posted(entry *planPushEntry, record PlanPushRecord, label string) error {
	if err := p.saveRecord(entry.Item.ID, record); err != nil {
		return fmt.Errorf("plan push: %s: the factory accepted it as %s but the posted record was not saved: %w; rerun push to recover it", entry.Item.ID, label, err)
	}
	_, err := fmt.Fprintf(p.out, "posted %s: %s\n", entry.Item.ID, label)
	return err
}

// postDocument posts a requirement or System Design item.
func (p *planPusher) postDocument(ctx context.Context, entry *planPushEntry, body map[string]any) error {
	path := entry.Request.Path
	if p.opts.DryRun {
		return p.printDryRun(entry, path, false, body, "", nil)
	}
	record := p.intentRecord(entry)
	if err := p.saveRecord(entry.Item.ID, record); err != nil {
		return fmt.Errorf("plan push: %s: record intent: %w; nothing posted for this item", entry.Item.ID, err)
	}
	var answer json.RawMessage
	if err := p.api.postJSON(ctx, path, body, &answer); err != nil {
		return p.refused(entry, err)
	}
	serverID, version, err := planDocumentAnswer(entry.Item, answer)
	if err != nil {
		return fmt.Errorf("plan push: %s: the factory accepted the post but its answer is unreadable: %w; rerun push to recover it", entry.Item.ID, err)
	}
	record.Phase, record.ServerID, record.Version = PlanPushPosted, serverID, version
	return p.posted(entry, record, fmt.Sprintf("%s v%d (pending)", serverID, version))
}

// planDocumentAnswer reads the server ID and version from a create answer
// (`{document|requirement, version}`) or a revise answer (the version).
func planDocumentAnswer(item PlanItem, answer json.RawMessage) (string, int, error) {
	var version struct {
		RequirementID string `json:"requirement_id"`
		DocumentID    string `json:"document_id"`
		Version       int    `json:"version"`
	}
	target := answer
	if item.Mode == PlanModeCreate {
		var wrapped struct {
			Version json.RawMessage `json:"version"`
		}
		if err := json.Unmarshal(answer, &wrapped); err != nil {
			return "", 0, err
		}
		target = wrapped.Version
	}
	if err := json.Unmarshal(target, &version); err != nil {
		return "", 0, err
	}
	id := version.RequirementID
	if item.Kind == PlanKindSystemDesign {
		id = version.DocumentID
	}
	if id == "" || version.Version < 1 {
		return "", 0, fmt.Errorf("answer names no document ID and version")
	}
	return id, version.Version, nil
}

// nextDecisionID chooses one above the highest DEC-n the factory lists or
// this run already chose (LP-5 decision row).
func (p *planPusher) nextDecisionID(ctx context.Context, refresh bool) (string, error) {
	if _, err := p.loadDecisions(ctx, refresh); err != nil {
		return "", err
	}
	p.highestDecision++
	return fmt.Sprintf("DEC-%d", p.highestDecision), nil
}

// planDecisionAttempts bounds ID collisions within one post.
const planDecisionAttempts = 5

// postDecision posts a decision with a chosen DEC-n. A recorded intent is
// resolved first: an equal decision at chosen_id is adopted, a 404 posts with
// that ID, and different fields choose a new ID. A 409 on post whose ID is
// now held chooses a new ID and records a new intent.
func (p *planPusher) postDecision(ctx context.Context, entry *planPushEntry, body map[string]any, unresolved []string) (bool, error) {
	chosen := ""
	if entry.HasRecord && entry.Record.Phase == PlanPushIntent && entry.Record.ChosenID != "" {
		existing, found, err := p.api.decision(ctx, entry.Record.ChosenID)
		if err != nil {
			return false, fmt.Errorf("plan push: %s: read decision %s: %w", entry.Item.ID, entry.Record.ChosenID, err)
		}
		switch {
		case found && planDecisionEqual(existing, body):
			return true, p.adopt(entry, existing.ID, 0)
		case !found:
			chosen = entry.Record.ChosenID
			if number, ok := planDecisionNumber(chosen); ok {
				if _, err := p.loadDecisions(ctx, false); err != nil {
					return false, err
				}
				p.highestDecision = max(p.highestDecision, number)
			}
		}
	}
	for attempt := 0; attempt < planDecisionAttempts; attempt++ {
		if chosen == "" {
			id, err := p.nextDecisionID(ctx, attempt > 0)
			if err != nil {
				return false, fmt.Errorf("plan push: %s: %w", entry.Item.ID, err)
			}
			chosen = id
		}
		body["id"] = chosen
		if p.opts.DryRun {
			p.simulated[entry.Item.ID] = chosen
			return false, p.printDryRun(entry, entry.Request.Path, false, body, "", unresolved)
		}
		record := p.intentRecord(entry)
		record.ChosenID = chosen
		if err := p.saveRecord(entry.Item.ID, record); err != nil {
			return false, fmt.Errorf("plan push: %s: record intent: %w; nothing posted for this item", entry.Item.ID, err)
		}
		var decision core.Decision
		err := p.api.postJSON(ctx, entry.Request.Path, body, &decision)
		if planAPIStatus(err) == http.StatusConflict {
			if _, held, readErr := p.api.decision(ctx, chosen); readErr == nil && held {
				chosen = ""
				continue
			}
		}
		if err != nil {
			return false, p.refused(entry, err)
		}
		serverID := decision.ID
		if serverID == "" {
			serverID = chosen
		}
		record.Phase, record.ServerID = PlanPushPosted, serverID
		return false, p.posted(entry, record, serverID+" (pending)")
	}
	return false, fmt.Errorf("plan push: %s: %d chosen decision IDs were taken while posting; rerun push", entry.Item.ID, planDecisionAttempts)
}

// postTask posts a task through multipart intake. A recorded intent posts
// again with the same intake key, so the factory returns the original task
// (conveyor:internal/httpapi/task_intake.go#sameIntakeRequest).
func (p *planPusher) postTask(ctx context.Context, entry *planPushEntry, body map[string]any, unresolved []string) error {
	key := p.taskIntakeKey(entry)
	if p.opts.DryRun {
		return p.printDryRun(entry, entry.Request.Path, true, body, key, unresolved)
	}
	record := p.intentRecord(entry)
	record.IntakeKey = key
	if err := p.saveRecord(entry.Item.ID, record); err != nil {
		return fmt.Errorf("plan push: %s: record intent: %w; nothing posted for this item", entry.Item.ID, err)
	}
	var task core.Task
	if err := p.api.postTask(ctx, body, key, &task); err != nil {
		return p.refused(entry, err)
	}
	if task.ID == "" {
		return fmt.Errorf("plan push: %s: the factory accepted the task but its answer names no task ID; rerun push to recover it", entry.Item.ID)
	}
	record.Phase, record.ServerID = PlanPushPosted, task.ID
	return p.posted(entry, record, fmt.Sprintf("task %s (%s)", task.ID, task.State))
}
