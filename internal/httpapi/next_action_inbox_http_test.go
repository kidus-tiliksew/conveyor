package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestNextActionInboxBandsPagingAndActs(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC().Truncate(time.Second)
	parent := core.Task{ID: "z-parent", Workspace: "demo", Repo: "conveyor", Branch: "parent", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now.Add(time.Hour)}
	child := core.Task{ID: "a-child", Workspace: "demo", Repo: "conveyor", Branch: "child", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	running := core.Task{ID: "m-running", Workspace: "demo", Repo: "conveyor", Branch: "running", State: core.TaskRunning, CreatedAt: now}
	otherRepo := core.Task{ID: "other-repo", Workspace: "demo", Repo: "other", Branch: "other", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	for _, task := range []core.Task{parent, child, running, otherRepo} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: child.ID, DependsOnTaskID: parent.ID, Reason: "parent first", RequestID: "dep-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "stall-job", TaskID: running.ID, Stage: core.StageImplement, State: core.JobRunning}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: "stall-order", TaskID: running.ID, JobID: "stall-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueDeadline: now.Add(time.Hour), RetrySuppressed: true}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	plain := core.Task{ID: "plain-running", Workspace: "demo", Repo: "conveyor", Branch: "plain", State: core.TaskRunning, CreatedAt: now.Add(2 * time.Hour)}
	if err := st.CreateTask(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: parent.ID, Kind: "task.noted", ActorRole: core.ActorSystem, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: child.ID, Kind: "task.noted", ActorRole: core.ActorSystem, At: now.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: otherRepo.ID, Kind: "task.noted", ActorRole: core.ActorSystem, At: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	recent := core.Event{TaskID: plain.ID, Kind: "pipeline.bounced", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"source": "mcp-review"})}
	old := core.Event{TaskID: plain.ID, Kind: "merge.conflict_fix_dispatched", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(-25 * time.Hour)}
	for _, event := range []core.Event{recent, old} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-inbox", Title: "Inbox requirement"}, core.RequirementVersion{
		Content: "# Inbox", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Show the inbox."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "# Inbox revised", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Show the revised inbox."}},
	}); err != nil {
		t.Fatal(err)
	}
	confirmed, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-context", Title: "Context requirement"}, core.RequirementVersion{
		Content: "# Context", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Attach context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, confirmed.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: parent.ID, TargetKind: core.TaskContextProposalRequirement, TargetID: confirmed.ID, Source: core.TaskContextProposalTriage, Justification: "The task implements REQ-1."}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
		return response
	}
	invalid := call("/v1/next-actions?workspace_id=demo&band=nope")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid band status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	badFilter := call("/v1/next-actions?workspace_id=demo&state=not-a-state")
	if badFilter.Code != http.StatusBadRequest {
		t.Fatalf("invalid filter status=%d body=%s", badFilter.Code, badFilter.Body.String())
	}

	body := call("/v1/next-actions?workspace_id=demo")
	if body.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", body.Code, body.Body.String())
	}
	if body.Header().Get("X-Conveyor-Total") == "" || body.Header().Get("X-Conveyor-Limit") == "" || body.Header().Get("X-Conveyor-Offset") != "0" {
		t.Fatalf("paging headers=%v", body.Header())
	}
	var payload struct {
		Items []struct {
			Band     string `json:"band"`
			Kind     string `json:"kind"`
			Sentence string `json:"sentence"`
			Task     *struct {
				ID string `json:"id"`
			} `json:"task"`
			NextAction *struct {
				Label string `json:"label"`
			} `json:"next_action"`
			Proposal *struct {
				Tier string `json:"tier"`
				ID   string `json:"id"`
			} `json:"proposal"`
		} `json:"items"`
		Acts []struct {
			Kind string `json:"kind"`
		} `json:"acts"`
	}
	if err = json.Unmarshal(body.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Body.String(), `"tier":"task_context"`) {
		t.Fatal("task_context proposal leaked into the inbox")
	}
	if !strings.Contains(body.Body.String(), dismissEarlierVersions) {
		t.Fatalf("missing multi-version sentence: %s", body.Body.String())
	}
	ids := make([]string, 0)
	for _, item := range payload.Items {
		if item.Task != nil {
			ids = append(ids, item.Task.ID)
			if item.NextAction == nil || item.NextAction.Label == "" {
				t.Fatalf("item %s missing compact label", item.Task.ID)
			}
		}
	}
	parentAt, childAt, otherAt := indexOf(ids, parent.ID), indexOf(ids, child.ID), indexOf(ids, otherRepo.ID)
	// The child waits at its own plan gate with no queued order, so it names
	// no edge in blocks; the parent leads on its older last_event_at.
	if parentAt < 0 || childAt < 0 || parentAt > childAt {
		t.Fatalf("last_event_at order=%v", ids)
	}
	if otherAt < 0 || childAt < 0 || otherAt > childAt {
		t.Fatalf("older last_event_at first order=%v", ids)
	}
	stallAt, plainAt := indexOf(ids, running.ID), indexOf(ids, plain.ID)
	if stallAt < 0 || plainAt < 0 || stallAt > plainAt {
		t.Fatalf("deadline-before-nil order=%v", ids)
	}
	var sawOld bool
	for _, act := range payload.Acts {
		if act.Kind == "merge.conflict_fix_dispatched" {
			sawOld = true
		}
	}
	if sawOld || len(payload.Acts) == 0 {
		t.Fatalf("acts=%+v", payload.Acts)
	}

	page := call("/v1/next-actions?workspace_id=demo&limit=1&offset=0")
	if page.Header().Get("X-Conveyor-Limit") != "1" || page.Header().Get("X-Conveyor-Offset") != "0" || page.Header().Get("X-Conveyor-Total") != body.Header().Get("X-Conveyor-Total") {
		t.Fatalf("page headers=%v", page.Header())
	}
	for _, band := range []string{"decisions", "blocked", "automatic"} {
		response := call("/v1/next-actions?workspace_id=demo&band=" + band)
		if response.Code != http.StatusOK {
			t.Fatalf("band %s status=%d", band, response.Code)
		}
		var filtered struct {
			Items []struct {
				Band string `json:"band"`
			} `json:"items"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &filtered); err != nil {
			t.Fatal(err)
		}
		for _, item := range filtered.Items {
			if item.Band != band {
				t.Fatalf("band %s item=%s", band, item.Band)
			}
		}
	}
	filtered := call("/v1/next-actions?workspace_id=demo&repository=other")
	if filtered.Code != http.StatusOK || strings.Contains(filtered.Body.String(), `"id":"`+parent.ID+`"`) || !strings.Contains(filtered.Body.String(), `"id":"`+otherRepo.ID+`"`) {
		t.Fatalf("repository filter body=%s", filtered.Body.String())
	}
}

// GET /v1/next-actions serializes blocks on each task item's compact next
// action and orders an item with dependents ahead of one with an earlier
// deadline, also when the request's filter excludes the dependent
// (feature-operator-next-action NA-5; req-operator-next-action AC-2.1).
func TestNextActionInboxOrdersDependentsFirst(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC().Truncate(time.Second)
	// Each fixture order lost its lease and has an automatic recovery
	// scheduled, so its statement carries the recovery time as its deadline.
	// A queued order's stored queue deadline is no deadline
	// (component-next-action invariant 14), so these fixtures replace the
	// retired retry-suppressed orders whose queue deadlines ordered the band.
	stalled := func(id, repo string, deadline time.Time) {
		t.Helper()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: "demo", Title: "Task " + id, Repo: repo, Branch: id, State: core.TaskRunning, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: id + "-job", TaskID: id, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: id + "-order", TaskID: id, JobID: id + "-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(24 * time.Hour), LastAttemptID: id + "-attempt", LastAttemptOutcome: core.WorkOrderOutcomeExpired, NextAutomaticRecoveryAt: deadline}); err != nil {
			t.Fatal(err)
		}
	}
	stalled("z-blocker", "conveyor", now.Add(2*time.Hour))
	stalled("a-earlier", "conveyor", now.Add(time.Hour))
	if err := st.CreateTask(ctx, core.Task{ID: "m-dependent", Workspace: "demo", Title: "Dependent", Repo: "other", Branch: "dependent", State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, core.Job{ID: "m-dependent-job", TaskID: "m-dependent", Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "m-dependent-order", TaskID: "m-dependent", JobID: "m-dependent-job", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: "m-dependent", DependsOnTaskID: "z-blocker", Reason: "blocker first", RequestID: "inbox-dependents"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	type item struct {
		Band string `json:"band"`
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
		NextAction *struct {
			Deadline *time.Time              `json:"deadline"`
			Blocks   []nextaction.BlockerRef `json:"blocks"`
		} `json:"next_action"`
	}
	read := func(path string) ([]item, string) {
		t.Helper()
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, path, nil)))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		var payload struct {
			Items []item `json:"items"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Items, response.Body.String()
	}
	want := []nextaction.BlockerRef{{TaskID: "m-dependent", Title: "Dependent", State: string(core.TaskRunning)}}
	for _, path := range []string{"/v1/next-actions?workspace_id=demo&band=automatic", "/v1/next-actions?workspace_id=demo&band=automatic&repository=conveyor"} {
		items, body := read(path)
		var order []string
		byID := map[string]item{}
		for _, it := range items {
			if it.Task != nil {
				order = append(order, it.Task.ID)
				byID[it.Task.ID] = it
			}
		}
		blocker, earlier := byID["z-blocker"], byID["a-earlier"]
		if blocker.NextAction == nil || earlier.NextAction == nil || earlier.NextAction.Deadline == nil || blocker.NextAction.Deadline == nil || !earlier.NextAction.Deadline.Before(*blocker.NextAction.Deadline) {
			t.Fatalf("%s fixture deadlines: %s", path, body)
		}
		if indexOf(order, "z-blocker") < 0 || indexOf(order, "z-blocker") > indexOf(order, "a-earlier") {
			t.Fatalf("%s order=%v, want the blocker before the earlier deadline", path, order)
		}
		if !reflect.DeepEqual(blocker.NextAction.Blocks, want) {
			t.Fatalf("%s blocker blocks=%+v, want %+v", path, blocker.NextAction.Blocks, want)
		}
		if earlier.NextAction.Blocks == nil || len(earlier.NextAction.Blocks) != 0 || !strings.Contains(body, `"blocks":[]`) {
			t.Fatalf("%s earlier blocks=%#v body=%s", path, earlier.NextAction.Blocks, body)
		}
	}
}

func indexOf(ids []string, want string) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}

// GET /v1/next-actions states each pending requirement, System Design, and
// decision proposal with its read-time age and the NA-4 inbox copy
// (feature-operator-next-action NA-5; req-operator-next-action AC-2.6).
func TestNextActionInboxDocumentProposalsCarryAgeAndCopy(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-age", Title: "Age requirement"}, core.RequirementVersion{
		Content: "# Age", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "State the age."}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-age", Title: "Age design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Age design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeDecision(ctx, core.Decision{Statement: "Age decision", Context: "Inbox", AlternativesRejected: "None", Origin: core.DecisionOriginOperator}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	before := time.Now().UTC()
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/next-actions?workspace_id=demo", nil)))
	elapsed := int64(time.Since(before)/time.Second) + 1
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []struct {
			Kind      string `json:"kind"`
			Label     string `json:"label"`
			Question  string `json:"question"`
			IfIgnored string `json:"if_ignored"`
			Proposal  *struct {
				Tier       string    `json:"tier"`
				ProposedAt time.Time `json:"proposed_at"`
				AgeSeconds *int64    `json:"age_seconds"`
			} `json:"proposal"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"requirement": "Requirement proposal", "system_design": "System Design proposal", "decision": "Decision proposal"}
	seen := map[string]bool{}
	for _, item := range payload.Items {
		if item.Kind != "document_proposal" || item.Proposal == nil {
			continue
		}
		tier := item.Proposal.Tier
		seen[tier] = true
		if item.Proposal.ProposedAt.IsZero() || item.Proposal.AgeSeconds == nil || *item.Proposal.AgeSeconds < 0 || *item.Proposal.AgeSeconds > elapsed+int64(time.Since(item.Proposal.ProposedAt)/time.Second) {
			t.Fatalf("%s age=%v proposed_at=%v", tier, item.Proposal.AgeSeconds, item.Proposal.ProposedAt)
		}
		if item.Label != labels[tier] || item.Question == "" || item.IfIgnored != "The proposal stays pending." {
			t.Fatalf("%s copy=%q %q %q", tier, item.Label, item.Question, item.IfIgnored)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("tiers=%v body=%s", seen, response.Body.String())
	}
}

// A task item's pull_request is the recorded pull request from its latest
// pull_request.opened, never the task's GitHub issue mirror, even when the
// issue number differs; a task with only an issue carries none
// (feature-operator-next-action NA-5; task 261021-cd3bf8).
func TestNextActionInboxPullRequestIsTheRecordedPullRequest(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC().Truncate(time.Second)
	merge := core.Task{ID: "merge-task", Workspace: "demo", Repo: "conveyor", Branch: "merge", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, CreatedAt: now}
	issueOnly := core.Task{ID: "issue-only", Workspace: "demo", Repo: "conveyor", Branch: "issue", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	for _, task := range []core.Task{merge, issueOnly} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if err := st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: task.ID, Repository: "acme/runtime", IssueNumber: 1671, IssueURL: "https://github.com/acme/runtime/issues/1671"}); err != nil {
			t.Fatal(err)
		}
	}
	for i, number := range []int{1700, 1709} {
		if err := st.AppendEvent(ctx, core.Event{TaskID: merge.ID, Kind: "pull_request.opened", ActorRole: core.ActorSystem, At: now.Add(time.Duration(i) * time.Minute), Payload: core.JSONPayload(map[string]any{
			"url": "https://github.com/acme/runtime/pull/" + strconv.Itoa(number), "number": number, "repository": "acme/runtime", "head_sha": "abc",
		})}); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/next-actions?workspace_id=demo&band=decisions", nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []struct {
			Task *struct {
				ID string `json:"id"`
			} `json:"task"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, item := range payload.Items {
		if item.Task != nil {
			got[item.Task.ID] = string(item.PullRequest)
		}
	}
	if want := `{"url":"https://github.com/acme/runtime/pull/1709","number":1709,"repository":"acme/runtime"}`; got[merge.ID] != want {
		t.Fatalf("merge pull_request = %s, want %s", got[merge.ID], want)
	}
	if pr, listed := got[issueOnly.ID]; !listed || pr != "" {
		t.Fatalf("issue-only pull_request = %q (listed %v), want none", pr, listed)
	}
}

// The Inbox items of feedback reports (component-feedback-reports v5
// invariants 19, 21; component-next-action invariants 21, 22, 24;
// feature-production-feedback-listening FR-5). The memo clock is the
// harness clock, so a memo entry ends only when a notice drops it or the
// test moves the clock past readMemoLifetime.

type feedbackInboxRead struct {
	items []map[string]json.RawMessage
	total string
	etag  string
}

func (h *reportRoutesHarness) inbox(query string) feedbackInboxRead {
	h.t.Helper()
	response := h.want(http.StatusOK, http.MethodGet, "/v1/next-actions"+query, "")
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		h.t.Fatalf("decode inbox %s: %v", response.Body, err)
	}
	return feedbackInboxRead{items: body.Items, total: response.Header().Get("X-Conveyor-Total"), etag: response.Header().Get("ETag")}
}

// reportIDs lists the report IDs of the read's feedback_report items in
// order, and checks each item's shape.
func (r feedbackInboxRead) reportIDs(t *testing.T) []string {
	t.Helper()
	ids := []string{}
	for _, item := range r.items {
		var kind, band string
		_ = json.Unmarshal(item["kind"], &kind)
		_ = json.Unmarshal(item["band"], &band)
		if kind != feedbackReportInboxKind {
			continue
		}
		var report servedFeedbackReport
		if err := json.Unmarshal(item["report"], &report); err != nil {
			t.Fatalf("decode report of %v: %v", item, err)
		}
		if band != "decisions" || !report.InInbox || report.State != store.FeedbackReportOpen || item["task"] != nil || item["proposal"] != nil {
			t.Fatalf("report item band=%s in_inbox=%v state=%s task=%s proposal=%s", band, report.InInbox, report.State, item["task"], item["proposal"])
		}
		ids = append(ids, report.ID)
	}
	return ids
}

func (r feedbackInboxRead) decisions() int {
	count := 0
	for _, item := range r.items {
		var band string
		_ = json.Unmarshal(item["band"], &band)
		if band == "decisions" {
			count++
		}
	}
	return count
}

// attention reads the attention summary of GET /v1/pending-proposals.
func (h *reportRoutesHarness) attention() attentionSummary {
	h.t.Helper()
	response := h.want(http.StatusOK, http.MethodGet, "/v1/pending-proposals", "")
	var body pendingProposalsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		h.t.Fatalf("decode pending proposals %s: %v", response.Body, err)
	}
	return body.Attention
}

// servedInInbox reads in_inbox of every open and closed report from the
// list route, by ID.
func (h *reportRoutesHarness) servedInInbox() map[string]bool {
	h.t.Helper()
	out := map[string]bool{}
	for _, state := range []string{"open", "closed"} {
		response := h.want(http.StatusOK, http.MethodGet, "/v1/feedback-reports?limit=200&state="+state, "")
		var reports []servedFeedbackReport
		if err := json.Unmarshal(response.Body.Bytes(), &reports); err != nil {
			h.t.Fatal(err)
		}
		for _, report := range reports {
			out[report.ID] = report.InInbox
		}
	}
	return out
}

// requireFeedbackInbox checks that the Inbox lists exactly want, in order,
// that the attention count is the Inbox's decisions count, and that every
// served report's in_inbox agrees with the Inbox.
func (h *reportRoutesHarness) requireFeedbackInbox(want []string) feedbackInboxRead {
	h.t.Helper()
	read := h.inbox("")
	if got := read.reportIDs(h.t); !slices.Equal(got, want) {
		h.t.Fatalf("inbox reports = %v, want %v", got, want)
	}
	attention := h.attention()
	if attention.OperatorDecisions != read.decisions() || attention.Total != attention.OperatorDecisions {
		h.t.Fatalf("attention = %+v, inbox decisions = %d", attention, read.decisions())
	}
	listed := map[string]bool{}
	for _, id := range want {
		listed[id] = true
	}
	for id, inInbox := range h.servedInInbox() {
		if inInbox != listed[id] {
			h.t.Fatalf("report %s: served in_inbox=%v, listed in the Inbox=%v", id, inInbox, listed[id])
		}
	}
	return read
}

func TestFeedbackInboxMembershipAndCountAgree(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.server.UseReadMemoClock(h.clock)
	h.requireFeedbackInbox([]string{})
	tasksBefore := h.want(http.StatusOK, http.MethodGet, "/v1/attention/tasks", "").Body.String()

	never := h.storeReport("never", nil)
	confirmed := h.storeReport("confirmed", nil)
	added := h.storeReport("added", nil)
	removed := h.storeReport("removed", nil)
	closedReport := h.storeReport("closed", nil)
	fixed := h.storeReport("fixed", nil)
	unchanged := h.storeReport("unchanged", nil)
	userReopened := h.storeReport("user-reopened", nil)
	reportReopened := h.storeReport("report-reopened", nil)
	// The end of linking, with possible links, before any read asks the
	// linking job.
	cause := h.mergedTask("")
	h.endLinking(confirmed, []store.FeedbackReportLinkCandidate{{TaskID: cause.ID, Evidence: []store.FeedbackReportEvidence{{Kind: store.FeedbackEvidenceLine, File: "a.go", Line: 1, Commit: readsCommit("ab")}}}}, nil)
	h.endLinking(removed, []store.FeedbackReportLinkCandidate{{TaskID: cause.ID, Evidence: []store.FeedbackReportEvidence{{Kind: store.FeedbackEvidenceLine, File: "b.go", Line: 2, Commit: readsCommit("cd")}}}}, nil)
	staging := h.create(`{"name":"Staging"}`)
	stagingKey := h.issue(staging.ID)
	h.advance(time.Second)
	if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: staging.ID, KeyID: stagingKey.ID, ArrivedAt: h.clock(), Entries: []store.FeedbackReportEntry{{Kind: store.FeedbackReportAlert, Key: "staging", Summary: "Staging queue is deep"}}}); err != nil {
		t.Fatal(err)
	}
	stagingReports, err := h.st.ListEnvironmentFeedbackReports(h.ctx, staging.ID)
	if err != nil || len(stagingReports) != 1 {
		t.Fatalf("staging reports = %v, %v", stagingReports, err)
	}
	// A report of another workspace is never an Inbox item here.
	foreignEnvironment, err := h.st.CreateEnvironment(h.betaCtx, store.EnvironmentInput{Name: "Beta"})
	if err != nil {
		t.Fatal(err)
	}
	foreignKey, err := h.st.IssueEnvironmentKey(h.betaCtx, foreignEnvironment.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.RecordFeedbackReports(h.betaCtx, store.FeedbackReportBatch{EnvironmentID: foreignEnvironment.ID, KeyID: foreignKey.ID, ArrivedAt: h.clock(), Entries: []store.FeedbackReportEntry{{Kind: store.FeedbackReportError, Key: "foreign", Summary: "Foreign"}}}); err != nil {
		t.Fatal(err)
	}
	all := []string{never.ID, confirmed.ID, added.ID, removed.ID, closedReport.ID, fixed.ID, unchanged.ID, userReopened.ID, reportReopened.ID, stagingReports[0].ID}
	// Every new report is in the Inbox, oldest entry first.
	h.requireFeedbackInbox(all)

	// The end of linking is not a person's act: both stay in the Inbox.
	h.requireFeedbackInbox(all)

	// Each act of a person takes its report out of the Inbox, through the
	// routes, so the act's notice drops the memo.
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+confirmed.ID+"/links", `{"task_id":"`+cause.ID+`"}`)
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+added.ID+"/links", `{"crate_id":"`+h.crate("Checkout").ID+`"}`)
	links, err := h.st.ListFeedbackReportLinks(h.ctx, removed.ID)
	if err != nil || len(links) != 1 {
		t.Fatalf("links = %v, %v", links, err)
	}
	h.want(http.StatusOK, http.MethodDelete, "/v1/feedback-reports/"+removed.ID+"/links/"+links[0].ID, "")
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+closedReport.ID+"/close", `{"reason":"Fixed by the last deploy."}`)
	h.want(http.StatusOK, http.MethodPut, "/v1/feedback-reports/"+fixed.ID+"/fix-task", `{"task_id":"`+h.openTask().ID+`"}`)
	// An act that changes nothing records nothing and leaves the report in
	// the Inbox.
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+unchanged.ID+"/reopen", "")
	// Closed and reopened by a person: open, and out of the Inbox.
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+userReopened.ID+"/close", `{"reason":"Not a bug."}`)
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+userReopened.ID+"/reopen", "")
	h.want(http.StatusOK, http.MethodPost, "/v1/feedback-reports/"+reportReopened.ID+"/close", `{"reason":"Gone."}`)
	h.requireFeedbackInbox([]string{never.ID, unchanged.ID, stagingReports[0].ID})

	// A later entry after the close reopens the report: it is back once, at
	// the time of the reopening, so it is now the newest entry.
	again := h.storeReport("report-reopened", nil)
	if again.ReopenedAt == nil || again.State != store.FeedbackReportOpen || !again.InInbox {
		t.Fatalf("reopened by a report = %+v", again)
	}
	read := h.requireFeedbackInbox([]string{never.ID, unchanged.ID, stagingReports[0].ID, reportReopened.ID})
	for _, item := range read.items {
		var report struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(item["report"], &report)
		var at time.Time
		_ = json.Unmarshal(item["last_event_at"], &at)
		switch report.ID {
		case reportReopened.ID:
			if !at.Equal(again.ReopenedAt.UTC()) {
				t.Fatalf("reopened report entered at %s, want its reopening %s", at, again.ReopenedAt)
			}
		case never.ID:
			if !at.Equal(never.CreatedAt.UTC()) {
				t.Fatalf("new report entered at %s, want its creation %s", at, never.CreatedAt)
			}
		}
	}
	// A later entry that only updates an open report changes neither the
	// membership nor the entry time.
	h.storeReport("never", nil)
	h.requireFeedbackInbox([]string{never.ID, unchanged.ID, stagingReports[0].ID, reportReopened.ID})

	// Reports change no task's attention (req-production-feedback AC-11.1).
	if tasksAfter := h.want(http.StatusOK, http.MethodGet, "/v1/attention/tasks", "").Body.String(); tasksAfter != tasksBefore {
		t.Fatalf("attention tasks changed: %s then %s", tasksBefore, tasksAfter)
	}
	if attention := h.attention(); attention.TaskCount != 0 || attention.PendingProposalCount != 0 || attention.OperatorDecisions != 4 {
		t.Fatalf("attention = %+v, want 4 reports and no task or proposal", attention)
	}
}

// Reports keep the Inbox order: within the decisions band, entry time
// ascending with the sort key breaking a tie, and a band or task filter
// treats them as a document proposal (component-next-action invariant 22;
// req-operator-next-action AC-2.12).
func TestFeedbackInboxOrderPagingAndFilters(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.server.UseReadMemoClock(h.clock)
	ids := []string{}
	for i := range 5 {
		ids = append(ids, h.storeReport(fmt.Sprintf("paged-%d", i), nil).ID)
	}
	h.requireFeedbackInbox(ids)
	// Pages of two: every report once, in order, with the same total.
	seen := []string{}
	for offset := 0; offset < 6; offset += 2 {
		read := h.inbox(fmt.Sprintf("?limit=2&offset=%d", offset))
		if read.total != "5" {
			t.Fatalf("page at %d total=%s, want 5", offset, read.total)
		}
		seen = append(seen, read.reportIDs(t)...)
	}
	if !slices.Equal(seen, ids) {
		t.Fatalf("paged reports = %v, want %v", seen, ids)
	}
	if got := h.inbox("?band=decisions").reportIDs(t); !slices.Equal(got, ids) {
		t.Fatalf("decisions band = %v", got)
	}
	for _, band := range []string{"blocked", "automatic"} {
		if got := h.inbox("?band=" + band).reportIDs(t); len(got) != 0 {
			t.Fatalf("%s band lists reports %v", band, got)
		}
	}
	// A task filter does not remove a report.
	if got := h.inbox("?repo=elsewhere").reportIDs(t); !slices.Equal(got, ids) {
		t.Fatalf("task-filtered inbox = %v, want %v", got, ids)
	}

	// Two reports stored by one request enter at one time. A page boundary
	// between them still lists each once, in sort-key order.
	h.advance(time.Second)
	if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: h.environment.ID, KeyID: h.key.ID, ArrivedAt: h.clock(), Entries: []store.FeedbackReportEntry{
		{Kind: store.FeedbackReportError, Key: "tie-b", Summary: "Tie B"},
		{Kind: store.FeedbackReportError, Key: "tie-a", Summary: "Tie A"},
	}}); err != nil {
		t.Fatal(err)
	}
	tieA, tieB := h.reportByKey("tie-a"), h.reportByKey("tie-b")
	if !tieA.CreatedAt.Equal(tieB.CreatedAt) {
		t.Fatalf("one request stored two entry times %s and %s", tieA.CreatedAt, tieB.CreatedAt)
	}
	tied := []string{tieA.ID, tieB.ID}
	slices.Sort(tied)
	want := append(slices.Clone(ids), tied...)
	h.requireFeedbackInbox(want)
	seen = []string{}
	for offset := 0; offset < 7; offset += 3 {
		read := h.inbox(fmt.Sprintf("?limit=3&offset=%d", offset))
		if read.total != "7" {
			t.Fatalf("page at %d total=%s, want 7", offset, read.total)
		}
		seen = append(seen, read.reportIDs(t)...)
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("paged reports across a tie = %v, want %v", seen, want)
	}

	// A tie at one entry time: a report, a task, and a proposal order by
	// their sort keys, the same way on every sort.
	at := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	task := inboxItem{Band: "decisions", LastEventAt: at, SortID: "261128-aaaaaa"}
	proposal := inboxItem{Kind: "document_proposal", Band: "decisions", LastEventAt: at, SortID: "component-x"}
	first := inboxItem{Kind: feedbackReportInboxKind, Band: "decisions", LastEventAt: at, SortID: inboxFeedbackReportSortPrefix + "rpt-261128-000001"}
	second := inboxItem{Kind: feedbackReportInboxKind, Band: "decisions", LastEventAt: at, SortID: inboxFeedbackReportSortPrefix + "rpt-261128-000002"}
	earlier := inboxItem{Kind: feedbackReportInboxKind, Band: "decisions", LastEventAt: at.Add(-time.Nanosecond), SortID: inboxFeedbackReportSortPrefix + "rpt-261128-ffffff"}
	deadline := at.Add(time.Hour)
	due := inboxItem{Band: "decisions", LastEventAt: at.Add(time.Hour), Deadline: &deadline, SortID: "261128-zzzzzz"}
	sorted := []string{due.SortID, earlier.SortID, task.SortID, proposal.SortID, first.SortID, second.SortID}
	for _, order := range [][]inboxItem{{second, first, proposal, task, earlier, due}, {first, task, due, second, earlier, proposal}} {
		sortInbox(order)
		got := make([]string, len(order))
		for i, item := range order {
			got[i] = item.SortID
		}
		if !slices.Equal(got, sorted) {
			t.Fatalf("sorted = %v, want %v", got, sorted)
		}
	}
}

// The MCP inbox returns the report item as the route does.
func TestFeedbackInboxMCPItemIsTheRouteItem(t *testing.T) {
	h := newReportRoutesHarness(t)
	report := h.storeReport("mcp", nil)
	h.endLinking(report, nil, nil)
	route := h.inbox("")
	page := readAs(t, h.handler, h.manager(), "list_operator_inbox", map[string]any{"workspace_id": "alpha"})
	if len(page.Items) != 1 || len(route.items) != 1 {
		t.Fatalf("mcp items=%d route items=%d", len(page.Items), len(route.items))
	}
	mcp := decodeFields(t, page.Items[0])
	var fromMCP, fromRoute servedFeedbackReport
	if err := json.Unmarshal(mcp["report"], &fromMCP); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(route.items[0]["report"], &fromRoute); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromMCP, fromRoute) || fromMCP.ID != report.ID || string(mcp["kind"]) != `"feedback_report"` || string(mcp["last_event_at"]) != string(route.items[0]["last_event_at"]) {
		t.Fatalf("mcp item %s, route item %s", page.Items[0], route.items[0]["report"])
	}
}

// feedbackInboxStepStore runs a hook once at one step of the Inbox read.
type feedbackInboxStepStore struct {
	store.Backend
	beforeProposals, beforeActs func()
}

func (s *feedbackInboxStepStore) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	if hook := s.beforeProposals; hook != nil {
		s.beforeProposals = nil
		hook()
	}
	return s.Backend.ListPendingProposals(ctx)
}

func (s *feedbackInboxStepStore) ListFactoryActs(ctx context.Context, since time.Time, limit int) ([]nextaction.Act, error) {
	if hook := s.beforeActs; hook != nil {
		s.beforeActs = nil
		hook()
	}
	return s.Backend.ListFactoryActs(ctx, since, limit)
}

// An act, a close, or a reopening between two steps of the Inbox read
// changes the state the read started from, so its answer is sent once with
// no tag and is not kept; the next read builds again. Every answer's items,
// total, and page come from one built list, so they agree
// (component-next-action invariant 58).
func TestFeedbackInboxChangeBetweenReadStepsIsNotKept(t *testing.T) {
	for _, step := range []string{"before the report read", "after the report read"} {
		t.Run(step, func(t *testing.T) {
			h := newReportRoutesHarness(t)
			h.server.UseReadMemoClock(h.clock)
			h.server.Changes = nil
			first := h.storeReport("first", nil)
			second := h.storeReport("second", nil)
			h.endLinking(first, nil, nil)
			h.endLinking(second, nil, nil)
			steps := &feedbackInboxStepStore{Backend: h.st}
			h.server.Store = steps
			closeFirst := func() {
				h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActClose, ReportID: first.ID, Reason: "Done."})
			}
			if step == "before the report read" {
				steps.beforeProposals = closeFirst
			} else {
				steps.beforeActs = closeFirst
			}
			changed := h.inbox("?limit=1")
			if changed.etag != "" {
				t.Fatalf("an answer read across a change carries tag %q", changed.etag)
			}
			ids := changed.reportIDs(t)
			want := map[string][]string{"before the report read": {second.ID}, "after the report read": {first.ID}}[step]
			if !slices.Equal(ids, want) {
				t.Fatalf("changed read = %v, want %v", ids, want)
			}
			wantTotal := map[string]string{"before the report read": "1", "after the report read": "2"}[step]
			if changed.total != wantTotal {
				t.Fatalf("changed read total=%s, want %s from the same list", changed.total, wantTotal)
			}
			// Nothing was kept: the next read builds from the state after
			// the close and is tagged.
			next := h.inbox("?limit=1")
			if next.etag == "" || !slices.Equal(next.reportIDs(t), []string{second.ID}) || next.total != "1" {
				t.Fatalf("next read etag=%q items=%v total=%s", next.etag, next.reportIDs(t), next.total)
			}
		})
	}
}

// inboxWithoutReports drops the feedback_report items from an Inbox body.
// A report's Inbox item is its one call on attention, so a check that a
// report changes no delivery read compares the Inbox without it
// (component-feedback-reports invariant 1).
func inboxWithoutReports(t *testing.T, body string) string {
	t.Helper()
	var page map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode inbox %s: %v", body, err)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(page["items"], &items); err != nil {
		t.Fatalf("decode inbox items %s: %v", page["items"], err)
	}
	kept := make([]map[string]json.RawMessage, 0, len(items))
	for _, item := range items {
		if string(item["kind"]) != `"`+feedbackReportInboxKind+`"` {
			kept = append(kept, item)
		}
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	page["items"] = encoded
	out, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// More than 1,000 open reports that people already acted on, across six
// environments each under the open-report limit, no longer fail the Inbox:
// REST and MCP list the task and proposal decisions, the count holds them,
// and a valid new report is listed and counted once (review finding 1;
// req-production-feedback AC-6.5; req-operator-next-action AC-2.7, AC-2.12).
func TestFeedbackInboxAboveOneThousandOpenReportsActedOn(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.server.UseReadMemoClock(h.clock)
	crate := h.crate("Acted on")
	const environments, perEnvironment = 6, 170
	for e := range environments {
		environment := h.create(fmt.Sprintf(`{"name":"Region %d"}`, e))
		key := h.issue(environment.ID)
		for batch := 0; batch*store.MaxFeedbackReportEntries < perEnvironment; batch++ {
			entries := []store.FeedbackReportEntry{}
			for i := batch * store.MaxFeedbackReportEntries; i < min((batch+1)*store.MaxFeedbackReportEntries, perEnvironment); i++ {
				entries = append(entries, store.FeedbackReportEntry{Kind: store.FeedbackReportError, Key: fmt.Sprintf("region-%d-%03d", e, i), Summary: "Acted on"})
			}
			h.advance(time.Second)
			if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: environment.ID, KeyID: key.ID, ArrivedAt: h.clock(), Entries: entries}); err != nil {
				t.Fatal(err)
			}
		}
		reports, err := h.st.ListEnvironmentFeedbackReports(h.ctx, environment.ID)
		if err != nil || len(reports) != perEnvironment {
			t.Fatalf("environment %d holds %d reports, %v", e, len(reports), err)
		}
		for _, report := range reports {
			// Linking a crate is a person's act that keeps the report open.
			h.storeAct(store.FeedbackReportAct{Kind: store.FeedbackReportActLink, ReportID: report.ID, CrateID: crate.ID})
		}
	}
	open, err := h.st.ListFeedbackReports(h.ctx, store.FeedbackReportQuery{State: store.FeedbackReportOpen, Limit: 1, RefuseAbove: store.MaxFeedbackReportQueryLimit})
	if err != nil || open.Total != environments*perEnvironment || open.Total <= store.MaxFeedbackReportQueryLimit {
		t.Fatalf("open reports = %d, %v; the case needs more than %d", open.Total, err, store.MaxFeedbackReportQueryLimit)
	}
	// One task decision and one document proposal.
	if err := h.st.CreateTask(h.ctx, core.Task{ID: "261128-decide", Workspace: "alpha", Repo: "app", Branch: "decide", Title: "Approve the plan", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, err = h.st.ProposeDecision(h.userCtx, core.Decision{Statement: "Keep the Inbox whole.", Context: "Test.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	kinds := func(read feedbackInboxRead) (tasks, proposals int) {
		for _, item := range read.items {
			switch {
			case item["task"] != nil:
				var band string
				_ = json.Unmarshal(item["band"], &band)
				if band == "decisions" {
					tasks++
				}
			case item["proposal"] != nil:
				proposals++
			}
		}
		return tasks, proposals
	}
	read := h.requireFeedbackInbox([]string{})
	if tasks, proposals := kinds(read); tasks != 1 || proposals != 1 {
		t.Fatalf("inbox lists %d task and %d proposal decisions, want 1 and 1", tasks, proposals)
	}
	if attention := h.attention(); attention.OperatorDecisions != 2 || attention.TaskCount != 1 || attention.PendingProposalCount != 1 {
		t.Fatalf("attention = %+v, want 2 decisions", attention)
	}
	page := readAs(t, h.handler, h.manager(), "list_operator_inbox", map[string]any{"workspace_id": "alpha"})
	if len(page.Items) != 2 {
		t.Fatalf("MCP inbox lists %d items, want the task and the proposal", len(page.Items))
	}
	// A valid new report is listed and counted once.
	fresh := h.storeReport("fresh", nil)
	read = h.requireFeedbackInbox([]string{fresh.ID})
	if tasks, proposals := kinds(read); tasks != 1 || proposals != 1 {
		t.Fatalf("after a new report the inbox lists %d task and %d proposal decisions", tasks, proposals)
	}
	if attention := h.attention(); attention.OperatorDecisions != 3 {
		t.Fatalf("attention after a new report = %+v, want 3", attention)
	}
	page = readAs(t, h.handler, h.manager(), "list_operator_inbox", map[string]any{"workspace_id": "alpha"})
	if len(page.Items) != 3 {
		t.Fatalf("MCP inbox lists %d items after a new report, want 3", len(page.Items))
	}
}

// storeInboxReports stores perEnvironment new reports in each of
// environments new environments, under the open-report limit of each, with
// a summary of the given length.
func (h *reportRoutesHarness) storeInboxReports(environments, perEnvironment, summaryLength int) {
	h.t.Helper()
	for e := range environments {
		environment := h.create(fmt.Sprintf(`{"name":"Inbox region %d"}`, e))
		key := h.issue(environment.ID)
		for batch := 0; batch*store.MaxFeedbackReportEntries < perEnvironment; batch++ {
			entries := []store.FeedbackReportEntry{}
			for i := batch * store.MaxFeedbackReportEntries; i < min((batch+1)*store.MaxFeedbackReportEntries, perEnvironment); i++ {
				entries = append(entries, store.FeedbackReportEntry{Kind: store.FeedbackReportError, Key: fmt.Sprintf("inbox-%d-%03d", e, i), Summary: strings.Repeat("s", summaryLength)})
			}
			h.advance(time.Second)
			if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: environment.ID, KeyID: key.ID, ArrivedAt: h.clock(), Entries: entries}); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// The limits of the reads at their largest valid state: more than 1,000
// reports in the Inbox, across six environments under the open-report
// limit. REST lists every one through its pages with one total, and the
// count equals the membership. The MCP read framework's limits of 1,000
// items and 1 MiB for one read are the framework's own: above them
// list_operator_inbox answers its in-band refusal and asks for a narrower
// read, and a band other than decisions still answers.
func TestFeedbackInboxLimitsAboveOneThousandReportsInTheInbox(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.server.UseReadMemoClock(h.clock)
	const environments, perEnvironment = 6, 168
	h.storeInboxReports(environments, perEnvironment, 20)
	total := environments * perEnvironment
	if err := h.st.CreateTask(h.ctx, core.Task{ID: "261128-decide", Workspace: "alpha", Repo: "app", Branch: "decide", Title: "Approve the plan", State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if attention := h.attention(); attention.OperatorDecisions != total+1 {
		t.Fatalf("attention = %+v, want %d", attention, total+1)
	}
	// REST pages through every decision: 200 at a time, one total, each
	// report once, and the task decision listed.
	seen := map[string]bool{}
	task := false
	for offset := 0; offset < total+1; offset += 200 {
		read := h.inbox(fmt.Sprintf("?limit=200&offset=%d", offset))
		if read.total != strconv.Itoa(total+1) {
			t.Fatalf("page at %d total=%s, want %d", offset, read.total, total+1)
		}
		for _, id := range read.reportIDs(t) {
			if seen[id] {
				t.Fatalf("report %s listed twice", id)
			}
			seen[id] = true
		}
		for _, item := range read.items {
			if item["task"] != nil {
				task = true
			}
		}
	}
	if len(seen) != total || !task {
		t.Fatalf("REST listed %d reports and task=%v, want %d and the task", len(seen), task, total)
	}
	// MCP: above 1,000 items the framework refuses the read in band.
	if refusal := refusalAs(t, h.handler, h.manager(), "list_operator_inbox", map[string]any{"workspace_id": "alpha"}); !strings.Contains(refusal, "read exceeds 1000 items") {
		t.Fatalf("MCP refusal = %q", refusal)
	}
	if page := readAs(t, h.handler, h.manager(), "list_operator_inbox", map[string]any{"workspace_id": "alpha", "band": "automatic"}); len(page.Items) != 0 {
		t.Fatalf("MCP automatic band lists %d items", len(page.Items))
	}
}

// Under 1,000 items, the reports' size can pass the MCP read's 1 MiB
// snapshot budget: the framework refuses in band, and REST still lists them.
func TestFeedbackInboxLimitsMCPSnapshotBytes(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.server.UseReadMemoClock(h.clock)
	h.storeInboxReports(4, 175, 1000) // the longest summary, 1,000 characters
	if refusal := refusalAs(t, h.handler, h.manager(), "list_operator_inbox", map[string]any{"workspace_id": "alpha"}); !strings.Contains(refusal, "snapshot byte budget") {
		t.Fatalf("MCP refusal = %q", refusal)
	}
	if read := h.inbox("?limit=200"); read.total != "700" || len(read.reportIDs(t)) != 200 {
		t.Fatalf("REST total=%s first page=%d", read.total, len(read.reportIDs(t)))
	}
	if attention := h.attention(); attention.OperatorDecisions != 700 {
		t.Fatalf("attention = %+v, want 700", attention)
	}
}
