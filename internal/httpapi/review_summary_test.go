package httpapi

// Review round summary surfaces over the full handler: MCP
// submit_review_summary and REST POST /v1/tasks/{id}/review-rounds/{round}/summaries
// (req-review-gates-evidence AC-10.3, AC-10.4, AC-10.7; DEC-19; DEC-79;
// feature-review-round-summary SU-4, SU-7).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// summaryServiceStore gives the work-order service the same membership
// authority the server consults, as a durable backend does.
type summaryServiceStore struct {
	store.Store
	store.MembershipStore
}

// newSummarySurfaceFixture is the governance fixture with the service's
// store bound to the fixture's live membership authority.
func newSummarySurfaceFixture(t *testing.T) *governanceAuthFixture {
	t.Helper()
	f := newGovernanceAuthFixture(t)
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: governanceWorkspace, Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
	}
	f.server.WorkOrders = &workorder.Service{Store: summaryServiceStore{Store: f.store, MembershipStore: f.members}, ConfigProvider: provider}
	f.handler = f.server.Handler()
	return f
}

// settledSummaryTask stores a task whose submitted implementation was
// reviewed in round 1: seat 1 requested changes and seat 2 approved, through
// the real claim and acceptance paths. It returns the task and seat order IDs;
// each seat's session is "<order>-session".
func (f *governanceAuthFixture) settledSummaryTask(name string) (string, []string) {
	f.t.Helper()
	implement := f.order(name, core.StageImplement)
	taskID := "gov-task-" + name
	f.claimRun(implement, "usr_owner", implement+"-session")
	submitted, err := f.store.GetWorkOrder(f.ctx, implement)
	if err != nil {
		f.t.Fatal(err)
	}
	submitted.State = core.WorkOrderSubmitted
	if err = storetest.For(f.store).UpdateWorkOrder(f.ctx, submitted, core.WorkOrderCmdSubmitForReview); err != nil {
		f.t.Fatal(err)
	}
	var jobs []core.Job
	var orders []core.WorkOrder
	var ids []string
	for seat := 1; seat <= 2; seat++ {
		id := fmt.Sprintf("%s-review-1-seat-%d", taskID, seat)
		ids = append(ids, id)
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)})
	}
	if err = storetest.CreateReviewRound(f.ctx, f.store, taskID, jobs, orders); err != nil {
		f.t.Fatal(err)
	}
	for i, verdict := range []string{"changes_requested", "approve"} {
		f.claimRun(ids[i], "usr_seat", ids[i]+"-session")
		decision := core.ReviewDecision{TaskID: taskID, JobID: ids[i], ReviewWorkOrderID: ids[i], ReviewRound: 1, ReviewSeat: i + 1, ClaimSession: ids[i] + "-session", Verdict: verdict, ReasonCode: "verified", Summary: "seat verdict", Feedback: "seat feedback", PolicyVersion: 1, MaxBounces: 5}
		if err = taskops.New(f.store).AcceptReviewDecision(f.ctx, decision); err != nil {
			f.t.Fatal(err)
		}
	}
	return taskID, ids
}

func summaryArgs(taskID, requestID string, extra map[string]any) map[string]any {
	args := map[string]any{
		"workspace_id": governanceWorkspace, "task_id": taskID, "review_round": 1, "request_id": requestID,
		"summary": "Seat 1 found one defect.", "author_model": "claude-opus-5-5",
		"findings": []any{map[string]any{"severity": "blocking", "problem": "Defect.", "location": "internal/x.go", "fix": "Fix it.", "seats": []any{1}}},
	}
	for key, value := range extra {
		if value == nil {
			delete(args, key)
			continue
		}
		args[key] = value
	}
	return args
}

func (f *governanceAuthFixture) summaryEventCount(taskID string) (summaries, total int) {
	f.t.Helper()
	events, err := f.store.ListEvents(f.ctx, taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == store.ReviewSummaryRecordedEvent {
			summaries++
		}
	}
	return summaries, len(events)
}

// lifecycleState is the AC-10.7 snapshot: everything but summary events.
func (f *governanceAuthFixture) lifecycleState(taskID string) []any {
	f.t.Helper()
	task, err := f.store.GetTask(f.ctx, taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	orders, err := f.store.ListTaskWorkOrdersStored(f.ctx, taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	jobs, err := f.store.ListJobs(f.ctx, taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	interventions, err := f.store.ListInterventions(f.ctx, taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	events, err := f.store.ListEvents(f.ctx, taskID)
	if err != nil {
		f.t.Fatal(err)
	}
	var lifecycle []core.Event
	for _, event := range events {
		if event.Kind != store.ReviewSummaryRecordedEvent {
			lifecycle = append(lifecycle, event)
		}
	}
	return []any{task, orders, jobs, interventions, lifecycle}
}

func (f *governanceAuthFixture) postSummary(token, taskID, round, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/review-rounds/"+round+"/summaries?workspace_id="+governanceWorkspace, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func requireSummaryResultFields(t *testing.T, label, text, taskID string, version int) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("%s result %q: %v", label, text, err)
	}
	keys := slices.Sorted(func(yield func(string) bool) {
		for key := range result {
			if !yield(key) {
				return
			}
		}
	})
	if !reflect.DeepEqual(keys, []string{"event_id", "recorded_at", "review_round", "task_id", "version"}) {
		t.Fatalf("%s result fields=%v", label, keys)
	}
	if result["task_id"] != taskID || result["review_round"] != float64(1) || result["version"] != float64(version) || result["event_id"] == float64(0) {
		t.Fatalf("%s result=%v, want version %d of %s", label, result, version, taskID)
	}
	if _, err := time.Parse(time.RFC3339Nano, result["recorded_at"].(string)); err != nil {
		t.Fatalf("%s recorded_at=%v", label, result["recorded_at"])
	}
	return result
}

// TestSubmitReviewSummaryMCP drives submit_review_summary through tools/list
// and tools/call over the full /mcp handler.
func TestSubmitReviewSummaryMCP(t *testing.T) {
	f := newSummarySurfaceFixture(t)
	userToken, _ := f.user("usr_executor", core.WorkspaceRoleExecutor)
	_, seatAgent := f.user("usr_seat", core.WorkspaceRoleExecutor)
	taskID, seats := f.settledSummaryTask("summary-mcp")

	if got := mcpCapabilities["submit_review_summary"]; got != core.CapabilityClaimWork {
		t.Fatalf("capability=%q, want claim_work", got)
	}
	if claimantBoundMCPTools["submit_review_summary"] {
		t.Fatal("submit_review_summary must not be claimant-bound: user callers hold no order")
	}
	var schema map[string]any
	for _, tool := range mcpTools() {
		if tool["name"] == "submit_review_summary" {
			schema = tool["inputSchema"].(map[string]any)
		}
	}
	if schema == nil {
		t.Fatal("submit_review_summary is not registered")
	}
	if required := schema["required"].([]string); !reflect.DeepEqual(required, []string{"task_id", "review_round", "request_id", "summary", "findings"}) {
		t.Fatalf("required=%v", required)
	}
	properties := schema["properties"].(map[string]any)
	for _, name := range []string{"workspace_id", "task_id", "review_round", "request_id", "summary", "findings", "author_model", "work_order_id", "session_id"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("schema lacks %s", name)
		}
	}

	text, isErr := f.call(userToken, "submit_review_summary", summaryArgs(taskID, "user-1", nil))
	if isErr {
		t.Fatalf("user summary refused: %s", text)
	}
	first := requireSummaryResultFields(t, "user", text, taskID, 1)
	if text, isErr = f.call(userToken, "submit_review_summary", summaryArgs(taskID, "user-1", nil)); isErr {
		t.Fatalf("replay refused: %s", text)
	}
	if replay := requireSummaryResultFields(t, "replay", text, taskID, 1); !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay=%v, want %v", replay, first)
	}
	if text, isErr = f.call(seatAgent, "submit_review_summary", summaryArgs(taskID, "seat-1", map[string]any{"work_order_id": seats[0], "session_id": seats[0] + "-session"})); isErr {
		t.Fatalf("completed seat refused: %s", text)
	}
	requireSummaryResultFields(t, "seat", text, taskID, 2)

	for label, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"uncited seat":       {summaryArgs(taskID, "uncited", map[string]any{"findings": []any{}}), "review_summary_invalid"},
		"unknown seat":       {summaryArgs(taskID, "unknown", map[string]any{"findings": []any{map[string]any{"severity": "blocking", "problem": "p", "seats": []any{1, 7}}}}), "seat 7"},
		"unsettled round":    {summaryArgs(taskID, "unsettled", map[string]any{"review_round": 2}), "review_summary_round_not_settled"},
		"conflicting replay": {summaryArgs(taskID, "user-1", map[string]any{"summary": "Different."}), "review_summary_conflict"},
		"string round":       {summaryArgs(taskID, "string-round", map[string]any{"review_round": "1"}), "review_round must be an integer"},
		"fractional round":   {summaryArgs(taskID, "fraction", map[string]any{"review_round": 1.5}), "review_round must be an integer"},
		"fractional seat":    {summaryArgs(taskID, "seat-fraction", map[string]any{"findings": []any{map[string]any{"severity": "blocking", "problem": "p", "seats": []any{1.5}}}}), "invalid_argument: findings"},
		"unknown field":      {summaryArgs(taskID, "extra", map[string]any{"findings": []any{map[string]any{"severity": "blocking", "problem": "p", "seats": []any{1}, "owner": "x"}}}), "invalid_argument: findings"},
		"mistyped summary":   {summaryArgs(taskID, "typed", map[string]any{"summary": 7}), "summary must be a string"},
	} {
		before, _ := f.summaryEventCount(taskID)
		text, isErr := f.call(userToken, "submit_review_summary", tc.args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Fatalf("%s isError=%v text=%q, want %q", label, isErr, text, tc.want)
		}
		if after, _ := f.summaryEventCount(taskID); after != before {
			t.Fatalf("%s appended a summary", label)
		}
	}
}

// TestReviewSummaryREST drives the user-only REST route through the routed
// middleware: 201 for a new version, 200 for a replay, 400 for invalid input,
// 409 for unsettled rounds, conflicts, and the uniform refusal, and no
// lifecycle side effects (AC-10.7).
func TestReviewSummaryREST(t *testing.T) {
	f := newSummarySurfaceFixture(t)
	userToken, agentToken := f.user("usr_executor", core.WorkspaceRoleExecutor)
	viewerToken, _ := f.user("usr_viewer", core.WorkspaceRoleViewer)
	taskID, _ := f.settledSummaryTask("summary-rest")
	before := f.lifecycleState(taskID)
	body := `{"request_id":"rest-1","summary":"  Seat 1 found one defect.  ","author_model":"claude-opus-5-5","findings":[{"severity":"blocking","problem":"Defect.","seats":[1]}]}`

	created := f.postSummary(userToken, taskID, "1", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	first := requireSummaryResultFields(t, "create", created.Body.String(), taskID, 1)
	replayed := f.postSummary(userToken, taskID, "1", body)
	if replayed.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	if replay := requireSummaryResultFields(t, "replay", replayed.Body.String(), taskID, 1); !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay=%v, want %v", replay, first)
	}
	events, err := f.store.ListEvents(f.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var payload store.ReviewSummaryPayload
	for _, event := range events {
		if event.Kind == store.ReviewSummaryRecordedEvent {
			if err = json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	wantAuthor := store.ReviewSummaryAuthor{ActorID: store.UserActorID("usr_executor"), ActorRole: core.ActorUser, Model: "claude-opus-5-5", ModelEnforcement: "self-reported"}
	if payload.Author != wantAuthor || payload.Summary != "Seat 1 found one defect." || payload.WorkspaceID != governanceWorkspace {
		t.Fatalf("payload=%+v", payload)
	}

	refusal := f.postSummary(userToken, "gov-task-summary-absent", "1", body)
	for label, tc := range map[string]struct {
		token, task, round, body string
		status                   int
		want                     string
	}{
		"uncited seat":       {userToken, taskID, "1", `{"request_id":"r2","summary":"s","findings":[]}`, http.StatusBadRequest, "uncited seats: 1"},
		"bad severity":       {userToken, taskID, "1", `{"request_id":"r3","summary":"s","findings":[{"severity":"major","problem":"p","seats":[1]}]}`, http.StatusBadRequest, "severity"},
		"malformed json":     {userToken, taskID, "1", `{"request_id":`, http.StatusBadRequest, "invalid_argument"},
		"empty body":         {userToken, taskID, "1", ``, http.StatusBadRequest, "request body is required"},
		"fractional seat":    {userToken, taskID, "1", `{"request_id":"r4","summary":"s","findings":[{"severity":"blocking","problem":"p","seats":[1.5]}]}`, http.StatusBadRequest, "invalid_argument: findings"},
		"non-integer round":  {userToken, taskID, "one", body, http.StatusBadRequest, "review round must be an integer"},
		"unsettled round":    {userToken, taskID, "2", body, http.StatusConflict, "review_summary_round_not_settled"},
		"round zero":         {userToken, taskID, "0", body, http.StatusConflict, "review_summary_round_not_settled"},
		"conflicting replay": {userToken, taskID, "1", strings.Replace(body, "one defect", "two defects", 1), http.StatusConflict, "review_summary_conflict"},
		"missing task":       {userToken, "gov-task-summary-absent", "1", body, http.StatusConflict, store.ErrWorkOrderClaimUnauthorized.Error()},
		"viewer":             {viewerToken, taskID, "1", body, http.StatusNotFound, "workspace_not_found"},
		"agent credential":   {agentToken, taskID, "1", body, http.StatusUnauthorized, "unauthorized"},
	} {
		response := f.postSummary(tc.token, tc.task, tc.round, tc.body)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.want) {
			t.Fatalf("%s status=%d body=%q, want %d containing %q", label, response.Code, response.Body.String(), tc.status, tc.want)
		}
	}
	if refusal.Body.String() != store.ErrWorkOrderClaimUnauthorized.Error()+"\n" {
		t.Fatalf("missing-task refusal body=%q", refusal.Body.String())
	}
	if summaries, _ := f.summaryEventCount(taskID); summaries != 1 {
		t.Fatalf("summary events=%d, want 1", summaries)
	}
	if after := f.lifecycleState(taskID); !reflect.DeepEqual(before, after) {
		t.Fatalf("REST summary changed lifecycle state:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// TestReviewSummarySurfaceAuthorization pins SU-3 at the MCP boundary: who
// is admitted, and one identical refusal for everyone else, including the
// capability, workspace, run-child, and worker refusals the generic MCP
// boundary would otherwise render differently (DEC-19).
func TestReviewSummarySurfaceAuthorization(t *testing.T) {
	f := newSummarySurfaceFixture(t)
	userToken, _ := f.user("usr_executor", core.WorkspaceRoleExecutor)
	viewerToken, viewerAgent := f.user("usr_viewer", core.WorkspaceRoleViewer)
	_, seatAgent := f.user("usr_seat", core.WorkspaceRoleExecutor)
	taskID, seats := f.settledSummaryTask("summary-auth")
	otherTask, otherSeats := f.settledSummaryTask("summary-auth-other")
	childToken := "child-summary"
	f.credentials[childToken] = core.AuthenticatedCredential{ID: "agt_child_summary", OwnerUserID: "usr_seat", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, RunWorkspaceID: governanceWorkspace, RunWorkOrderID: seats[1], RunSessionID: seats[1] + "-session"}
	workerToken := "worker-summary-token"
	sum := sha256.Sum256([]byte(workerToken))
	if err := createOwnedWorker(f.store, f.ctx, core.Worker{ID: "worker-summary", Workspace: governanceWorkspace, OwnerUserID: "usr_seat", Name: "worker-summary", CredentialHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	seatArgs := func(order int, request string) map[string]any {
		return summaryArgs(taskID, request, map[string]any{"work_order_id": seats[order], "session_id": seats[order] + "-session"})
	}

	before := f.lifecycleState(taskID)
	for label, tc := range map[string]struct {
		token string
		args  map[string]any
	}{
		"user without an order":       {userToken, summaryArgs(taskID, "admit-user", nil)},
		"completed seat agent":        {seatAgent, seatArgs(0, "admit-seat")},
		"run child bound to its seat": {childToken, seatArgs(1, "admit-child")},
		"submitted implementation":    {seatAgent, summaryArgs(taskID, "admit-implement", map[string]any{"work_order_id": "gov-order-summary-auth", "session_id": "gov-order-summary-auth-session"})},
	} {
		if text, isErr := f.call(tc.token, "submit_review_summary", tc.args); isErr {
			t.Fatalf("%s refused: %s", label, text)
		}
	}
	if after := f.lifecycleState(taskID); !reflect.DeepEqual(before, after) {
		t.Fatalf("admitted summaries changed lifecycle state")
	}

	uniform := store.ErrWorkOrderClaimUnauthorized.Error()
	_, beforeTotal := f.summaryEventCount(taskID)
	_, beforeOther := f.summaryEventCount(otherTask)
	for label, tc := range map[string]struct {
		token string
		args  map[string]any
	}{
		"viewer user":                    {viewerToken, summaryArgs(taskID, "viewer", nil)},
		"viewer user, unsettled round":   {viewerToken, summaryArgs(taskID, "viewer-unsettled", map[string]any{"review_round": 9})},
		"viewer's agent with a seat":     {viewerAgent, seatArgs(0, "viewer-agent")},
		"user in a foreign workspace":    {userToken, summaryArgs(taskID, "foreign", map[string]any{"workspace_id": "beta"})},
		"user naming a missing task":     {userToken, summaryArgs("gov-task-summary-absent", "absent", nil)},
		"agent without an order":         {seatAgent, summaryArgs(taskID, "no-order", nil)},
		"agent with another session":     {seatAgent, summaryArgs(taskID, "other-session", map[string]any{"work_order_id": seats[0], "session_id": seats[1] + "-session"})},
		"agent with another task's seat": {seatAgent, summaryArgs(taskID, "other-task", map[string]any{"work_order_id": otherSeats[0], "session_id": otherSeats[0] + "-session"})},
		"agent naming a missing task":    {seatAgent, summaryArgs("gov-task-summary-absent", "agent-absent", map[string]any{"work_order_id": seats[0], "session_id": seats[0] + "-session"})},
		"run child naming another seat":  {childToken, seatArgs(0, "child-other")},
		"worker credential":              {workerToken, seatArgs(0, "worker")},
	} {
		text, isErr := f.call(tc.token, "submit_review_summary", tc.args)
		if !isErr || text != uniform {
			t.Fatalf("%s isError=%v text=%q, want the uniform refusal %q", label, isErr, text, uniform)
		}
	}
	if _, after := f.summaryEventCount(taskID); after != beforeTotal {
		t.Fatalf("refusals appended events %d -> %d", beforeTotal, after)
	}
	if _, after := f.summaryEventCount(otherTask); after != beforeOther {
		t.Fatalf("refusals appended events to the other task %d -> %d", beforeOther, after)
	}
}
