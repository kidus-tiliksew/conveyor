package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestAssignmentResolveTaskWiresRecordedOwnerLookup(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	resolver := assignmentResolveTask(st, "app", "org/app")
	source := monitor.GitHubSource{NewTaskResolver: resolver}
	if source.NewTaskResolver == nil {
		t.Fatal("daemon GitHubSource ResolveTask is nil")
	}
	merged := core.Task{ID: "A", Workspace: "demo", Repo: "app", Branch: "feature/x", State: core.TaskMerged, CreatedAt: time.Now()}
	open := core.Task{ID: "B", Workspace: "demo", Repo: "app", Branch: "feature/x", State: core.TaskRunning, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctx, open); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: "A", Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 4, "repository": "org/app"})}); err != nil {
		t.Fatal(err)
	}
	resolve, err := source.NewTaskResolver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, ok, err := resolve(ctx, "feature/x", 4)
	if err != nil || !ok || id != "A" {
		t.Fatalf("recorded owner A lost: id=%s ok=%t err=%v", id, ok, err)
	}
}

type countingMonitorStore struct {
	store.Store
	taskReads, eventReads int
	eventIDs              []string
	taskErr, eventErr     error
}

func (s *countingMonitorStore) ListTasks(context.Context) ([]core.Task, error) {
	panic("monitor must filter tasks before loading events")
}
func (s *countingMonitorStore) ListEvents(context.Context, string) ([]core.Event, error) {
	panic("monitor must batch opened events")
}
func (s *countingMonitorStore) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	s.taskReads++
	if s.taskErr != nil {
		return nil, s.taskErr
	}
	return s.Store.ListTasksFiltered(ctx, filter)
}
func (s *countingMonitorStore) ListMonitorPullRequestEventsForTasks(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	s.eventReads++
	s.eventIDs = append([]string(nil), ids...)
	if s.eventErr != nil {
		return nil, s.eventErr
	}
	return s.Store.ListMonitorPullRequestEventsForTasks(ctx, ids)
}

func TestAssignmentResolverPollReadCount(t *testing.T) {
	for _, n := range []int{1, 12} {
		for _, unrelated := range []int{0, 50} {
			t.Run(fmt.Sprintf("pulls=%d/unrelated=%d", n, unrelated), func(t *testing.T) {
				ctx := store.WithWorkspace(t.Context(), "demo")
				st := &countingMonitorStore{Store: store.NewMemory()}
				for i := 0; i <= unrelated; i++ {
					repo := "other"
					if i == 0 {
						repo = "app"
					}
					task := core.Task{ID: fmt.Sprintf("task-%d", i), Workspace: "demo", Repo: repo, Branch: fmt.Sprintf("branch-%d", i), State: core.TaskRunning, CreatedAt: time.Now()}
					if err := st.CreateTask(ctx, task); err != nil {
						t.Fatal(err)
					}
				}
				calls, matches := 0, 0
				factory := assignmentResolveTask(st, "app", "org/app")
				source := monitor.GitHubSource{Repository: "app", GitHubSlug: "org/app",
					NewTaskResolver: func(ctx context.Context) (monitor.TaskResolver, error) {
						resolve, err := factory(ctx)
						if err != nil {
							return nil, err
						}
						return func(ctx context.Context, head string, number int) (string, bool, error) {
							calls++
							id, ok, err := resolve(ctx, head, number)
							if ok {
								matches++
							}
							return id, ok, err
						}, nil
					},
					Run: func(_ context.Context, args ...string) ([]byte, error) {
						if strings.HasSuffix(args[3], "/commits") {
							var commits []map[string]any
							for i := 1; i <= n; i++ {
								commits = append(commits, map[string]any{"sha": fmt.Sprint(i)})
							}
							return json.Marshal(commits)
						}
						var number int
						if _, err := fmt.Sscanf(args[3], "repos/org/app/commits/%d/pulls", &number); err != nil {
							return nil, err
						}
						return json.Marshal([]map[string]any{{"number": number, "merged_at": "2026-09-23T00:00:00Z", "head": map[string]string{"ref": "custom-unmatched"}}})
					},
				}
				for poll := 1; poll <= 2; poll++ {
					got, err := source.Observations(ctx, time.Time{})
					if err != nil || len(got) != n {
						t.Fatalf("observations=%v err=%v", got, err)
					}
					if st.taskReads != poll || st.eventReads != poll || !reflect.DeepEqual(st.eventIDs, []string{"task-0"}) {
						t.Fatalf("reads tasks=%d events=%d candidates=%v", st.taskReads, st.eventReads, st.eventIDs)
					}
					if calls != poll*n {
						t.Fatalf("resolved %d pulls, want %d", calls, poll*n)
					}
					if poll == 1 {
						if matches != 0 {
							t.Fatal("matched unrecorded PR")
						}
						// A PR recorded between polls must be visible in the next snapshot.
						if err := st.AppendEvent(ctx, core.Event{TaskID: "task-0", Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 1})}); err != nil {
							t.Fatal(err)
						}
					} else if matches != 1 {
						t.Fatalf("next poll missed new PR: matches=%d", matches)
					}
				}
			})
		}
	}
}

func TestAssignmentResolverReadErrorsAbortPoll(t *testing.T) {
	sentinel := errors.New("store unavailable")
	for _, stage := range []string{"tasks", "events"} {
		t.Run(stage, func(t *testing.T) {
			st := &countingMonitorStore{Store: store.NewMemory()}
			if stage == "tasks" {
				st.taskErr = sentinel
			} else {
				st.eventErr = sentinel
			}
			source := monitor.GitHubSource{Repository: "app", GitHubSlug: "org/app", NewTaskResolver: assignmentResolveTask(st, "app", "org/app"),
				Run: func(context.Context, ...string) ([]byte, error) {
					t.Fatal("forge read after failed initialization")
					return nil, nil
				},
			}
			got, err := source.Observations(store.WithWorkspace(t.Context(), "demo"), time.Time{})
			if !errors.Is(err, sentinel) || len(got) != 0 {
				t.Fatalf("observations=%v err=%v", got, err)
			}
			wantEvents := 1
			if stage == "tasks" {
				wantEvents = 0
			}
			if st.taskReads != 1 || st.eventReads != wantEvents {
				t.Fatalf("reads=%d/%d", st.taskReads, st.eventReads)
			}
		})
	}
}

func TestAssignmentResolverRefreshesBranchBetweenPolls(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "task", Workspace: "demo", Repo: "app", Branch: "old-branch", State: core.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	factory := assignmentResolveTask(st, "app", "org/app")
	before, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AttachTaskBranch(ctx, task.ID, "new-branch"); err != nil {
		t.Fatal(err)
	}
	after, err := factory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		resolve monitor.TaskResolver
		branch  string
		want    bool
	}{
		{before, "old-branch", true}, {before, "new-branch", false},
		{after, "old-branch", false}, {after, "new-branch", true},
	} {
		id, ok, err := tc.resolve(ctx, tc.branch, 0)
		if err != nil || ok != tc.want || ok && id != task.ID {
			t.Fatalf("branch=%s id=%s ok=%v err=%v", tc.branch, id, ok, err)
		}
	}
}
