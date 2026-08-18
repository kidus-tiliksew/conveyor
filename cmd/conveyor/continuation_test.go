package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestPlanContinuationLaunchEligibilityMatrix(t *testing.T) {
	base := core.WorkOrder{
		Stage: core.StageImplement, LastAttemptID: "attempt-1", LastFailureMessage: core.WorkOrderReleaseReasonOperatorCheckpointReached,
		ContinuationSessionID: "native-1", ContinuationAttemptID: "attempt-1", ContinuationHarness: "claude", ContinuationLaunchEnvironment: "worker:one",
	}
	harness := config.Harness{Name: "claude", ResumeCommand: []string{"--resume", "{session_id}"}}
	tests := []struct {
		name     string
		order    core.WorkOrder
		harness  config.Harness
		env      string
		resumes  bool
		reasonIn string
	}{
		{"checkpoint", base, harness, "worker:one", true, "eligible"},
		{"declined plan revision", func() core.WorkOrder {
			value := base
			value.LastFailureMessage = core.WorkOrderReleaseReasonPlanRevisionRequested
			return value
		}(), harness, "worker:one", true, "eligible"},
		{"crash", func() core.WorkOrder { value := base; value.LastFailureMessage = "harness exited"; return value }(), harness, "worker:one", false, "not eligible"},
		{"claim loss", func() core.WorkOrder { value := base; value.LastFailureMessage = "claim authority lost"; return value }(), harness, "worker:one", false, "not eligible"},
		{"missing metadata", func() core.WorkOrder { value := base; value.ContinuationSessionID = ""; return value }(), harness, "worker:one", false, "not eligible"},
		{"different environment", base, harness, "worker:two", false, "environment differs"},
		{"different harness", base, config.Harness{Name: "codex", ResumeCommand: []string{"resume", "{session_id}"}}, "worker:one", false, "harness differs"},
		{"no capability", base, config.Harness{Name: "claude"}, "worker:one", false, "no resume capability"},
		{"review", func() core.WorkOrder { value := base; value.Stage = core.StageReview; return value }(), harness, "worker:one", false, "not eligible"},
		{"post-bounce implement", func() core.WorkOrder { value := base; value.ContinuationAttemptID = "review-attempt"; return value }(), harness, "worker:one", false, "not eligible"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := planContinuationLaunch(test.order, test.harness, test.env)
			if plan.Resume != test.resumes || !strings.Contains(plan.Reason, test.reasonIn) {
				t.Fatalf("plan=%+v", plan)
			}
		})
	}
}

func TestContinuationLaunchEnvironmentSeparatesRunAndWorker(t *testing.T) {
	worker := continuationLaunchEnvironment(core.WorkOrder{WorkerID: "worker-1"}, "worker", "demo", "credential")
	run := continuationLaunchEnvironment(core.WorkOrder{}, "run", "demo", "credential")
	if worker != "worker:worker-1" || !strings.HasPrefix(run, "run:") || run == worker {
		t.Fatalf("worker=%q run=%q", worker, run)
	}
	if other := continuationLaunchEnvironment(core.WorkOrder{}, "run", "demo", "other-credential"); other == run {
		t.Fatalf("different run credentials shared identity %q", run)
	}
}

func TestContinuationSessionObserverRecognizesHarnessLifecycleOnly(t *testing.T) {
	var observed []string
	observer := newContinuationSessionObserver(func(value string) { observed = append(observed, value) })
	ignored := `{"type":"tool","session_id":"ignore-child"}`
	_, _ = observer.Write([]byte(`{"type":"thread.started","thread_id":"codex-native"}` + "\n" + ignored[:len(ignored)/2]))
	_, _ = observer.Write([]byte(ignored[len(ignored)/2:] + "\n" + `{"type":"system","subtype":"init","session_id":"claude-native"}` + "\n"))
	if !reflect.DeepEqual(observed, []string{"codex-native", "claude-native"}) {
		t.Fatalf("observed=%v", observed)
	}
}

func TestContinuationResumeArgvPromptAndHistory(t *testing.T) {
	argv := appendContinuationResumeArgv([]string{"claude", "-p", "prompt"}, []string{"--resume", "{session_id}"}, "native")
	if !reflect.DeepEqual(argv, []string{"claude", "-p", "prompt", "--resume", "native"}) {
		t.Fatalf("argv=%v", argv)
	}
	order := core.WorkOrder{LastAttemptID: "attempt-1", OperatorDirection: "Proceed with the approved choice."}
	prompt := continuationRecoveryPrompt("base", order)
	if !strings.Contains(prompt, order.OperatorDirection) {
		t.Fatalf("prompt=%q", prompt)
	}
}

func TestReportDispatchContinuationUsesSharedRunAndWorkerCommandPlane(t *testing.T) {
	for _, dispatch := range []string{"run", "worker"} {
		t.Run(dispatch, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Params struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request.Params.Name != "report_continuation" || request.Params.Arguments["continuation_session_id"] != "native" || request.Params.Arguments["launch_environment"] != dispatch+":local" {
					t.Fatalf("request=%+v", request)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{\"ok\":true}`}}}})
			}))
			defer server.Close()
			c := &client{base: server.URL, workspace: "demo"}
			item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "order"}, Dispatch: dispatch}
			err := c.reportDispatchContinuationContext(t.Context(), "credential", item, "claim-session", core.WorkOrderContinuation{SessionID: "native", AttemptID: "attempt", Harness: "claude", LaunchEnvironment: dispatch + ":local"})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalExecutionLoaderRejectsInvalidResumeCapability(t *testing.T) {
	harness := config.HarnessTemplates()[1].Harness
	choices := localExecutionChoices{
		Spec:      localStageChoice{Harness: harness.Name, Model: "model", Effort: "high", Timeout: "30m"},
		Implement: localStageChoice{Harness: harness.Name, Model: "model", Effort: "high", Timeout: "4h"},
		Review:    localStageChoice{Harness: harness.Name, Model: "model", Effort: "high", Timeout: "1h"},
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := writeLocalExecutionConfig(path, "demo", choices, []config.Harness{harness}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("{session_id}"), []byte("literal-session"), 1)
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadLocalExecutionSetup(path); err == nil || !strings.Contains(err.Error(), "resume_command must contain exactly one {session_id}") {
		t.Fatalf("loader error=%v", err)
	}
}

func TestRecoveredContinuationLaunchesForRunAndWorker(t *testing.T) {
	for _, dispatch := range []string{"run", "worker"} {
		for _, scenario := range []struct {
			name   string
			reject bool
		}{{name: "resume"}, {name: "rejected-resume-cold-fallback", reject: true}} {
			t.Run(dispatch+"/"+scenario.name, func(t *testing.T) {
				t.Setenv("CONVEYOR_FAKE_HARNESS", "1")
				t.Setenv("CONVEYOR_FAKE_HARNESS_NATIVE_SESSION", "native-current")
				if scenario.reject {
					t.Setenv("CONVEYOR_FAKE_HARNESS_REJECT_RESUME", "1")
				}
				argvReport := filepath.Join(t.TempDir(), "argv.txt")
				t.Setenv("CONVEYOR_FAKE_HARNESS_ARGV_REPORT", argvReport)
				credential := dispatch + "-credential"
				state := core.WorkOrderClaimed
				var mu sync.Mutex
				var sessionID, history string
				var capture map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+credential {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					if r.URL.Path == "/mcp" {
						var request struct {
							Params struct {
								Name      string         `json:"name"`
								Arguments map[string]any `json:"arguments"`
							} `json:"params"`
						}
						_ = json.NewDecoder(r.Body).Decode(&request)
						mu.Lock()
						switch request.Params.Name {
						case "get_work_order":
						case "report_continuation_launch":
							history, _ = request.Params.Arguments["mode"].(string)
						case "report_continuation":
							capture = request.Params.Arguments
						case "submit_for_review":
							state = core.WorkOrderSubmitted
						default:
							mu.Unlock()
							http.Error(w, "unexpected tool", http.StatusBadRequest)
							return
						}
						mu.Unlock()
						_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{\"ok\":true}`}}}})
						return
					}
					switch {
					case strings.HasSuffix(r.URL.Path, "/claim"):
						var request struct {
							SessionID string `json:"session_id"`
						}
						_ = json.NewDecoder(r.Body).Decode(&request)
						sessionID = request.SessionID
						claim := core.WorkOrder{
							ID: "recovered-implement", TaskID: "task", Stage: core.StageImplement, State: core.WorkOrderClaimed,
							SessionID: sessionID, AttemptID: "attempt-current", LastAttemptID: "attempt-prior",
							LastFailureMessage: core.WorkOrderReleaseReasonOperatorCheckpointReached, OperatorDirection: "Proceed with the selected option.",
							ContinuationSessionID: "native-prior", ContinuationAttemptID: "attempt-prior", ContinuationHarness: "fake",
							LeaseExpiresAt: time.Now().Add(time.Minute),
						}
						if dispatch == "worker" {
							claim.WorkerID = "worker-1"
							claim.ContinuationLaunchEnvironment = "worker:worker-1"
						} else {
							claim.ContinuationLaunchEnvironment = continuationLaunchEnvironment(claim, dispatch, "demo", credential)
						}
						_ = json.NewEncoder(w).Encode(claim)
					case strings.HasSuffix(r.URL.Path, "/renew"):
						_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "recovered-implement", State: state, LeaseExpiresAt: time.Now().Add(time.Minute)})
					case strings.HasSuffix(r.URL.Path, "/reconcile"):
						_ = json.NewEncoder(w).Encode(workerservice.ClaimReconciliation{WorkOrder: core.WorkOrder{ID: "recovered-implement", State: state}, Authorized: state == core.WorkOrderClaimed})
					default:
						http.NotFound(w, r)
					}
				}))
				defer server.Close()

				c := &client{base: server.URL, workspace: "demo"}
				item := workerservice.DispatchOrder{
					Order: core.WorkOrder{ID: "recovered-implement", TaskID: "task", Stage: core.StageImplement},
					Task:  core.Task{ID: "task"}, Dispatch: dispatch,
					Harness: config.Harness{
						Name: "fake", Command: []string{os.Args[0], "-test.run=TestWorkerHarnessHelper", "--", "{prompt}", "{mcp_config}"},
						ResumeCommand: []string{"--resume", "{session_id}"}, MCPTransport: config.MCPTransportJSONFile,
					},
				}
				var stdout, stderr bytes.Buffer
				if err := runHarnessChildWithOutput(t.Context(), c, credential, item, &stdout, &stderr); err != nil {
					t.Fatalf("launch: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
				}
				argv, err := os.ReadFile(argvReport)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				for {
					mu.Lock()
					reported := capture != nil
					mu.Unlock()
					if reported || time.Now().After(deadline) {
						break
					}
					time.Sleep(time.Millisecond)
				}
				mu.Lock()
				defer mu.Unlock()
				if strings.Contains(string(argv), "--resume\nnative-prior") == scenario.reject || !strings.Contains(string(argv), "Proceed with the selected option.") {
					t.Fatalf("argv report=%q", argv)
				}
				wantHistory := "resumed"
				if scenario.reject {
					wantHistory = "cold"
				}
				if history != wantHistory {
					t.Fatalf("history=%q", history)
				}
				if capture["continuation_session_id"] != "native-current" || capture["attempt_id"] != "attempt-current" {
					t.Fatalf("capture=%+v", capture)
				}
			})
		}
	}
}
