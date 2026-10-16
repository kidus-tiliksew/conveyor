package httpapi

import (
	"context"
	"log"
	"net/http"
	"strconv"
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
	PullRequest      *core.GitHubLifecycle   `json:"pull_request,omitempty"`
	Serves           []string                `json:"serves,omitempty"`
	Recommendation   string                  `json:"recommendation,omitempty"`
	NoJudgmentNeeded bool                    `json:"no_judgment_needed,omitempty"`
	Proposal         *core.PendingProposal   `json:"proposal,omitempty"`
	Sentence         string                  `json:"sentence,omitempty"`
	Blockers         []nextaction.BlockerRef `json:"blockers,omitempty"`
	Deadline         *time.Time              `json:"deadline,omitempty"`
	LastEventAt      time.Time               `json:"last_event_at,omitempty"`
	SortID           string                  `json:"-"`
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		log.Printf("next action options: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	items, err := s.readInbox(r.Context(), opts, band, filter)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	total := len(items)
	start := min(offset, total)
	end := min(start+limit, total)
	acts, err := s.Store.ListFactoryActs(r.Context(), time.Now().UTC().Add(-24*time.Hour), 100)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Conveyor-Total", strconv.Itoa(total))
	w.Header().Set("X-Conveyor-Limit", strconv.Itoa(limit))
	w.Header().Set("X-Conveyor-Offset", strconv.Itoa(offset))
	writeJSON(w, http.StatusOK, map[string]any{"items": items[start:end], "acts": acts})
}

// readInbox builds the operator inbox in server order. GET /v1/next-actions
// and the MCP list_operator_inbox read share it, so the inbox has one
// derivation (DEC-61; feature-operator-next-action NA-8).
func (s *Server) readInbox(ctx context.Context, opts store.NextActionOptions, band string, filter store.TaskFilter) ([]inboxItem, error) {
	tasks, err := s.Store.ListTasksFiltered(ctx, filter)
	if err != nil {
		return nil, err
	}
	candidates := make([]core.Task, 0, len(tasks))
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if core.TaskTerminal(task.State) || core.BlueprintAnchor(task) {
			continue
		}
		candidates = append(candidates, task)
		ids = append(ids, task.ID)
	}
	projected, err := store.ReadNextActions(ctx, s.Store, candidates, opts)
	if err != nil {
		return nil, err
	}
	proposals, err := s.Store.ListPendingProposals(ctx)
	if err != nil {
		return nil, err
	}
	markers, err := s.Store.ListActivityMarkersForTasks(ctx, ids)
	if err != nil {
		return nil, err
	}
	lastEvent := map[string]time.Time{}
	for _, marker := range markers {
		lastEvent[marker.TaskID] = marker.LastEventAt
	}
	folded := map[string]bool{}
	for _, task := range candidates {
		if projected[task.ID].Kind == nextaction.KindDocumentProposal {
			folded[task.ID] = true
		}
	}
	items := inboxTaskItems(candidates, projected, lastEvent, folded)
	items = append(items, documentProposalItems(proposals)...)
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

func inboxTaskItems(candidates []core.Task, projected map[string]nextaction.NextAction, lastEvent map[string]time.Time, folded map[string]bool) []inboxItem {
	var items []inboxItem
	for _, task := range candidates {
		if folded[task.ID] {
			continue
		}
		action := projected[task.ID]
		item := inboxItem{
			Task: &task, NextAction: compactPtr(action), PullRequest: task.GitHub,
			Serves: serves(task), Deadline: action.Deadline, LastEventAt: lastEvent[task.ID],
			SortID: task.ID, Blockers: action.BlockedBy,
		}
		switch {
		case action.Actor == nextaction.ActorOperator && action.Judgment:
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

func documentProposalItems(proposals []core.PendingProposal) []inboxItem {
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
		item := inboxItem{
			Kind: "document_proposal", Band: "decisions", Proposal: &proposal,
			LastEventAt: proposal.ProposedAt, SortID: proposal.ID,
		}
		if counts[proposal.Tier+"/"+proposal.ID] > 1 {
			item.Sentence = dismissEarlierVersions
		}
		items = append(items, item)
	}
	return items
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
