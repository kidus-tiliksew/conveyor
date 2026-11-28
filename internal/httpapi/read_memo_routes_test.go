package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func newMemoRouteFixture(t *testing.T) *activityConditionalFixture {
	t.Helper()
	f := newActivityConditionalFixture(t)
	f.server.readMemo = &readMemo{clock: func() time.Time { return f.now }}
	f.server.nextActionClock = func() time.Time { return f.now }
	return f
}

func TestMemoPublicRoutesWarmAndExpired(t *testing.T) {
	for _, path := range []string{"/v1/tasks", "/v1/next-actions"} {
		t.Run(path, func(t *testing.T) {
			f := newMemoRouteFixture(t)
			f.task("memo-task", core.TaskQueued)
			cold := f.read(path, "")
			if cold.Code != http.StatusOK || cold.Header().Get("ETag") == "" {
				t.Fatalf("cold status=%d tag=%q body=%s", cold.Code, cold.Header().Get("ETag"), cold.Body.String())
			}
			// One validation read precedes the build and one checks it before
			// the body is published (component-next-action invariant 58).
			if f.counted.calls["ReadActivityInputs"] != 1 || f.counted.calls["ReadActivityValidation"] != 2 {
				t.Fatalf("miss did not share bounded inputs: %v", f.counted.calls)
			}
			f.counted.reset()
			warm := f.read(path, "")
			if warm.Code != http.StatusOK || warm.Body.String() != cold.Body.String() {
				t.Fatalf("warm changed response: status=%d body=%s", warm.Code, warm.Body.String())
			}
			unchanged := f.read(path, cold.Header().Get("ETag"))
			if unchanged.Code != http.StatusNotModified || unchanged.Body.Len() != 0 {
				t.Fatalf("conditional status=%d body=%s", unchanged.Code, unchanged.Body.String())
			}
			if len(f.counted.calls) != 0 {
				t.Fatalf("warm payload/validation reads: %v", f.counted.calls)
			}
			f.now = f.now.Add(readMemoLifetime)
			expired := f.read(path, cold.Header().Get("ETag"))
			if expired.Code != http.StatusOK || expired.Header().Get("ETag") == cold.Header().Get("ETag") {
				t.Fatalf("expired tag admitted: status=%d", expired.Code)
			}
			if f.counted.calls["ReadActivityInputs"] != 1 || f.counted.calls["ReadActivityValidation"] != 2 {
				t.Fatalf("expiry reads: %v", f.counted.calls)
			}
		})
	}
}

func TestMemoPublicRefreshFailureNeverServesExpiredBody(t *testing.T) {
	for _, path := range []string{"/v1/tasks", "/v1/next-actions"} {
		t.Run(path, func(t *testing.T) {
			f := newMemoRouteFixture(t)
			f.task("memo-failure", core.TaskQueued)
			cold := f.read(path, "")
			f.now = f.now.Add(readMemoLifetime)
			f.counted.validationErr = errors.New("unavailable")
			for range 2 {
				failed := f.read(path, cold.Header().Get("ETag"))
				if failed.Code != http.StatusInternalServerError || failed.Header().Get("ETag") != "" {
					t.Fatalf("stale success after failure: status=%d tag=%q", failed.Code, failed.Header().Get("ETag"))
				}
			}
			f.counted.validationErr = nil
			if got := f.read(path, cold.Header().Get("ETag")); got.Code != http.StatusOK {
				t.Fatalf("recovery=%d", got.Code)
			}
		})
	}
}

func TestMemoPublicNoticesAndPeerExpiry(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint("local=", local), func(t *testing.T) {
			f := newMemoRouteFixture(t)
			hub := changes.NewHub()
			if local {
				f.server.readMemo.subscribe(hub)
			}
			f.task("memo-before", core.TaskQueued)
			cold := f.read("/v1/tasks", "")
			f.task("memo-after", core.TaskQueued)
			if local {
				hub.Publish(changes.Notice{Kind: changes.Task, Workspace: "demo"})
			} else {
				if got := f.read("/v1/tasks", cold.Header().Get("ETag")); got.Code != http.StatusNotModified {
					t.Fatalf("peer warm status=%d", got.Code)
				}
				f.now = f.now.Add(readMemoLifetime)
			}
			got := f.read("/v1/tasks", cold.Header().Get("ETag"))
			if got.Code != http.StatusOK || got.Body.String() == cold.Body.String() {
				t.Fatalf("change not observed: status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
}

func TestMemoPublicScopeAndConfigurationIsolation(t *testing.T) {
	f := newMemoRouteFixture(t)
	f.task("memo-scope", core.TaskQueued)
	cfg := &config.Config{MaxBounces: 1}
	f.server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
	f.server.Credentials = staticCredentialVerifier{
		"reader-a": {ID: "a", OwnerUserID: "a", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"reader-b": {ID: "b", OwnerUserID: "b", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
	}
	f.handler = f.server.Handler()
	token := "reader-a"
	f.authorize = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	cold := f.read("/v1/tasks?limit=1", "")
	if cold.Code != http.StatusOK {
		t.Fatalf("cold=%d %s", cold.Code, cold.Body.String())
	}
	for _, path := range []string{"/v1/tasks?limit=2", "/v1/tasks?limit=1&offset=1", "/v1/next-actions"} {
		if got := f.read(path, cold.Header().Get("ETag")); got.Code != http.StatusOK {
			t.Fatalf("cross-query admitted %s: %d", path, got.Code)
		}
	}
	token = "reader-b"
	if got := f.read("/v1/tasks?limit=1", cold.Header().Get("ETag")); got.Code != http.StatusOK {
		t.Fatalf("cross-caller admitted: %d", got.Code)
	}
	token = "reader-a"
	cfg = &config.Config{MaxBounces: 2}
	if got := f.read("/v1/tasks?limit=1", cold.Header().Get("ETag")); got.Code != http.StatusOK {
		t.Fatalf("changed config admitted: %d", got.Code)
	}
	token = "revoked"
	if got := f.read("/v1/tasks?limit=1", cold.Header().Get("ETag")); got.Code != http.StatusUnauthorized {
		t.Fatalf("cached authorization: %d", got.Code)
	}
}

func TestMemoPublicDeadlineAndMinuteBoundary(t *testing.T) {
	for _, path := range []string{"/v1/tasks", "/v1/next-actions"} {
		t.Run(path, func(t *testing.T) {
			f := newMemoRouteFixture(t)
			f.task("memo-deadline", core.TaskRunning)
			order := core.WorkOrder{ID: "memo-order", TaskID: "memo-deadline", JobID: "memo-order", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: f.now.Add(-time.Minute), QueueDeadline: f.now.Add(time.Hour)}
			f.order(order)
			// The activity horizon every backend exposes is the open review
			// lease; a queued implement order's queue deadline is not part of
			// the next-action projection.
			claim := order
			claim.State, claim.LeaseExpiresAt = core.WorkOrderClaimed, f.now.Add(time.Second)
			payload, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.base.AppendEvent(f.ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", At: f.now.Add(-time.Minute), Payload: payload}); err != nil {
				t.Fatal(err)
			}
			cold := f.read(path, "")
			f.counted.reset()
			f.now = f.now.Add(time.Second)
			if got := f.read(path, cold.Header().Get("ETag")); got.Code != http.StatusOK {
				t.Fatalf("deadline tag survived: %d", got.Code)
			}
			if f.counted.calls["ReadActivityInputs"] != 1 {
				t.Fatalf("deadline did not refresh: %v", f.counted.calls)
			}
			f.now = f.now.Truncate(time.Minute).Add(time.Minute - time.Second)
			cold = f.read(path, "")
			f.now = f.now.Add(time.Second)
			if got := f.read(path, cold.Header().Get("ETag")); got.Code != http.StatusOK {
				t.Fatalf("boundary tag survived: %d", got.Code)
			}
		})
	}
}

func TestMemoPublicEvictsOldestPage(t *testing.T) {
	f := newMemoRouteFixture(t)
	f.task("memo-eviction", core.TaskQueued)
	// Offset is only valid with a limit (parseListPage), so every page states
	// both members explicitly.
	if got := f.read("/v1/tasks?limit=1&offset=0", ""); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	for i := 1; i <= readMemoCapacity; i++ {
		if got := f.read(fmt.Sprintf("/v1/tasks?limit=1&offset=%d", i), ""); got.Code != http.StatusOK {
			t.Fatal(got.Code)
		}
	}
	f.counted.reset()
	if got := f.read("/v1/tasks?limit=1&offset=0", ""); got.Code != http.StatusOK || f.counted.calls["ReadActivityInputs"] != 1 {
		t.Fatalf("old page not rebuilt: status=%d reads=%v", got.Code, f.counted.calls)
	}
}

// memoScopedTasks applies the workspace predicate every durable backend binds
// from the request context. store.NewMemory leaves that scoping to
// ListTaskOperations, so the mem-backed fixture supplies it here.
type memoScopedTasks struct{ store.Store }

func (s memoScopedTasks) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	tasks, err := s.Store.ListTasksFiltered(ctx, filter)
	if err != nil {
		return nil, err
	}
	workspace, scoped := store.WorkspaceFromContext(ctx)
	if !scoped {
		return tasks, nil
	}
	out := make([]core.Task, 0, len(tasks))
	for _, task := range tasks {
		if task.Workspace == workspace {
			out = append(out, task)
		}
	}
	return out, nil
}

type memoConcurrentStore struct {
	store.Store
	validations atomic.Int32
	entered     chan struct{}
	release     chan struct{}
}

func (s *memoConcurrentStore) ReadActivityValidation(ctx context.Context, now time.Time) (store.ActivityValidation, error) {
	if s.validations.Add(1) == 1 {
		close(s.entered)
		<-s.release
	}
	return s.Store.ReadActivityValidation(ctx, now)
}

func TestMemoPublicSimultaneousMissesShareProjection(t *testing.T) {
	st := &memoConcurrentStore{Store: store.NewMemory(), entered: make(chan struct{}), release: make(chan struct{})}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "test-reader"
	now := time.Now().UTC()
	server.readMemo = &readMemo{clock: func() time.Time { return now }}
	handler := server.Handler()
	const count = 12
	responses := make([]*httptest.ResponseRecorder, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
			request.Header.Set("Authorization", "Bearer test-reader")
			responses[i] = httptest.NewRecorder()
			handler.ServeHTTP(responses[i], request)
		}()
	}
	<-st.entered
	close(st.release)
	wg.Wait()
	for _, response := range responses {
		if response.Code != http.StatusOK || response.Body.String() != responses[0].Body.String() {
			t.Fatalf("inconsistent concurrent response: %d %s", response.Code, response.Body.String())
		}
	}
	// One build serves every request: its validation read and its
	// publication check.
	if st.validations.Load() != 2 {
		t.Fatalf("validation refreshes=%d", st.validations.Load())
	}
}

func TestMemoInboxReturnsPrivateMutableResults(t *testing.T) {
	f := newMemoRouteFixture(t)
	f.task("memo-alias", core.TaskQueued)
	opts, err := f.server.nextActionOptionsFor(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.server.readInbox(f.ctx, opts, "", store.TaskFilter{})
	if err != nil || len(first) == 0 {
		t.Fatalf("first=%v err=%v", first, err)
	}
	original := first[0].Task.Title
	first[0].Task.Title = "request-owned mutation"
	f.counted.reset()
	second, err := f.server.readInbox(f.ctx, opts, "", store.TaskFilter{})
	if err != nil || second[0].Task.Title != original {
		t.Fatalf("mutable alias: %v %v", second, err)
	}
	if len(f.counted.calls) != 0 {
		t.Fatalf("warm shared inbox reads: %v", f.counted.calls)
	}
}

func TestMemoPublicMCPInboxKeepsNormalResultAndChecksAdmission(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	if err := s.Store.CreateTask(ctx, core.Task{ID: "memo-mcp", Workspace: "demo", Title: "Memo inbox", State: core.TaskQueued, NextStage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	counted := &activityConditionalStore{Store: s.Store, calls: map[string]int{}}
	s.Store = counted
	now := time.Now().UTC()
	s.readMemo = &readMemo{clock: func() time.Time { return now }}
	cold, failure := mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure != "" {
		t.Fatal(failure)
	}
	counted.reset()
	warm, failure := mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure != "" {
		t.Fatal(failure)
	}
	if fmt.Sprint(cold.Items) != fmt.Sprint(warm.Items) {
		t.Fatalf("MCP result changed: cold=%v warm=%v", cold.Items, warm.Items)
	}
	if counted.calls["ReadActivityInputs"] != 0 || counted.calls["ReadActivityValidation"] != 0 {
		t.Fatalf("warm MCP rebuilt projection: %v", counted.calls)
	}
	_, failure = mcpReadCall(t, s, "denied", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure == "" {
		t.Fatal("memo bypassed workspace admission")
	}
}

func TestMemoPublicReviewLeaseExpiresBeforeTTL(t *testing.T) {
	for _, path := range []string{"/v1/tasks", "/v1/next-actions"} {
		t.Run(path, func(t *testing.T) {
			f := newMemoRouteFixture(t)
			f.task("memo-review", core.TaskRunning)
			order := core.WorkOrder{ID: "memo-review-order", TaskID: "memo-review", JobID: "memo-review-order", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1}
			f.order(order)
			claim := order
			claim.State, claim.LeaseExpiresAt = core.WorkOrderClaimed, f.now.Add(time.Second)
			payload, err := json.Marshal(claim)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.base.AppendEvent(f.ctx, core.Event{TaskID: order.TaskID, JobID: order.JobID, Kind: "work_order.claimed", At: f.now.Add(-time.Minute), Payload: payload}); err != nil {
				t.Fatal(err)
			}
			cold := f.read(path, "")
			if cold.Code != http.StatusOK {
				t.Fatalf("cold=%d %s", cold.Code, cold.Body.String())
			}
			f.counted.reset()
			f.now = f.now.Add(time.Second)
			if got := f.read(path, cold.Header().Get("ETag")); got.Code != http.StatusOK || f.counted.calls["ReadActivityInputs"] != 1 {
				t.Fatalf("lease tag survived: status=%d reads=%v", got.Code, f.counted.calls)
			}
		})
	}
}

func TestMemoPublicConfigurationErrorDoesNotReuseAuthorization(t *testing.T) {
	f := newMemoRouteFixture(t)
	f.task("memo-config-error", core.TaskQueued)
	cold := f.read("/v1/tasks", "")
	f.server.ConfigProvider = func(context.Context) (*config.Config, error) { return nil, errors.New("configuration unavailable") }
	if got := f.read("/v1/tasks", cold.Header().Get("ETag")); got.Code != http.StatusInternalServerError {
		t.Fatalf("warm result bypassed configuration: %d", got.Code)
	}
	f.server.ConfigProvider = nil
	if got := f.read("/v1/tasks", ""); got.Code != http.StatusOK {
		t.Fatalf("configuration error cached: %d", got.Code)
	}
}

func TestMemoPublicMCPWorkspaceAndPermissionIsolation(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	membership := s.Memberships.(*membershipFixture)
	membership.roles["reader"]["private"] = core.WorkspaceRoleViewer
	s.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
		workspace, _ := store.WorkspaceFromContext(ctx)
		return &config.Config{Workspace: workspace}, nil
	}
	if err := s.Store.CreateTask(ctx, core.Task{ID: "memo-demo", Workspace: "demo", Title: "Demo only", State: core.TaskQueued, NextStage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	counted := &activityConditionalStore{Store: s.Store, calls: map[string]int{}}
	s.Store = memoScopedTasks{Store: counted}
	now := time.Now().UTC()
	s.readMemo = &readMemo{clock: func() time.Time { return now }}
	cold, failure := mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure != "" {
		t.Fatal(failure)
	}
	private, failure := mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "private"})
	if failure != "" || len(private.Items) != 0 || len(cold.Items) == 0 {
		t.Fatalf("workspace leak: demo=%v private=%v error=%s", cold.Items, private.Items, failure)
	}
	delete(membership.roles["reader"], "demo")
	_, failure = mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure == "" {
		t.Fatal("warm inbox survived membership revocation")
	}
}

// TestMemoMCPInboxMissMakesNoDiscardedRead proves an MCP list_operator_inbox
// miss makes no validation read it would discard, and sends exactly the
// statements the uncached inbox sends to the same caller (DEC-140(3)).
func TestMemoMCPInboxMissMakesNoDiscardedRead(t *testing.T) {
	s, ctx := newMCPReadFixture(t)
	s.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
		workspace, _ := store.WorkspaceFromContext(ctx)
		return &config.Config{Workspace: workspace}, nil
	}
	for _, task := range []core.Task{
		{ID: "inbox-queued", Workspace: "demo", Title: "Queued", State: core.TaskQueued, NextStage: core.StageImplement},
		{ID: "inbox-gate", Workspace: "demo", Title: "Gate", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, MergeApproval: true, ReviewedHeadSHA: "head", NextStage: core.StageReview},
	} {
		if err := s.Store.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	counted := &activityConditionalStore{Store: s.Store, calls: map[string]int{}}
	s.Store = memoScopedTasks{Store: counted}
	now := time.Now().UTC()
	s.readMemo = &readMemo{clock: func() time.Time { return now }}
	memoized, failure := mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure != "" {
		t.Fatal(failure)
	}
	if got := counted.calls["ReadActivityValidation"]; got != 0 {
		t.Fatalf("the inbox miss made %d validation reads it discards", got)
	}
	if len(memoized.Items) == 0 {
		t.Fatal("the inbox listed nothing")
	}
	s.readMemo = nil
	uncached, failure := mcpReadCall(t, s, "reader", "list_operator_inbox", map[string]any{"workspace_id": "demo"})
	if failure != "" {
		t.Fatal(failure)
	}
	want, _ := json.Marshal(uncached.Items)
	got, _ := json.Marshal(memoized.Items)
	if string(got) != string(want) {
		t.Fatalf("memo miss items=%s\nuncached items=%s", got, want)
	}
}

// TestMemoPublicSlowBuildProjectsPastADeadline proves a memo build that
// takes longer than the memo lifetime answers the projection a fresh read
// gives when a stated deadline due after the lifetime passed during the
// build, and that a build crossing only the lifetime reads once and answers
// the projection at its start with no ETag (component-next-action invariant
// 58; req-operator-next-action AC-1.7).
func TestMemoPublicSlowBuildProjectsPastADeadline(t *testing.T) {
	for _, path := range []string{"/v1/tasks", "/v1/next-actions"} {
		for _, tc := range []struct {
			name      string
			deadline  time.Duration
			wantReads int
		}{
			{"deadline passes during the build", 6 * time.Second, 2},
			{"only the lifetime passes", time.Hour, 1},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				f := newMemoRouteFixture(t)
				f.task("memo-slow", core.TaskRunning)
				f.order(core.WorkOrder{
					ID: "memo-slow-order", TaskID: "memo-slow", JobID: "memo-slow-order", Stage: core.StageImplement, State: core.WorkOrderQueued,
					QueueEnteredAt: f.now.Add(-time.Hour), QueueDeadline: f.now.Add(48 * time.Hour),
					ExecutionStartedAt: f.now.Add(-time.Hour), ExecutionDeadline: f.now.Add(tc.deadline),
				})
				before := f.read(path, "")
				f.server.readMemo = &readMemo{clock: func() time.Time { return f.now }}
				f.counted.reset()
				// The first build's input read takes seven seconds.
				f.counted.duringInputs = func() { f.now = f.now.Add(7 * time.Second) }
				got := f.read(path, "")
				f.counted.duringInputs = nil
				// A build that outlived its horizon carries no tag. A
				// rebuild that fit its horizon may carry its own.
				if got.Code != http.StatusOK || (tc.wantReads == 1 && got.Header().Get("ETag") != "") {
					t.Fatalf("slow build status=%d tag=%q", got.Code, got.Header().Get("ETag"))
				}
				if reads := f.counted.calls["ReadActivityInputs"]; reads != tc.wantReads {
					t.Fatalf("slow build input reads=%d, want %d", reads, tc.wantReads)
				}
				// A fresh read at the answer instant is the projection a
				// rebuilt answer must equal.
				f.server.readMemo = &readMemo{clock: func() time.Time { return f.now }}
				fresh := f.read(path, "")
				if tc.wantReads == 2 {
					if got.Body.String() != fresh.Body.String() {
						t.Fatalf("slow build answered a projection from before the deadline:\n got %s\nwant %s", got.Body.String(), fresh.Body.String())
					}
					if before.Body.String() == fresh.Body.String() {
						t.Fatalf("the passed deadline did not change the projection: %s", fresh.Body.String())
					}
				} else if got.Body.String() != before.Body.String() {
					t.Fatalf("lifetime-only build changed the projection:\n got %s\nwant %s", got.Body.String(), before.Body.String())
				}
			})
		}
	}
}

// TestMemoPublicSlowInboxWithPendingProposalReadsOnce proves a pending
// document proposal's age second, a display instant, does not make a slow
// GET /v1/next-actions read its inputs again: the inbox build that outlives
// it answers once, untagged, with one input read for the whole request.
func TestMemoPublicSlowInboxWithPendingProposalReadsOnce(t *testing.T) {
	f := newMemoRouteFixture(t)
	f.task("memo-proposal-task", core.TaskRunning)
	if _, _, err := f.base.CreateRequirement(f.ctx, core.Requirement{ID: "req-memo-proposal", Title: "Memo proposal"}, core.RequirementVersion{Content: "# Memo proposal", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Pending."}}}); err != nil {
		t.Fatal(err)
	}
	f.counted.reset()
	// The inbox's input read takes two seconds, past the proposal's next
	// age second and inside the memo lifetime.
	f.counted.duringInputs = func() { f.now = f.now.Add(2 * time.Second) }
	got := f.read("/v1/next-actions", "")
	if got.Code != http.StatusOK || got.Header().Get("ETag") != "" {
		t.Fatalf("slow inbox status=%d tag=%q body=%s", got.Code, got.Header().Get("ETag"), got.Body.String())
	}
	if !strings.Contains(got.Body.String(), "req-memo-proposal") {
		t.Fatalf("inbox lost the pending proposal: %s", got.Body.String())
	}
	if reads := f.counted.calls["ReadActivityInputs"]; reads != 1 {
		t.Fatalf("one request read its inputs %d times, want 1", reads)
	}
}

// A later entry changes a report's count and times with no event and no
// notice. A conditional Inbox read with the earlier tag answers 304 until
// the memo's lifetime ends, and then 200 with the new values
// (feature-production-feedback-listening FR-5; component-next-action
// invariant 58).
func TestFeedbackInboxConditionalReadSeesALaterEntry(t *testing.T) {
	h := newReportRoutesHarness(t)
	h.server.UseReadMemoClock(h.clock)
	report := h.storeReport("conditional", nil)
	// Linking has ended, so no read asks the linking job, whose end would
	// raise a notice.
	h.endLinking(report, nil, nil)
	first := h.inbox("")
	if first.etag == "" || !slices.Equal(first.reportIDs(t), []string{report.ID}) {
		t.Fatalf("first read etag=%q items=%v", first.etag, first.items)
	}
	conditional := func() *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/next-actions", nil)
		request.Header.Set("Authorization", "Bearer "+h.manager())
		request.Header.Set("X-Workspace-ID", "alpha")
		request.Header.Set("If-None-Match", first.etag)
		return h.serve(request)
	}
	if response := conditional(); response.Code != http.StatusNotModified {
		t.Fatalf("unchanged conditional read status=%d", response.Code)
	}
	// The later entry raises no notice: the memo still answers 304 inside
	// its lifetime.
	h.server.Changes = nil
	h.advance(time.Millisecond)
	if err := h.st.RecordFeedbackReports(h.ctx, store.FeedbackReportBatch{EnvironmentID: h.environment.ID, KeyID: h.key.ID, ArrivedAt: h.clock(), Entries: []store.FeedbackReportEntry{{Kind: store.FeedbackReportError, Key: "conditional", Summary: "Again", Count: 41}}}); err != nil {
		t.Fatal(err)
	}
	if response := conditional(); response.Code != http.StatusNotModified {
		t.Fatalf("conditional read inside the memo lifetime status=%d", response.Code)
	}
	h.advance(readMemoLifetime)
	response := conditional()
	if response.Code != http.StatusOK || response.Header().Get("ETag") == first.etag {
		t.Fatalf("conditional read after the lifetime status=%d etag=%q (was %q)", response.Code, response.Header().Get("ETag"), first.etag)
	}
	var body struct {
		Items []struct {
			Report servedFeedbackReport `json:"report"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
		t.Fatalf("decode %s: %v", response.Body, err)
	}
	if got := body.Items[0].Report; got.Count != 42 || !got.LastSeen.After(report.LastSeen) {
		t.Fatalf("served after the later entry count=%d last_seen=%s, want 42 and later than %s", got.Count, got.LastSeen, report.LastSeen)
	}
}
