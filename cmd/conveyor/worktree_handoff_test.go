package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// This subprocess executes checkout with the exact environment assembled by
// the production launcher; standalone operator checkout is not the oracle.
func TestCheckpointHandoffChild(t *testing.T) {
	if os.Getenv("CONVEYOR_WRITER_GENERATION") == "" {
		return
	}
	c := newClient()
	taskID := os.Getenv("CONVEYOR_TASK_ID")
	branch, base, repo, repoURL, ok := assignedCheckoutFromEnvironment(taskID)
	if !ok {
		t.Fatal("missing assignment")
	}
	path, err := checkoutWithWriter(t.Context(), c, taskID, branch, base, repo, repoURL, "", os.Getenv("CONVEYOR_WORKTREE_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	action := os.Args[len(os.Args)-1]
	if action == "produce" || action == "produce-linger" {
		if err = os.WriteFile(filepath.Join(path, "README.md"), []byte("tracked implementation\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(path, "new.txt"), []byte("untracked implementation\n"), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		for name, want := range map[string]string{"README.md": "tracked implementation\n", "new.txt": "untracked implementation\n"} {
			data, err := os.ReadFile(filepath.Join(path, name))
			if err != nil || string(data) != want {
				t.Fatalf("lost %s: %q %v", name, data, err)
			}
		}
		if os.Getenv("CONVEYOR_PREVIOUS_WORK_ORDER_ID") == os.Getenv("CONVEYOR_WORK_ORDER_ID") {
			t.Fatal("producer order replaced by successor")
		}
	}
	fmt.Println("checkpoint fixture child executed", action)
	payload, _ := json.Marshal(map[string]string{"session_id": os.Getenv("CONVEYOR_SESSION_ID"), "order_id": os.Getenv("CONVEYOR_WORK_ORDER_ID"), "action": action})
	var result any
	if err = c.workerDoContext(t.Context(), http.MethodPost, "/fixture/finish", payload, &result, c.token); err != nil {
		t.Fatal(err)
	}
	if action == "produce-linger" {
		time.Sleep(time.Second)
	}
}

func TestCheckpointHandoffLauncherPlanRevision(t *testing.T) {
	// pack.Load must precede the git fixture's chdir.
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"run", "worker"} {
		for _, termination := range []string{"exit", "renewal", "teardown", "preserve-failure", "audit-failure", "legacy-audited"} {
			linger := termination == "renewal" || termination == "teardown"
			t.Run(fmt.Sprintf("%s/%s", mode, termination), func(t *testing.T) {
				fixture := newGitFixture(t)
				ctx := store.WithWorkspace(t.Context(), "demo")
				st := store.NewMemory()
				now := time.Now().UTC()
				task := core.Task{ID: "handoff", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-handoff", PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				must(st.CreateTask(ctx, task))
				plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Approach\nPreserve work.\n\n## Files touched\n- README.md\n\n## Ordering\n1. Implement.\n\n## Risks\n- Loss.\n\n## Done criteria\n- Content survives.", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
				must(err)
				must(st.ApproveSpecVersion(ctx, task.ID, plan.Version))
				job := core.Job{ID: "handoff-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
				order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
				_, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order)
				must(err)
				cfg := &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "conveyor", URL: fixture.origin, Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Model: "planner", Execution: config.ExecutionMCP, Timeout: time.Hour}, "implement": {Model: "implementer", Execution: config.ExecutionMCP, Timeout: time.Hour}}}}
				dispatcher := dispatch.New(st, cfg, nil)
				dispatcher.DisableMemoryQueueForTest()
				service := &workorder.Service{Store: st, Dispatcher: dispatcher, Pack: bundle, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
				workers := &workerservice.Service{Store: st, WorkOrders: service, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
				identity := func(session string) core.WorkOrderClaimIdentity {
					return core.WorkOrderClaimIdentity{WorkerID: "fixture-worker", ClaimantID: "fixture-worker", SessionID: session}
				}
				finished := make(chan struct{}, 2)
				var uncertainAudits atomic.Int32
				var failedAudits atomic.Int32
				originalCheckpointer := workerAttemptCheckpointer
				if termination == "preserve-failure" {
					workerAttemptCheckpointer = func(context.Context, string, string, string, string, attemptCheckpoint) (*attemptCheckpointResult, error) {
						return nil, fmt.Errorf("injected preservation failure")
					}
				}
				if termination == "legacy-audited" {
					workerAttemptCheckpointer = func(ctx context.Context, root, branch, repo, repoURL string, checkpoint attemptCheckpoint) (*attemptCheckpointResult, error) {
						checkpoint.TerminationReason = "claim authority lost: different renewal text"
						return originalCheckpointer(ctx, root, branch, repo, repoURL, checkpoint)
					}
				}
				defer func() { workerAttemptCheckpointer = originalCheckpointer }()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					parts := strings.Split(r.URL.Path, "/")
					id := parts[len(parts)-2]
					action := parts[len(parts)-1]
					var response any
					var err error
					switch action {
					case "agent-credential":
						response = map[string]string{"credential_id": "child", "credential": "child-credential"}
					case "claim":
						var input struct {
							SessionID   string `json:"session_id"`
							ClientToken string `json:"client_token"`
						}
						err = json.NewDecoder(r.Body).Decode(&input)
						if err == nil {
							response, err = storetest.For(st).ClaimWorkOrder(ctx, id, core.WorkOrderClaim{WorkerID: "fixture-worker", ClaimantID: "fixture-worker", SessionID: input.SessionID, ClientToken: input.ClientToken, Lease: time.Minute, ExecutionTimeout: time.Hour})
						}
					case "renew":
						var input struct {
							SessionID string `json:"session_id"`
						}
						err = json.NewDecoder(r.Body).Decode(&input)
						if err == nil {
							response, err = workers.RenewClaim(ctx, identity(input.SessionID), id)
						}
					case "reconcile":
						response, err = workers.ReconcileClaim(ctx, identity(r.URL.Query().Get("session_id")), id)
					case "worktree-handoff":
						var input core.WorktreeHandoffRequest
						err = json.NewDecoder(r.Body).Decode(&input)
						if err == nil {
							if termination == "audit-failure" && input.Action == "audit" && id == order.ID && failedAudits.Add(1) <= 2 {
								http.Error(w, "injected audit persistence failure", 503)
								return
							}
							response, err = workers.WorktreeHandoff(ctx, id, identity(input.SessionID), input)
							if err == nil && input.Action == "audit" && uncertainAudits.Add(1) == 1 {
								http.Error(w, "audit response lost after commit", http.StatusServiceUnavailable)
								return
							}
						}
					case "finish":
						var input map[string]string
						err = json.NewDecoder(r.Body).Decode(&input)
						if err == nil && strings.HasPrefix(input["action"], "produce") {
							response, err = taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
								return st.RequestPlanRevisionCommand(ctx, lease, input["order_id"], identity(input["session_id"]), "fixture requires revised plan")
							})
						} else if err == nil {
							response, err = workers.ReleaseClaim(ctx, identity(input["session_id"]), input["order_id"], core.WorkOrderRelease{SessionID: input["session_id"], Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "fixture reached verified content"}, Outcome: core.WorkOrderOutcomeReleased})
						}
						if err == nil {
							finished <- struct{}{}
						}
					case "progress", "usage":
						response = map[string]bool{"ok": true}
					default:
						http.NotFound(w, r)
						return
					}
					if err != nil {
						http.Error(w, err.Error(), http.StatusConflict)
						return
					}
					_ = json.NewEncoder(w).Encode(response)
				}))
				defer server.Close()
				previousRenew, previousGrace := workerClaimRenewInterval, workerRunTerminalChildGrace
				if linger {
					workerClaimRenewInterval = 20 * time.Millisecond
					workerRunTerminalChildGrace = 30 * time.Millisecond
				} else {
					workerClaimRenewInterval = time.Minute
				}
				defer func() { workerClaimRenewInterval = previousRenew; workerRunTerminalChildGrace = previousGrace }()
				worktreeRoot := filepath.Join(fixture.tmp, "worktrees")
				ctx = contextWithWorktreeRoot(ctx, worktreeRoot)
				launch := func(o core.WorkOrder, action string) {
					item := workerservice.DispatchOrder{Order: o, Task: task, Repository: cfg.Repos[0], Dispatch: mode, Harness: config.Harness{Name: "fixture", Command: []string{os.Args[0], "-test.run=^TestCheckpointHandoffChild$", "--", action}}}
					var out, diagnostics bytes.Buffer
					launchCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					if termination == "teardown" && strings.HasPrefix(action, "produce") {
						go func() {
							select {
							case <-finished:
								cancel()
							case <-launchCtx.Done():
							}
						}()
					}
					err := runHarnessChildWithFirstActivityTimeoutAndOutput(launchCtx, &client{base: server.URL, workspace: "demo"}, "fixture-credential", item, 5*time.Second, &out, &diagnostics)
					wantFailure := strings.HasPrefix(action, "produce") && (termination == "preserve-failure" || termination == "audit-failure")
					if wantFailure && (err == nil || !strings.Contains(err.Error(), "checkpoint recovery pending")) {
						t.Fatalf("missing preservation failure: %v", err)
					}
					if !wantFailure && err != nil {
						t.Fatalf("launcher %s: %v\nstdout: %s\nstderr: %s", action, err, &out, &diagnostics)
					}
					if !strings.Contains(out.String(), "checkpoint fixture child executed") {
						t.Fatalf("child did not resume: %s", &out)
					}
				}
				produce := "produce"
				if linger {
					produce = "produce-linger"
				}
				launch(order, produce)
				workerAttemptCheckpointer = originalCheckpointer
				if termination == "legacy-audited" {
					lockPath, err := worktreeWriterPath(ctx, fixture.primary, task.Branch, task.Repo, fixture.origin)
					must(err)
					must(os.Remove(lockPath + ".json"))
				}
				if termination == "preserve-failure" {
					if _, err := checkoutTask(ctx, task.Branch, task.BaseBranch, task.Repo, fixture.origin, task.ID, ""); err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
						t.Fatalf("legacy successor did not reproduce dirty-checkout incident: %v", err)
					}
				}

				preservedHead := mustGitOutput(t, fixture.primary, "rev-parse", task.Branch)
				if remote := mustGitOutput(t, fixture.primary, "ls-remote", "--heads", "origin", task.Branch); termination != "preserve-failure" && !strings.HasPrefix(remote, preservedHead+"\t") {
					t.Fatalf("local checkpoint not pushed: %s", remote)
				}
				released, err := st.GetWorkOrder(ctx, order.ID)
				must(err)
				if released.LastFailureMessage != core.WorkOrderReleaseReasonPlanRevisionRequested || released.AutomaticRetryCount != 0 {
					t.Fatalf("handoff relabeled: %+v", released)
				}
				// The fixture acts as operator through the real plan-revision transition.
				current, err := st.GetTask(ctx, task.ID)
				must(err)
				latest, _, err := st.GetLatestJob(ctx, task.ID)
				must(err)
				approve := core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionRedirect, ReasonCode: dispatch.PlanRevisionApprovedReasonCode, Comment: "approve fixture revision"}
				must(dispatcher.HandleIntervention(ctx, current, latest, approve))
				must(st.CreateIntervention(ctx, approve))
				must(dispatcher.DispatchNow(ctx, task.ID))
				orders, err := st.ListTaskWorkOrders(ctx, task.ID)
				must(err)
				specOrder, err := storetest.For(st).ClaimWorkOrder(ctx, orders[len(orders)-1].ID, core.WorkOrderClaim{WorkerID: "planner", SessionID: "planner", ClientToken: "plan-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
				must(err)
				_, err = service.SubmitPlan(ctx, specOrder.ID, specOrder.SessionID, pipeline.StructuredPlan{Markdown: plan.Content, Decomposition: []pipeline.DecompositionItem{}})
				must(err)
				current, err = st.GetTask(ctx, task.ID)
				must(err)
				latest, _, err = st.GetLatestJob(ctx, task.ID)
				must(err)
				approve = core.Intervention{TaskID: task.ID, JobID: latest.ID, Action: core.InterventionApprove, ReasonCode: "plan-approved"}
				must(dispatcher.HandleIntervention(ctx, current, latest, approve))
				must(st.CreateIntervention(ctx, approve))
				must(dispatcher.DispatchNow(ctx, task.ID))
				orders, err = st.ListTaskWorkOrders(ctx, task.ID)
				must(err)
				successor := orders[len(orders)-1]
				if successor.ID == order.ID || successor.Stage != core.StageImplement {
					t.Fatalf("no fresh implementation: %+v", orders)
				}
				launch(successor, "resume")
				if head := mustGitOutput(t, fixture.primary, "rev-parse", task.Branch); termination != "preserve-failure" && head != preservedHead {
					t.Fatalf("resume made redundant checkpoint %s", head)
				}
				events, err := st.ListEvents(ctx, task.ID)
				must(err)
				count := 0
				for _, event := range events {
					if event.Kind == "work_order.attempt_checkpointed" {
						count++
						var p struct {
							WorkOrderID string `json:"work_order_id"`
						}
						must(json.Unmarshal(event.Payload, &p))
						if p.WorkOrderID != order.ID {
							t.Fatalf("misattributed checkpoint %s", event.Payload)
						}
					}
				}
				if count != 1 {
					t.Fatalf("checkpoint audit count=%d", count)
				}
			})
		}
	}
}

func TestCheckpointHandoffGitFailuresAndWriterFence(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := t.Context()
	branch := "conveyor/task-checkpoint-failure"
	path, err := checkoutTask(ctx, branch, "main", "conveyor", fixture.origin, "checkpoint-failure", "")
	if err != nil {
		t.Fatal(err)
	}
	lockPath, err := worktreeWriterPath(ctx, fixture.primary, branch, "conveyor", fixture.origin)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := acquireWorktreeWriter(ctx, lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	identity := core.WorktreeIdentity{Workspace: "demo", TaskID: "checkpoint-failure", Repository: "file:" + fixture.origin, Branch: branch, WorkOrderID: "failure-implement-1", AttemptID: "attempt-failure", SessionID: "failure-session", Generation: "generation", ClaimEventID: 1}
	writer.record = worktreeWriterRecord{Writer: identity, Producer: &identity, Parent: mustGitOutput(t, path, "rev-parse", "HEAD")}
	if err = writer.save(); err != nil {
		t.Fatal(err)
	}
	waiter, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if second, err := acquireWorktreeWriter(waiter, lockPath); err == nil {
		second.close()
		t.Fatal("second writer overlapped first")
	}
	writeFile(t, filepath.Join(path, "README.md"), "tracked change\n")
	writeFile(t, filepath.Join(path, "new.txt"), "untracked change\n")
	checkpoint := attemptCheckpoint{AttemptID: identity.AttemptID, WorkOrderID: identity.WorkOrderID, TerminationReason: "plan revision requested", Identity: &identity, Writer: writer, Authorize: func(context.Context) error { return writer.verify() }}
	hook := filepath.Join(fixture.origin, "hooks", "pre-receive")
	if err = os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = checkpointTaskWorktreeAtPath(ctx, path, branch, fixture.primary, checkpoint); err == nil || !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("expected preserved local commit and push failure: %v", err)
	}
	localHead := mustGitOutput(t, path, "rev-parse", "HEAD")
	if localHead == writer.record.Parent || writer.record.CommitSHA != localHead {
		t.Fatal("missing local recovery record")
	}
	originalMessage := mustGitOutput(t, path, "show", "-s", "--format=%B", "HEAD")
	if err = os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	checkpoint.TerminationReason = "renewal failed with different explanatory text"
	legacy := checkpoint
	legacy.Identity = nil
	if _, err = matchingAttemptCheckpointAtHEAD(ctx, path, legacy); err == nil || !strings.Contains(err.Error(), "not termination reason") {
		t.Fatalf("legacy reason mismatch was not reproduced: %v", err)
	}

	result, err := checkpointTaskWorktreeAtPath(ctx, path, branch, fixture.primary, checkpoint)
	if err != nil || result == nil || !result.Pushed || result.CommitSHA != localHead {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
	if message := mustGitOutput(t, path, "show", "-s", "--format=%B", "HEAD"); message != originalMessage {
		t.Fatal("retry rewrote original trailers")
	}
	if remote := mustGitOutput(t, path, "ls-remote", "--heads", "origin", branch); !strings.HasPrefix(remote, localHead+"\t") {
		t.Fatalf("retry failed push: %s", remote)
	}
	// Without audit or the original local producer record, clean WIP text alone
	// cannot prove authorship, even when its attempt trailer looks plausible.
	checkpoint.Writer = nil
	if _, err = matchingAttemptCheckpointAtHEAD(ctx, path, checkpoint); err == nil {
		t.Fatal("unverifiable legacy checkpoint adopted")
	}
	checkpoint.Writer = writer
	altered := writer.record
	altered.Writer.Generation = "successor"
	writer.record = altered
	if err = writer.save(); err != nil {
		t.Fatal(err)
	}
	writer.record.Writer.Generation = "generation"
	before := mustGitOutput(t, path, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(path, "new.txt"), "successor work\n")
	if _, err = checkpointTaskWorktreeAtPath(ctx, path, branch, fixture.primary, checkpoint); err == nil {
		t.Fatal("stale generation changed worktree")
	}
	if head := mustGitOutput(t, path, "rev-parse", "HEAD"); head != before {
		t.Fatal("stale writer committed")
	}
	if status := mustGitOutput(t, path, "diff", "--cached", "--name-only"); status != "" {
		t.Fatalf("stale writer staged edits: %s", status)
	}
}

// Transport-only launcher fixtures use this response after their own claim
// checks. Store and end-to-end fixtures above exercise real admission rules.
func writeTestWriterAdmission(w http.ResponseWriter, r *http.Request, identity core.WorktreeIdentity) {
	var request core.WorktreeHandoffRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	identity.SessionID = request.SessionID
	identity.Generation = request.Generation
	_ = json.NewEncoder(w).Encode(core.WorktreeHandoff{Writer: identity})
}

func TestCheckpointHandoffRejectsRemoteAheadBeforeStaging(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := t.Context()
	branch := "conveyor/task-divergent"
	path, err := checkoutTask(ctx, branch, "main", "conveyor", fixture.origin, "divergent", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "new.txt"), "first\n")
	checkpoint := attemptCheckpoint{AttemptID: "attempt-divergent", WorkOrderID: "divergent-implement", TerminationReason: "plan revision requested"}
	result, err := checkpointTaskWorktreeAtPath(ctx, path, branch, fixture.primary, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, fixture.seed, "fetch", "origin", branch)
	mustGit(t, fixture.seed, "checkout", "--detach", "FETCH_HEAD")
	writeFile(t, filepath.Join(fixture.seed, "remote.txt"), "new remote work\n")
	mustGit(t, fixture.seed, "add", "remote.txt")
	mustGit(t, fixture.seed, "commit", "-m", "remote writer")
	mustGit(t, fixture.seed, "push", "origin", "HEAD:refs/heads/"+branch)
	writeFile(t, filepath.Join(path, "new.txt"), "local dirty work\n")
	checkpoint.Identity = &core.WorktreeIdentity{AttemptID: checkpoint.AttemptID, WorkOrderID: checkpoint.WorkOrderID, CommitSHA: result.CommitSHA, CheckpointEventID: 1}
	if _, err = checkpointTaskWorktreeAtPath(ctx, path, branch, fixture.primary, checkpoint); err == nil || !strings.Contains(err.Error(), "ahead or divergent") {
		t.Fatalf("remote conflict accepted: %v", err)
	}
	if head := mustGitOutput(t, path, "rev-parse", "HEAD"); head != result.CommitSHA {
		t.Fatal("conflict created a commit")
	}
	if staged := mustGitOutput(t, path, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("conflict staged files: %s", staged)
	}
}
