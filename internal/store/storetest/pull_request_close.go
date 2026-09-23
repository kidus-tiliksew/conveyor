package storetest

import (
	"context"

	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runPullRequestClose(t *testing.T, factory Factory) {
	t.Run("attach_contends_with_branch_close", func(t *testing.T) {
		x := factory.fresh(t, []config.Repo{{Name: "local-config", GitHub: "org/different-slug", URL: "https://github.com/org/different-slug", Base: "main"}})
		st, ctx := x.Backend, x.Context
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "retired", Repo: "local-config", BaseBranch: "main", Branch: "feature/shared", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
		requireOK(t, st.CreateTask(ctx, old))
		_, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "replace"})
		requireOK(t, err)
		other := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "new holder", Repo: old.Repo, BaseBranch: "main", Branch: "feature/other", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
		requireOK(t, st.CreateTask(ctx, other))
		locked, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- st.WithTaskSideEffectLock(ctx, old.ID, func(ctx context.Context) error {
				return st.WithTaskSideEffectLock(ctx, store.BranchCloseLockKey(old.Repo, old.Branch), func(ctx context.Context) error {
					close(locked)
					<-release
					return nil
				})
			})
		}()
		select {
		case <-locked:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("nested branch lock deadlocked")
		}
		attached := make(chan error, 1)
		go func() { _, err := st.AttachTaskBranch(ctx, other.ID, old.Branch); attached <- err }()
		select {
		case err := <-attached:
			close(release)
			t.Fatalf("attach bypassed close lock: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		// An attach waiting for the branch key cannot monopolize memory's global
		// mutex. Ordinary reads must continue during the external side effect.
		read := make(chan error, 1)
		go func() { _, err := st.GetTask(ctx, old.ID); read <- err }()
		select {
		case err := <-read:
			requireOK(t, err)
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("waiting attach blocked task reads")
		}
		close(release)
		select {
		case err := <-done:
			requireOK(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("close lock did not release")
		}
		select {
		case err := <-attached:
			requireOK(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("attach did not resume")
		}
		current, err := st.GetTask(ctx, other.ID)
		requireOK(t, err)
		if current.Branch != old.Branch {
			t.Fatalf("assignment=%s", current.Branch)
		}
		t.Logf("attach/close shared-key contention executed; durable=%v config=%s forge=%s", st.IsDurable(), old.Repo, x.Config.Repos[0].GitHub)
	})
	t.Run("coherent_recorded_identity", func(t *testing.T) {
		x := factory.fresh(t, nil)
		st, ctx := x.Backend, x.Context
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "recorded", Repo: "conveyor", BaseBranch: "main", Branch: "feature/recorded", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
		requireOK(t, st.CreateTask(ctx, old))
		requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: old.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 42, "url": "https://github.com/acme/app/pull/42"})}))
		result, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "replace"})
		requireOK(t, err)
		successor, err := st.GetTask(ctx, result.Successor.ID)
		requireOK(t, err)
		p := core.PullRequestClose{WorkspaceID: x.Workspace, TaskID: old.ID, Repository: "acme/app", Branch: old.Branch, SuccessorID: successor.ID, SuccessorBranch: successor.Branch, Reason: "replace", RestartingOperatorID: "operator", ForgeAuthorClass: core.ForgeAuthorWorkspace, State: "queued", Number: 42, URL: "https://github.com/acme/app/pull/42"}
		bad := p
		bad.Number = 0
		if err := st.QueuePullRequestClose(ctx, bad); err == nil {
			t.Fatal("URL without number accepted")
		}
		requireOK(t, st.QueuePullRequestClose(ctx, p))
		got, ok, err := st.GetPullRequestClose(ctx, old.ID)
		requireOK(t, err)
		if !ok || got.Number != 42 || got.URL != p.URL {
			t.Fatalf("recorded intent=%+v", got)
		}
	})

	for _, outcome := range []string{"closed", "failed", "skipped"} {
		t.Run(outcome, func(t *testing.T) {
			x := factory.fresh(t, nil)
			st := x.Backend
			ctx := store.WithActor(x.Context, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
			old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Title: "Close PR", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + core.NewTaskID(), State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
			if err := st.CreateTask(ctx, old); err != nil {
				t.Fatal(err)
			}
			result, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "revised scope"})
			if err != nil {
				t.Fatal(err)
			}
			successor, err := st.GetTask(ctx, result.Successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			p := core.PullRequestClose{WorkspaceID: x.Workspace, TaskID: old.ID, Repository: "acme/app", Branch: old.Branch, SuccessorID: successor.ID, SuccessorBranch: successor.Branch, Reason: "revised scope", RestartingOperatorID: "restart-operator", ForgeAuthorClass: core.ForgeAuthorWorkspace, State: "queued"}
			intent := p
			invalidActor := store.WithActor(ctx, store.Actor{ID: "bad\x00actor", Role: core.ActorHuman})
			if err = st.QueuePullRequestClose(invalidActor, p); err == nil {
				t.Fatal("invalid audit accepted")
			}
			if _, exists, err := st.GetPullRequestClose(ctx, p.TaskID); err != nil || exists {
				t.Fatal("failed intent persisted")
			}

			numberOnly := intent
			numberOnly.Number = 7
			if err = st.QueuePullRequestClose(ctx, numberOnly); err == nil {
				t.Fatal("partial identity accepted")
			}
			for range 2 {
				if err = st.QueuePullRequestClose(ctx, intent); err != nil {
					t.Fatal(err)
				}
			}
			got, ok, err := st.GetPullRequestClose(ctx, old.ID)
			if err != nil || !ok || !reflect.DeepEqual(p, got) {
				t.Fatalf("intent=%+v exists=%v err=%v", got, ok, err)
			}
			foreign := store.WithWorkspace(ctx, "unrelated-workspace")
			if _, ok, err = st.GetPullRequestClose(foreign, old.ID); err != nil || ok {
				t.Fatalf("cross-workspace read exists=%v err=%v", ok, err)
			}
			if st.IsDurable() {
				job, err := logqueue.Load(ctx, st.Log(), x.Workspace, logqueue.StreamFor(queue.PullRequestCloseArgs{}.Kind(), old.ID))
				if err != nil || !job.Active() || job.MaxAttempts != core.PullRequestCloseMaxAttempts {
					t.Fatalf("durable job=%+v err=%v", job, err)
				}
			}
			p.State = "retrying"
			p.Attempts = 1
			if err = st.UpdatePullRequestClose(invalidActor, p); err == nil {
				t.Fatal("invalid progress audit accepted")
			}
			if unchanged, _, err := st.GetPullRequestClose(ctx, p.TaskID); err != nil || unchanged.State != "queued" {
				t.Fatal("failed update changed projection")
			}
			if err = st.UpdatePullRequestClose(ctx, p); err != nil {
				t.Fatal(err)
			}
			wrong := p
			wrong.SuccessorID = "different"
			if err = st.UpdatePullRequestClose(ctx, wrong); err == nil {
				t.Fatal("changed immutable successor")
			}
			wrong = p
			wrong.ForgeAuthorClass = core.ForgeAuthorClass("executing_user")
			if err = st.UpdatePullRequestClose(ctx, wrong); err == nil {
				t.Fatal("changed workspace identity")
			}
			if outcome == "failed" {
				for p.Attempts < core.PullRequestCloseMaxAttempts {
					p.Attempts++
					p.ForgeErrorCategory = "forge_request"
					if err = st.UpdatePullRequestClose(ctx, p); err != nil {
						t.Fatal(err)
					}
				}
			}
			p.Number = 42
			p.URL = "https://github.com/acme/app/pull/42"
			p.State = outcome
			if err = st.UpdatePullRequestClose(ctx, p); err != nil {
				t.Fatal(err)
			}
			// Repeated hooks cannot reopen terminal publications or emit completion twice.
			if err = st.QueuePullRequestClose(ctx, intent); err != nil {
				t.Fatal(err)
			}
			if err = st.UpdatePullRequestClose(ctx, p); err == nil {
				t.Fatal("terminal transition accepted")
			}
			got, ok, err = st.GetPullRequestClose(ctx, old.ID)
			if err != nil || !ok || !reflect.DeepEqual(p, got) {
				t.Fatalf("result=%+v exists=%v err=%v", got, ok, err)
			}
			retired, err := st.GetTask(ctx, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			if retired.PullRequestClose == nil || retired.PullRequestCloseState != outcome || !reflect.DeepEqual(*retired.PullRequestClose, p) {
				t.Fatalf("task projection=%+v", retired.PullRequestClose)
			}
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, task := range tasks {
				if task.ID == old.ID {
					found = task.PullRequestCloseState == outcome && task.PullRequestClose != nil
				}
			}
			if !found {
				t.Fatal("list omitted close projection")
			}
			after, err := st.GetTask(ctx, successor.ID)
			if err != nil || !reflect.DeepEqual(successor, after) {
				t.Fatalf("successor changed: %v", err)
			}
			events, err := st.ListEvents(ctx, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			queued, terminal := 0, 0
			for _, event := range events {
				if event.Kind == "pull_request.close_queued" {
					queued++
				}
				if event.Kind == store.PullRequestCloseEvent(p).Kind {
					terminal++
					var payload core.PullRequestClose
					if json.Unmarshal(event.Payload, &payload) != nil || !reflect.DeepEqual(payload, p) {
						t.Fatal("terminal event differs from projection")
					}
				}
			}
			if queued != 1 || terminal != 1 {
				t.Fatalf("queued=%d terminal=%d", queued, terminal)
			}
		})
	}
}
