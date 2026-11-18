package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// statementRoutes are the four conditional statement reads of
// component-next-action invariant 58, for one task ID.
func statementRoutes(taskID string) []string {
	return []string{
		"/v1/tasks/" + taskID + "/next-action?workspace_id=demo",
		"/v1/tasks/" + taskID + "/observation?workspace_id=demo",
		"/v1/tasks",
		"/v1/next-actions",
	}
}

// gateFixture is one task awaiting its merge_approval gate, whose merge
// readiness the task detail read fills through the forge hook, on a memo and
// a readiness cache that share the fixture clock.
func gateFixture(t *testing.T, taskID string) (*activityConditionalFixture, *atomic.Value) {
	t.Helper()
	f := newMemoRouteFixture(t)
	f.server.mergeReadiness.clock = func() time.Time { return f.now }
	// The memo hears the readiness notices on the server's hub.
	f.server.Changes = changes.NewHub()
	f.server.readMemo.subscribe(f.server.Changes)
	task := core.Task{ID: taskID, Workspace: "demo", Title: "Gate " + taskID, Repo: "conveyor", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, MergeApproval: true, ReviewedHeadSHA: "reviewed-head", NextStage: core.StageReview, CreatedAt: f.now.Add(-time.Hour)}
	if err := f.base.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := f.base.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: "task.created", At: task.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	state := &atomic.Value{}
	state.Store("MERGEABLE")
	f.server.OnMergeReadiness = func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
		return dispatch.MergeReadiness{State: state.Load().(string), HeadSHA: "reviewed-head", Number: 7}, nil
	}
	return f, state
}

func (f *activityConditionalFixture) detail(taskID string) {
	f.t.Helper()
	if response := f.read("/v1/tasks/"+taskID+"/activity?workspace_id=demo", ""); response.Code != http.StatusOK {
		f.t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
}

func expectStatus(t *testing.T, what string, response interface {
	Result() *http.Response
}, code int) string {
	t.Helper()
	result := response.Result()
	if result.StatusCode != code {
		t.Fatalf("%s: status=%d, want %d", what, result.StatusCode, code)
	}
	return result.Header.Get("ETag")
}

// TestStatementReadsCoverMergeReadiness proves each conditional statement
// read answers 304 for an unchanged read, and 200 with a new tag after the
// merge readiness it read changes and after the readiness value it read
// expires, even when nothing else changed (component-next-action invariant
// 58; DEC-140(3)).
func TestStatementReadsCoverMergeReadiness(t *testing.T) {
	for _, path := range statementRoutes("gate-readiness") {
		t.Run(path, func(t *testing.T) {
			f, _ := gateFixture(t, "gate-readiness")
			first := expectStatus(t, "cold read", f.read(path, ""), http.StatusOK)
			if first == "" {
				t.Fatal("the cold read states no ETag")
			}
			if got := expectStatus(t, "unchanged read", f.read(path, first), http.StatusNotModified); got != first {
				t.Fatalf("304 tag=%q, want %q", got, first)
			}
			// The task detail read fills the readiness cache: the value the
			// statement reads changes from none to MERGEABLE.
			f.detail("gate-readiness")
			changed := expectStatus(t, "read after a readiness change", f.read(path, first), http.StatusOK)
			if changed == "" || changed == first {
				t.Fatalf("readiness change kept tag %q", changed)
			}
			// Seven seconds on, the readiness value expires three seconds
			// before the memo would: the tag must not outlive the value.
			f.now = f.now.Add(7 * time.Second)
			held := expectStatus(t, "read before the readiness expiry", f.read(path, ""), http.StatusOK)
			f.now = f.now.Add(2 * time.Second)
			expectStatus(t, "unchanged read before the readiness expiry", f.read(path, held), http.StatusNotModified)
			f.now = f.now.Add(time.Second)
			expired := expectStatus(t, "read after the readiness expiry", f.read(path, held), http.StatusOK)
			if expired == "" || expired == held {
				t.Fatalf("expired readiness kept tag %q", expired)
			}
		})
	}
}

// TestStatementReadsCoverLeaseAndDeadline proves each conditional statement
// read answers 200 with a new tag once a review lease horizon passes and once
// a stated deadline passes, and 304 before either (component-next-action
// invariant 58).
func TestStatementReadsCoverLeaseAndDeadline(t *testing.T) {
	horizons := map[string]func(f *activityConditionalFixture, taskID string){
		"review lease": func(f *activityConditionalFixture, taskID string) {
			order := core.WorkOrder{ID: taskID + "-review", TaskID: taskID, JobID: taskID + "-review", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: f.now.Add(-time.Hour), QueueDeadline: f.now.Add(time.Hour), ExecutionStartedAt: f.now.Add(-time.Minute)}
			f.order(order)
			if err := f.base.AppendEvent(f.ctx, core.Event{TaskID: taskID, JobID: order.JobID, Kind: "work_order.claimed", At: f.now.Add(-time.Minute), Payload: core.JSONPayload(map[string]any{"id": order.ID, "lease_expires_at": f.now.Add(3 * time.Second)})}); err != nil {
				f.t.Fatal(err)
			}
		},
		"deadline": func(f *activityConditionalFixture, taskID string) {
			f.order(core.WorkOrder{ID: taskID + "-implement", TaskID: taskID, JobID: taskID + "-implement", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: f.now.Add(-time.Hour), QueueDeadline: f.now.Add(time.Hour), ExecutionStartedAt: f.now.Add(-time.Hour), ExecutionDeadline: f.now.Add(3 * time.Second)})
		},
	}
	for name, arrange := range horizons {
		for _, path := range statementRoutes("horizon-task") {
			t.Run(name+" "+path, func(t *testing.T) {
				f := newMemoRouteFixture(t)
				state := core.TaskQueued
				if name == "review lease" {
					state = core.TaskRunning
				}
				f.task("horizon-task", state)
				arrange(f, "horizon-task")
				cold := f.read(path, "")
				tag := expectStatus(t, "cold read", cold, http.StatusOK)
				f.now = f.now.Add(2 * time.Second)
				expectStatus(t, "read before the horizon", f.read(path, tag), http.StatusNotModified)
				f.now = f.now.Add(2 * time.Second)
				after := f.read(path, tag)
				if got := expectStatus(t, "read after the horizon", after, http.StatusOK); got == tag {
					t.Fatalf("the tag outlived the %s", name)
				}
			})
		}
	}
}

// movingStore performs one real store write before each marker read while
// armed, so the validation state moves under a statement read's projection.
type movingStore struct {
	store.Store
	during func()
}

func (s *movingStore) ReadActivityInputs(ctx context.Context, taskIDs []string, now time.Time) (store.ActivityInputs, error) {
	if s.during != nil {
		s.during()
	}
	return s.Store.ReadActivityInputs(ctx, taskIDs, now)
}

// TestStatementReadsSendNoETagWhenInputsMove proves a statement read whose
// validation state changes under every projection answers its body with no
// ETag: no tag certifies a body its inputs moved under (component-next-action
// invariant 58).
func TestStatementReadsSendNoETagWhenInputsMove(t *testing.T) {
	for _, path := range statementRoutes("moving-task") {
		t.Run(path, func(t *testing.T) {
			f := newMemoRouteFixture(t)
			f.task("moving-task", core.TaskQueued)
			writes := 0
			f.server.Store = &movingStore{Store: f.counted, during: func() {
				writes++
				if err := f.base.AppendEvent(f.ctx, core.Event{TaskID: "moving-task", Kind: fmt.Sprintf("moving.write.%d", writes)}); err != nil {
					t.Fatal(err)
				}
			}}
			response := f.read(path, "")
			if response.Code != http.StatusOK || response.Body.Len() == 0 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tag := response.Header().Get("ETag"); tag != "" {
				t.Fatalf("a body read under moving inputs carries ETag %q", tag)
			}
			if writes == 0 {
				t.Fatal("the read made no projection")
			}
		})
	}
}

// TestPerTaskStatementReadsRevalidateAfterProjection proves both per-task
// routes refuse a 304 when a covered input changes after the pre-read
// validation and before the answer: the read projects again and answers 200
// with the new body and a new tag (component-next-action invariant 58).
func TestPerTaskStatementReadsRevalidateAfterProjection(t *testing.T) {
	for _, path := range statementRoutes("race-task")[:2] {
		t.Run(path, func(t *testing.T) {
			f := newMemoRouteFixture(t)
			f.task("race-task", core.TaskQueued)
			tag := expectStatus(t, "cold read", f.read(path, ""), http.StatusOK)
			raced := false
			f.server.Store = &movingStore{Store: f.counted, during: func() {
				if raced {
					return
				}
				raced = true
				f.order(core.WorkOrder{ID: "race-task-implement-1", TaskID: "race-task", JobID: "race-job", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true})
			}}
			response := f.read(path, tag)
			if response.Code != http.StatusOK {
				t.Fatalf("a write after the pre-read check answered %d, want 200", response.Code)
			}
			if got := response.Header().Get("ETag"); got == "" || got == tag {
				t.Fatalf("raced read tag=%q, want a new tag", got)
			}
			if !strings.Contains(response.Body.String(), `"queued"`) && !strings.Contains(response.Body.String(), "race-task-implement-1") {
				t.Fatalf("raced read body=%s lacks the new order", response.Body.String())
			}
			if again := f.read(path, response.Header().Get("ETag")); again.Code != http.StatusNotModified {
				t.Fatalf("settled read status=%d, want 304", again.Code)
			}
		})
	}
}

// TestReadinessNoticesDropTheMemoLocally proves a merge-readiness change,
// forget, and expiry each publish an in-process workspace notice that drops
// a warm memo entry within the next request, and that none of them writes the
// store, so nothing is persisted or sent to a peer (DEC-140(3)).
func TestReadinessNoticesDropTheMemoLocally(t *testing.T) {
	f, state := gateFixture(t, "gate-notice")
	notices := f.server.Changes.Subscribe("demo", changes.Workspace)
	defer notices.Close()
	expectNotice := func(what string) {
		t.Helper()
		select {
		case n := <-notices.C():
			if n.Workspace != "demo" || n.Kind != changes.Workspace {
				t.Fatalf("%s: notice=%+v", what, n)
			}
		default:
			t.Fatalf("%s published no workspace notice", what)
		}
	}
	expectQuietHub := func(what string) {
		t.Helper()
		select {
		case n := <-notices.C():
			t.Fatalf("%s published %+v", what, n)
		default:
		}
	}
	warm := func(what string) {
		t.Helper()
		f.read("/v1/next-actions", "")
		f.counted.reset()
		f.read("/v1/next-actions", "")
		if f.counted.calls["ReadActivityValidation"] != 0 {
			t.Fatalf("%s: the memo was not warm: %v", what, f.counted.calls)
		}
	}
	rebuilt := func(what string) {
		t.Helper()
		f.counted.reset()
		f.read("/v1/next-actions", "")
		if f.counted.calls["ReadActivityValidation"] == 0 {
			t.Fatalf("%s: the next read answered from the memo", what)
		}
	}
	validation := func() store.ActivityValidation {
		t.Helper()
		v, err := f.base.ReadActivityValidation(f.ctx, f.now)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	stored := validation()

	warm("first value")
	f.detail("gate-notice")
	expectNotice("a new readiness value")
	rebuilt("a new readiness value")

	// The same live value read again announces nothing.
	f.server.mergeReadiness.forget(store.WithWorkspace(f.ctx, "demo"), "gate-notice")
	expectNotice("forget of a live value")
	f.detail("gate-notice")
	expectNotice("the value read again after forget")
	f.now = f.now.Add(time.Second)
	f.detail("gate-notice")
	expectQuietHub("an unchanged live value")

	warm("changed value")
	state.Store("CONFLICTING")
	f.now = f.now.Add(mergeReadinessReuse)
	f.detail("gate-notice")
	expectNotice("a changed readiness value")
	rebuilt("a changed readiness value")

	warm("expiry")
	f.now = f.now.Add(mergeReadinessReuse)
	// No forge read follows: the next statement read finds the value
	// expired, drops it, and announces it.
	f.read("/v1/next-actions", "")
	expectNotice("an expired readiness value")

	if after := validation(); after != stored {
		t.Fatal("a readiness notice wrote the store")
	}
}

// TestRequestConfigurationIsKeyedByWorkspace proves a request context that
// changes workspace reads that workspace's configuration and trusted authors,
// not the configuration it loaded for another workspace.
func TestRequestConfigurationIsKeyedByWorkspace(t *testing.T) {
	server := NewServer(store.NewMemory())
	reads := map[string]int{}
	server.ConfigProvider = func(ctx context.Context) (*config.Config, error) {
		workspace, _ := store.WorkspaceFromContext(ctx)
		reads[workspace]++
		return &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", TrustedAuthors: []string{"author-" + workspace}}}}, nil
	}
	ctx, alpha, err := server.requestConfiguration(store.WithWorkspace(t.Context(), "alpha"))
	if err != nil || alpha.Workspace != "alpha" {
		t.Fatalf("alpha=%+v err=%v", alpha, err)
	}
	if _, again, _ := server.requestConfiguration(ctx); again != alpha || reads["alpha"] != 1 {
		t.Fatalf("the request read alpha again: reads=%v", reads)
	}
	switched := store.WithWorkspace(ctx, "beta")
	_, beta, err := server.requestConfiguration(switched)
	if err != nil || beta.Workspace != "beta" || reads["beta"] != 1 {
		t.Fatalf("beta=%+v reads=%v err=%v", beta, reads, err)
	}
	opts, err := server.nextActionOptionsFor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for workspace, want := range map[string]string{"alpha": "author-alpha", "beta": "author-beta"} {
		authors, err := opts.TrustedAuthors(ctx, core.Task{ID: "t", Workspace: workspace, Repo: "conveyor"})
		if err != nil || len(authors) != 1 || authors[0] != want {
			t.Fatalf("%s trusted authors=%v err=%v, want %s", workspace, authors, err, want)
		}
	}
}
