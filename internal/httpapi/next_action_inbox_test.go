package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestInboxFoldsDocumentProposalTasks(t *testing.T) {
	task := core.Task{ID: "folded", State: core.TaskRunning}
	projected := map[string]nextaction.NextAction{
		task.ID: {Actor: nextaction.ActorOperator, Kind: nextaction.KindDocumentProposal, Judgment: true},
	}
	items := inboxTaskItems([]core.Task{task}, projected, map[string]time.Time{}, map[string]bool{task.ID: true})
	if len(items) != 0 {
		t.Fatalf("folded task still listed: %+v", items)
	}
}

// Each document-proposal item states its age at the read's clock and the
// NA-4 inbox copy; only a folded row-8 withholding version says the claim
// stays withheld (feature-operator-next-action NA-4, NA-5;
// req-operator-next-action AC-1.8, AC-2.6).
func TestDocumentProposalItemsStateAgeAndCopy(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	proposals := []core.PendingProposal{
		{ID: "req-a", Title: "Requirement A", Tier: "requirement", Version: 2, OriginType: "task", OriginID: "task-1", ProposedAt: now.Add(-90 * time.Minute)},
		{ID: "req-a", Title: "Requirement A", Tier: "requirement", Version: 3, OriginType: "operator", ProposedAt: now.Add(-1500 * time.Millisecond)},
		{ID: "design-b", Title: "Design B", Tier: "system_design", Version: 4, OriginType: "operator", ProposedAt: now.Add(time.Hour)},
		{ID: "DEC-9", Title: "Decision nine", Tier: "decision", OriginType: "session", OriginID: "session-1"},
		{ID: "ctx", Title: "Context", Tier: "task_context", ProposedAt: now},
	}
	withholding := map[string]bool{proposalKey("requirement", "req-a", 2): true}
	items := documentProposalItems(proposals, withholding, now)
	if len(items) != 4 {
		t.Fatalf("items=%+v", items)
	}
	type want struct {
		age                       *int64
		label, question, ifIgnore string
		sentence                  string
	}
	ages := []int64{5400, 1, 0}
	wants := []want{
		{&ages[0], "Requirement proposal", "Confirm or dismiss this proposed version?", "The claim stays withheld.", dismissEarlierVersions},
		{&ages[1], "Requirement proposal", "Confirm or dismiss this proposed version?", "The proposal stays pending.", dismissEarlierVersions},
		{&ages[2], "System Design proposal", "Confirm or dismiss this proposed version?", "The proposal stays pending.", ""},
		{nil, "Decision proposal", "Confirm or dismiss this proposed decision?", "The proposal stays pending.", ""},
	}
	for i, item := range items {
		w := wants[i]
		if item.Kind != "document_proposal" || item.Band != "decisions" || item.Proposal == nil {
			t.Fatalf("item %d shape=%+v", i, item)
		}
		if (w.age == nil) != (item.Proposal.AgeSeconds == nil) || (w.age != nil && *w.age != *item.Proposal.AgeSeconds) {
			t.Fatalf("item %d age=%v want %v", i, item.Proposal.AgeSeconds, w.age)
		}
		if item.Label != w.label || item.Question != w.question || item.IfIgnored != w.ifIgnore || item.Sentence != w.sentence {
			t.Fatalf("item %d copy=%q %q %q %q", i, item.Label, item.Question, item.IfIgnored, item.Sentence)
		}
	}
}

func TestDocumentProposalCopyUnknownTierReadsRowEight(t *testing.T) {
	label, question, ifIgnored := nextaction.DocumentProposalCopy("other", false)
	if label != "Document proposal" || question != "Confirm or dismiss the proposal?" || ifIgnored != "The proposal stays pending." {
		t.Fatalf("copy=%q %q %q", label, question, ifIgnored)
	}
}

// TestInboxStoreReadExcludesTerminalTasks proves the inbox's candidate read
// carries TaskFilter.Open, so merged and closed tasks and blueprint anchors
// never reach the projection, and that the response is the same with and
// without 200 merged and closed tasks (feature-operator-next-action NA-5).
func TestInboxStoreReadExcludesTerminalTasks(t *testing.T) {
	inbox := func(terminal int) (*taskListCountingStore, []byte, http.Header) {
		t.Helper()
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		for index, task := range []core.Task{
			{ID: "gate-plan", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval},
			{ID: "gate-merge", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval},
			{ID: "anchor", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval},
			{ID: "anchor-child", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, ParentTaskID: "anchor"},
			{ID: "running", State: core.TaskRunning},
		} {
			task.Workspace, task.Repo, task.BaseBranch, task.Branch = "demo", "conveyor", "main", "conveyor/task-"+task.ID
			task.NextStage, task.CreatedAt = core.StageImplement, base.Add(time.Duration(index)*time.Minute)
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
		}
		for index := range terminal {
			state := core.TaskMerged
			if index%2 == 1 {
				state = core.TaskClosed
			}
			id := fmt.Sprintf("history-%03d", index)
			if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, NextStage: core.StageReview, CreatedAt: base.Add(time.Hour + time.Duration(index)*time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		counter := newTaskListCountingStore(st)
		server := NewServer(counter)
		server.Workspace = "demo"
		response := httptest.NewRecorder()
		authenticatedMemoryHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/next-actions", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return counter, body.Items, response.Header()
	}
	_, without, withoutHeaders := inbox(0)
	counter, with, withHeaders := inbox(200)
	if string(with) != string(without) {
		t.Fatalf("inbox items changed with merged and closed history:\nwith    %s\nwithout %s", with, without)
	}
	for _, name := range []string{"X-Conveyor-Total", "X-Conveyor-Limit", "X-Conveyor-Offset"} {
		if withHeaders.Get(name) != withoutHeaders.Get(name) {
			t.Fatalf("%s=%q with history, %q without", name, withHeaders.Get(name), withoutHeaders.Get(name))
		}
	}
	calls, filters, projected := counter.snapshot()
	if calls["ListTasks"] != 0 {
		t.Fatalf("inbox read the whole workspace: %v", calls)
	}
	open := 0
	for _, filter := range filters {
		if filter.Open {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("ListTasksFiltered filters=%+v, want exactly one inbox read carrying Open", filters)
	}
	for _, id := range projected {
		if id == "anchor" || len(id) > 8 && id[:8] == "history-" {
			t.Fatalf("projection read %s, which the store read must exclude", id)
		}
	}
	var items []struct {
		Task *core.Task `json:"task"`
	}
	if err := json.Unmarshal(with, &items); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, item := range items {
		if item.Task != nil {
			listed[item.Task.ID] = true
		}
	}
	if !listed["gate-plan"] || !listed["gate-merge"] || !listed["anchor-child"] || listed["anchor"] {
		t.Fatalf("inbox task items=%v, want the open gates and the anchor's child but not the anchor", listed)
	}
}
