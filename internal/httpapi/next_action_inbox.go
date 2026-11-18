package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const dismissEarlierVersions = "Confirming a later one dismisses the earlier ones."

type inboxItem struct {
	Kind             string                  `json:"kind,omitempty"`
	Band             string                  `json:"band"`
	Task             *core.Task              `json:"task,omitempty"`
	NextAction       *nextaction.Compact     `json:"next_action,omitempty"`
	PullRequest      *inboxPullRequest       `json:"pull_request,omitempty"`
	Serves           []string                `json:"serves,omitempty"`
	Recommendation   string                  `json:"recommendation,omitempty"`
	NoJudgmentNeeded bool                    `json:"no_judgment_needed,omitempty"`
	Proposal         *inboxProposal          `json:"proposal,omitempty"`
	Label            string                  `json:"label,omitempty"`
	Question         string                  `json:"question,omitempty"`
	IfIgnored        string                  `json:"if_ignored,omitempty"`
	Sentence         string                  `json:"sentence,omitempty"`
	Blockers         []nextaction.BlockerRef `json:"blockers,omitempty"`
	Deadline         *time.Time              `json:"deadline,omitempty"`
	LastEventAt      time.Time               `json:"last_event_at,omitempty"`
	SortID           string                  `json:"-"`
}

// inboxPullRequest is a task item's recorded pull request: the latest
// pull_request.opened folded by the bounded detail summary rule. The task's
// GitHub issue mirror stays on the task and is never stated as the pull
// request (feature-operator-next-action NA-5).
type inboxPullRequest struct {
	URL        string    `json:"url"`
	Number     *jsNumber `json:"number,omitempty"`
	Repository string    `json:"repository,omitempty"`
}

// inboxProposal is a pending document proposal as the inbox states it: the
// stored fields plus its age at the read's clock, omitted when the proposal
// has no proposed time (feature-operator-next-action NA-5;
// req-operator-next-action AC-2.6).
type inboxProposal struct {
	core.PendingProposal
	AgeSeconds *int64 `json:"age_seconds,omitempty"`
}

func (s *Server) listNextActions(w http.ResponseWriter, r *http.Request) {
	band := r.URL.Query().Get("band")
	switch band {
	case "", "decisions", "blocked", "automatic":
	default:
		http.Error(w, "invalid band", http.StatusBadRequest)
		return
	}
	filter, err := parseTaskFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, ok := readListPage(w, r, defaultInboxPageLimit)
	if !ok {
		return
	}
	s.memoizedRead(w, r, "next-actions", []any{band, filter, page}, s.listNextActionsUncached)
}

func (s *Server) listNextActionsUncached(w http.ResponseWriter, r *http.Request) {
	band := r.URL.Query().Get("band")
	switch band {
	case "", "decisions", "blocked", "automatic":
	default:
		http.Error(w, "invalid band", http.StatusBadRequest)
		return
	}
	filter, err := parseTaskFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	page, ok := readListPage(w, r, defaultInboxPageLimit)
	if !ok {
		return
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// The inbox order and bands are computed from the next-action projection
	// over the workspace's open tasks, so no store page can select them. Only
	// open tasks are read, so the read follows open work and not workspace
	// history, and the response carries one page (DEC-61; component-next-action
	// invariants 21, 22).
	items, err := s.readInbox(r.Context(), opts, band, filter)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	total := len(items)
	start := min(page.Offset, total)
	end := min(start+page.Limit, total)
	acts, err := s.Store.ListFactoryActs(r.Context(), time.Now().UTC().Add(-24*time.Hour), 100)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	page.writeHeaders(w, total)
	writeJSON(w, http.StatusOK, map[string]any{"items": items[start:end], "acts": acts})
}

// readInbox builds the operator inbox in server order. GET /v1/next-actions
// and the MCP list_operator_inbox read share it, so the inbox has one
// derivation (DEC-61; feature-operator-next-action NA-8).
func (s *Server) readInbox(ctx context.Context, opts store.NextActionOptions, band string, filter store.TaskFilter) ([]inboxItem, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if s.readMemo == nil {
		return s.readInboxUncached(ctx, opts, band, filter)
	}
	ctx, admitted, key, err := s.memoReadScope(ctx, "inbox", []any{band, filter})
	if err != nil {
		return nil, err
	}
	opts.Capabilities, opts.CallerUserID = admitted.Capabilities, admitted.CallerUserID
	opts.TrustedAuthors = admitted.TrustedAuthors
	value, err := s.readMemo.get(ctx, key, func(ctx context.Context, now time.Time) (readMemoValue, error) {
		horizon := &readMemoHorizon{now: now, until: memoReadUntil(now)}
		if _, loaded := ctx.Value(memoReadValidationKey{}).(store.ActivityValidation); !loaded {
			if _, err := s.Store.ReadActivityValidation(ctx, now); err != nil {
				return readMemoValue{}, err
			}
		}
		ctx = context.WithValue(ctx, readMemoHorizonKey{}, horizon)
		opts.Now = now
		items, err := s.readInboxUncached(ctx, opts, band, filter)
		if err != nil {
			return readMemoValue{}, err
		}
		body, err := json.Marshal(items)
		return readMemoValue{body: body, validUntil: horizon.until}, err
	})
	if err != nil {
		return nil, err
	}
	memoHorizon(ctx, value.validUntil)
	var items []inboxItem
	if err := json.Unmarshal(value.body, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Server) readInboxUncached(ctx context.Context, opts store.NextActionOptions, band string, filter store.TaskFilter) ([]inboxItem, error) {
	// The store excludes merged and closed tasks and blueprint anchors before
	// a row is read, so the inbox's reads cover open tasks only
	// (feature-operator-next-action NA-5).
	filter.Open = true
	candidates, err := s.Store.ListTasksFiltered(ctx, filter)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(candidates))
	for _, task := range candidates {
		ids = append(ids, task.ID)
	}
	inputs, err := s.Store.ReadActivityInputs(ctx, ids, opts.Now)
	if err != nil {
		return nil, err
	}
	memoHorizon(ctx, inputs.ValidUntil)
	opts.MarkersLoaded, opts.Markers = true, inputs.Markers
	opts.OrdersLoaded, opts.Orders = true, inputs.Orders
	proposals, err := s.Store.ListPendingProposals(ctx)
	if err != nil {
		return nil, err
	}
	opts.ProposalsLoaded, opts.Proposals = true, proposals
	projected, err := store.ReadNextActions(ctx, s.Store, candidates, opts)
	if err != nil {
		return nil, err
	}
	lastEvent := map[string]time.Time{}
	for _, marker := range inputs.Markers {
		lastEvent[marker.TaskID] = marker.LastEventAt
	}
	items := inboxTaskItems(candidates, projected, lastEvent)
	if err := s.attachInboxPullRequests(ctx, items); err != nil {
		return nil, err
	}
	items = append(items, documentProposalItems(proposals, opts.Now)...)
	for _, item := range items {
		if item.Deadline != nil {
			memoHorizon(ctx, *item.Deadline)
		}
		if item.Proposal != nil && !item.Proposal.ProposedAt.IsZero() {
			proposed := item.Proposal.ProposedAt
			if proposed.After(opts.Now) {
				memoHorizon(ctx, proposed.Add(time.Second))
			} else {
				memoHorizon(ctx, proposed.Add((opts.Now.Sub(proposed)/time.Second+1)*time.Second))
			}
		}
	}
	if band != "" {
		filtered := make([]inboxItem, 0, len(items))
		for _, item := range items {
			if item.Band == band {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	sortInbox(items)
	return items, nil
}

func inboxTaskItems(candidates []core.Task, projected map[string]nextaction.NextAction, lastEvent map[string]time.Time) []inboxItem {
	var items []inboxItem
	for _, task := range candidates {
		action := projected[task.ID]
		item := inboxItem{
			Task: &task, NextAction: compactPtr(action),
			Serves: serves(task), Deadline: action.Deadline, LastEventAt: lastEvent[task.ID],
			SortID: task.ID, Blockers: action.BlockedBy,
		}
		switch {
		case store.TaskIsInboxDecision(action):
			item.Band = "decisions"
		case action.Actor == nextaction.ActorDependency:
			item.Band = "blocked"
		case action.Actor == nextaction.ActorFactory || (action.Actor == nextaction.ActorOperator && !action.Judgment):
			item.Band = "automatic"
			item.NoJudgmentNeeded = action.Actor == nextaction.ActorOperator && !action.Judgment
		default:
			continue
		}
		items = append(items, item)
	}
	return items
}

// attachInboxPullRequests reads every listed task item's pull_request.opened
// events in one batched read and sets each item's recorded pull request
// through the bounded detail's summary fold (feature-operator-next-action
// NA-5). An inbox without task items issues no read.
func (s *Server) attachInboxPullRequests(ctx context.Context, items []inboxItem) error {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item.Task != nil {
			ids = append(ids, item.Task.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	events, err := s.Store.ListMonitorPullRequestEventsForTasks(ctx, ids)
	if err != nil {
		return err
	}
	for i := range items {
		if items[i].Task != nil {
			items[i].PullRequest = inboxPullRequestFor(events[items[i].Task.ID])
		}
	}
	return nil
}

func inboxPullRequestFor(events []core.Event) *inboxPullRequest {
	if len(events) == 0 {
		return nil
	}
	summary := deriveTaskDetailSummaries(events, nil, nil).PullRequest
	if summary == nil || summary.URL == "" {
		return nil
	}
	return &inboxPullRequest{URL: summary.URL, Number: summary.Number, Repository: summary.Repository}
}

func compactPtr(action nextaction.NextAction) *nextaction.Compact {
	compact := nextaction.CompactFrom(action)
	return &compact
}

func serves(task core.Task) []string {
	ids := make([]string, 0, len(task.Context.Requirements))
	for _, requirement := range task.Context.Requirements {
		ids = append(ids, requirement.ID)
	}
	return ids
}

// documentProposalItems lists each pending document proposal as a decision.
// now is the read's clock (feature-operator-next-action NA-4, NA-5).
func documentProposalItems(proposals []core.PendingProposal, now time.Time) []inboxItem {
	counts := map[string]int{}
	for _, proposal := range proposals {
		if proposal.Tier == "task_context" {
			continue
		}
		counts[proposal.Tier+"/"+proposal.ID]++
	}
	items := make([]inboxItem, 0)
	for _, proposal := range proposals {
		if proposal.Tier == "task_context" {
			continue
		}
		label, question, ifIgnored := nextaction.DocumentProposalCopy(proposal.Tier)
		item := inboxItem{
			Kind: "document_proposal", Band: "decisions",
			Proposal: &inboxProposal{PendingProposal: proposal, AgeSeconds: proposalAge(proposal.ProposedAt, now)},
			Label:    label, Question: question, IfIgnored: ifIgnored,
			LastEventAt: proposal.ProposedAt, SortID: proposal.ID,
		}
		if counts[proposal.Tier+"/"+proposal.ID] > 1 {
			item.Sentence = dismissEarlierVersions
		}
		items = append(items, item)
	}
	return items
}

// proposalAge is the whole seconds from proposedAt to now, clamped at zero
// for a future timestamp, and nil when the proposal has no proposed time.
func proposalAge(proposedAt, now time.Time) *int64 {
	if proposedAt.IsZero() {
		return nil
	}
	age := int64(max(now.Sub(proposedAt), 0) / time.Second)
	return &age
}

func sortInbox(items []inboxItem) {
	for i := 1; i < len(items); i++ {
		item := items[i]
		j := i
		for j > 0 && inboxBefore(item, items[j-1]) {
			items[j] = items[j-1]
			j--
		}
		items[j] = item
	}
}

func inboxBefore(a, b inboxItem) bool {
	if a.Band != b.Band {
		return bandRank(a.Band) < bandRank(b.Band)
	}
	// Items with dependents come first: their compact next action's blocks
	// is the projection's own dependency reading, and a document-proposal
	// item without a task next action has none (feature-operator-next-action
	// NA-5).
	if aBlocks, bBlocks := hasDependents(a), hasDependents(b); aBlocks != bBlocks {
		return aBlocks
	}
	if a.Deadline == nil != (b.Deadline == nil) {
		return a.Deadline != nil
	}
	if a.Deadline != nil && b.Deadline != nil && !a.Deadline.Equal(*b.Deadline) {
		return a.Deadline.Before(*b.Deadline)
	}
	if !a.LastEventAt.Equal(b.LastEventAt) {
		return a.LastEventAt.Before(b.LastEventAt)
	}
	return a.SortID < b.SortID
}

func hasDependents(item inboxItem) bool {
	return item.NextAction != nil && len(item.NextAction.Blocks) > 0
}

func bandRank(band string) int {
	switch band {
	case "decisions":
		return 0
	case "blocked":
		return 1
	default:
		return 2
	}
}
