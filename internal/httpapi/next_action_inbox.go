package httpapi

import (
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
	DependentsFirst  bool                    `json:"-"`
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
	tasks, err := s.Store.ListTasksFiltered(r.Context(), filter)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
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
	projected, err := store.ReadNextActions(r.Context(), s.Store, candidates, s.nextActionOptions(r))
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	proposals, err := s.Store.ListPendingProposals(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	blockers, err := s.Store.ListDependencyBlockers(r.Context(), ids)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	markers, err := s.Store.ListActivityMarkersForTasks(r.Context(), ids)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	lastEvent := map[string]time.Time{}
	for _, marker := range markers {
		lastEvent[marker.TaskID] = marker.LastEventAt
	}
	dependedOn := map[string]bool{}
	for _, taskID := range ids {
		for _, blocker := range blockers[taskID].BlockingTaskIDs {
			dependedOn[blocker] = true
		}
	}
	folded := map[string]bool{}
	for _, task := range candidates {
		if projected[task.ID].Kind == nextaction.KindDocumentProposal {
			folded[task.ID] = true
		}
	}
	items := inboxTaskItems(candidates, projected, lastEvent, dependedOn, folded)
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
func inboxTaskItems(candidates []core.Task, projected map[string]nextaction.NextAction, lastEvent map[string]time.Time, dependedOn, folded map[string]bool) []inboxItem {
	var items []inboxItem
	for _, task := range candidates {
		if folded[task.ID] {
			continue
		}
		action := projected[task.ID]
		item := inboxItem{
			Task: &task, NextAction: compactPtr(action), PullRequest: task.GitHub,
			Serves: serves(task), Deadline: action.Deadline, LastEventAt: lastEvent[task.ID],
			DependentsFirst: dependedOn[task.ID], SortID: task.ID, Blockers: action.BlockedBy,
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
	if a.DependentsFirst != b.DependentsFirst {
		return a.DependentsFirst
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
