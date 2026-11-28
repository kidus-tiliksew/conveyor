package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Report is the served report of a feedback_report item, from the one
	// served-report function (component-feedback-reports invariant 21).
	Report *servedFeedbackReport `json:"report,omitempty"`
	SortID string                `json:"-"`
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

// factoryActWindow is how far back the inbox's factory act log reads.
const factoryActWindow = 24 * time.Hour

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
	acts, err := s.Store.ListFactoryActs(r.Context(), opts.Now.Add(-factoryActWindow), 100)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	// An act leaves the window factoryActWindow after it happened, so the
	// response is not reused past the first such instant
	// (component-next-action invariant 58).
	for _, act := range acts {
		memoHorizon(r.Context(), act.At.Add(factoryActWindow))
	}
	page.writeHeaders(w, total)
	s.writeTaskJSON(w, r, http.StatusOK, map[string]any{"items": items[start:end], "acts": acts})
}

// readInbox builds the operator inbox in server order. GET /v1/next-actions
// and the MCP list_operator_inbox read share it, so the inbox has one
// derivation (DEC-61; feature-operator-next-action NA-8).
func (s *Server) readInbox(ctx context.Context, opts store.NextActionOptions, band string, filter store.TaskFilter) ([]inboxItem, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if s.readMemo == nil {
		items, err := s.readInboxUncached(ctx, opts, band, filter)
		if err != nil {
			return nil, err
		}
		// Each item's task carries its crate's current name, for the inbox
		// route and for MCP list_operator_inbox (component-crates).
		if err := s.nameTaskCrates(ctx, &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	ctx, admitted, key, err := s.memoReadScope(ctx, "inbox", []any{band, filter})
	if err != nil {
		return nil, err
	}
	opts.Capabilities, opts.CallerUserID = admitted.Capabilities, admitted.CallerUserID
	opts.TrustedAuthors = admitted.TrustedAuthors
	value, err := s.readMemo.get(ctx, key, func(ctx context.Context, now time.Time) (readMemoValue, error) {
		// The entry is dropped by change notices and bounded by its
		// horizon; it reads no validation state it would not use.
		horizon := newReadMemoHorizon(now)
		reads := &readinessReads{}
		ctx = context.WithValue(ctx, readMemoHorizonKey{}, horizon)
		ctx = context.WithValue(ctx, readinessReadsKey{}, reads)
		opts.Now = now
		items, err := s.readInboxUncached(ctx, opts, band, filter)
		if err != nil {
			return readMemoValue{}, err
		}
		if err := s.nameTaskCrates(ctx, &items); err != nil {
			return readMemoValue{}, err
		}
		body, err := json.Marshal(items)
		return readMemoValue{body: body, validUntil: horizon.until, deadline: horizon.deadline, reads: reads.snapshot()}, err
	})
	if err != nil {
		return nil, err
	}
	// The enclosing read is bounded by this entry's horizon and covers the
	// readiness values it read, whether built here or held.
	memoEntryHorizon(ctx, value)
	if outer, ok := readinessReadsFrom(ctx); ok {
		outer.addAll(value.reads)
	}
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
	orderHorizon(ctx, inputs.Orders)
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
	// Reports sit only in the decisions band, so a read of another band
	// reads none (component-feedback-reports invariant 19).
	if band == "" || band == "decisions" {
		reports, err := s.readInboxFeedbackReports(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, reports...)
	}
	for _, item := range items {
		if item.Deadline != nil {
			memoHorizon(ctx, *item.Deadline)
		}
		if item.Proposal != nil && !item.Proposal.ProposedAt.IsZero() {
			proposed := item.Proposal.ProposedAt
			// The proposal's age is display text: its next second ends
			// the tag and the entry but is no projection deadline, so a
			// slow build is not read again for it.
			if proposed.After(opts.Now) {
				memoExpiry(ctx, proposed.Add(time.Second))
			} else {
				memoExpiry(ctx, proposed.Add((opts.Now.Sub(proposed)/time.Second+1)*time.Second))
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

// The Inbox items of feedback reports (component-feedback-reports invariant
// 19; component-next-action invariants 21, 22, 24; req-production-feedback
// AC-6.5; req-operator-next-action AC-2.7, AC-2.12). One report is one
// decision while its in_inbox is true. The Inbox items and the attention
// count both come from the store's ListInboxFeedbackReports, the one read of
// that membership, so the count equals the membership.

// feedbackReportInboxKind is the kind of a report's Inbox item.
const feedbackReportInboxKind = "feedback_report"

// inboxFeedbackReportSortPrefix makes a report's sort key differ from every
// task ID and document ID, so a tie at one entry time orders the same way on
// every read.
const inboxFeedbackReportSortPrefix = "feedback_report/"

// inboxFeedbackReports reads the workspace's Inbox membership: every open
// report whose in_inbox is true, with the details of the same store view
// (store.ReportReadStore.ListInboxFeedbackReports). The read has no row
// ceiling, so no valid state of the workspace makes it fail. A store
// without the report capability holds no report.
func (s *Server) inboxFeedbackReports(ctx context.Context) ([]store.FeedbackReport, store.FeedbackReportDetails, error) {
	backend, ok := s.Store.(store.ReportStore)
	if !ok {
		return nil, store.FeedbackReportDetails{}, nil
	}
	page, err := backend.ListInboxFeedbackReports(s.feedbackReadContext(ctx), false)
	if err != nil {
		return nil, store.FeedbackReportDetails{}, err
	}
	if len(page.Reports) != page.Total {
		return nil, store.FeedbackReportDetails{}, fmt.Errorf("inbox reports: the read answered %d of %d reports", len(page.Reports), page.Total)
	}
	for _, report := range page.Reports {
		if !feedbackReportInInbox(report) {
			return nil, store.FeedbackReportDetails{}, fmt.Errorf("inbox reports: report %s is not in the Inbox", report.ID)
		}
	}
	return page.Reports, page.Details, nil
}

// feedbackReportInInbox is the membership predicate the store read applies:
// an open report whose in_inbox is true. A closed report is never in the
// Inbox (component-feedback-reports invariant 19).
func feedbackReportInInbox(report store.FeedbackReport) bool {
	return report.InInbox && report.State == store.FeedbackReportOpen
}

// inboxFeedbackReportCount is the number of reports in the Inbox, read on
// every call and never cached (component-next-action invariant 24), through
// the predicate of the Inbox read and without loading any report.
func (s *Server) inboxFeedbackReportCount(ctx context.Context) (int, error) {
	backend, ok := s.Store.(store.ReportStore)
	if !ok {
		return 0, nil
	}
	page, err := backend.ListInboxFeedbackReports(s.feedbackReadContext(ctx), true)
	return page.Total, err
}

// readInboxFeedbackReports builds one decision item for each report in the
// Inbox, with the served report built from the same store answer, and asks
// the linking job again for each pending report it serves
// (component-feedback-reports invariants 15, 21).
func (s *Server) readInboxFeedbackReports(ctx context.Context) ([]inboxItem, error) {
	reports, details, err := s.inboxFeedbackReports(ctx)
	if err != nil || len(reports) == 0 {
		return nil, err
	}
	served, err := s.servedFeedbackReports(ctx, reports, details)
	if err != nil {
		return nil, err
	}
	items := make([]inboxItem, 0, len(served))
	for i := range served {
		items = append(items, inboxItem{
			Kind: feedbackReportInboxKind, Band: "decisions", Report: &served[i],
			LastEventAt: feedbackReportInboxEntry(reports[i]), SortID: inboxFeedbackReportSortPrefix + reports[i].ID,
		})
	}
	s.requestPendingFeedbackLinking(ctx, reports)
	return items, nil
}

// feedbackReportInboxEntry is the time a report entered the Inbox: its
// creation, or its latest reopening. in_inbox becomes true only when a
// report is stored or reopened by a report, and a person's reopening sets it
// false (component-feedback-reports invariants 17, 19), so a report in the
// Inbox with a reopening time entered at that reopening. last_seen is not
// used, because a later entry changes it.
func feedbackReportInboxEntry(report store.FeedbackReport) time.Time {
	if report.ReopenedAt != nil {
		return report.ReopenedAt.UTC()
	}
	return report.CreatedAt.UTC()
}
