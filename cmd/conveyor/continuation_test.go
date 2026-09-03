package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
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
		name    string
		order   core.WorkOrder
		harness config.Harness
		env     string
		resume  bool
		reason  string
	}{
		{name: "eligible", order: base, harness: harness, env: "worker:one", resume: true, reason: "eligible_match"},
		{name: "no metadata", order: func() core.WorkOrder { v := base; v.ContinuationSessionID = ""; return v }(), harness: harness, env: "worker:one", reason: "server_ineligible"},
		{name: "involuntary recovery", order: func() core.WorkOrder { v := base; v.LastFailureMessage = "harness exited"; return v }(), harness: harness, env: "worker:one", reason: "server_ineligible"},
		{name: "review", order: func() core.WorkOrder { v := base; v.Stage = core.StageReview; return v }(), harness: harness, env: "worker:one", reason: "server_ineligible"},
		{name: "post bounce", order: func() core.WorkOrder { v := base; v.ContinuationAttemptID = "other-attempt"; return v }(), harness: harness, env: "worker:one", reason: "server_ineligible"},
		{name: "no contract", order: base, harness: config.Harness{Name: "claude"}, env: "worker:one", reason: "no_local_resume_contract"},
		{name: "harness mismatch", order: base, harness: config.Harness{Name: "codex", ResumeCommand: []string{"resume", "{session_id}"}}, env: "worker:one", reason: "harness_mismatch"},
		{name: "environment mismatch", order: base, harness: harness, env: "worker:two", reason: "launch_environment_mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := planContinuationLaunch(test.order, test.harness, test.env)
			if got.Resume != test.resume || got.Reason != test.reason {
				t.Fatalf("plan=%+v", got)
			}
		})
	}
}

func TestContinuationObserverFollowsResumeCapability(t *testing.T) {
	contract := []string{"--resume", "{session_id}"}
	for _, name := range []string{"claude", "cursor"} {
		if !continuationObserverEnabled(config.Harness{Name: name, ResumeCommand: contract}, "worker:one") {
			t.Fatalf("%s resume contract should enable capture", name)
		}
	}
	for _, harness := range []config.Harness{{Name: "claude"}, {Name: "codex"}} {
		if continuationObserverEnabled(harness, "worker:one") {
			t.Fatalf("unexpected capture for %+v", harness)
		}
	}
	if continuationObserverEnabled(config.Harness{Name: "claude", ResumeCommand: contract}, "") {
		t.Fatal("capture without a reportable environment")
	}
}

func TestContinuationSessionObserverCapturesCursorInitEnvelope(t *testing.T) {
	var observed string
	observer := newContinuationSessionObserver(func(value string) { observed = value })
	_, _ = observer.Write([]byte(`{"type":"system","subtype":"init","session_id":"cursor-native"}` + "\n"))
	if observed != "cursor-native" {
		t.Fatalf("observed=%q", observed)
	}
}

func TestContinuationSessionObserverIsBoundedAndTolerant(t *testing.T) {
	var observed []string
	observer := newContinuationSessionObserver(func(value string) { observed = append(observed, value) })
	_, _ = observer.Write([]byte("not-json\n" + strings.Repeat("x", continuationLineLimit)))
	_, _ = observer.Write([]byte("overflow\n"))
	line := `{"type":"system","subtype":"init","session_id":"claude-native"}` + "\n"
	_, _ = observer.Write([]byte(line[:19]))
	_, _ = observer.Write([]byte(line[19:]))
	_, _ = observer.Write([]byte(`{"type":"system","subtype":"init","session_id":"later"}` + "\n"))
	if !reflect.DeepEqual(observed, []string{"claude-native"}) {
		t.Fatalf("observed=%v", observed)
	}
}

func TestContinuationResumeArgvAndPrompt(t *testing.T) {
	argv := appendContinuationResumeArgv([]string{"claude", "-p", "prompt"}, []string{"--resume", "{session_id}"}, "native")
	if !reflect.DeepEqual(argv, []string{"claude", "-p", "prompt", "--resume", "native"}) {
		t.Fatalf("argv=%v", argv)
	}
	prompt := continuationRecoveryPrompt("base recovery context", core.WorkOrder{OperatorDirection: "Use the safe option."})
	if !strings.Contains(prompt, "base recovery context") || !strings.Contains(prompt, "# Operator direction\n\nUse the safe option.") {
		t.Fatalf("prompt=%q", prompt)
	}
}

func TestContinuationLaunchEnvironmentSeparatesRunAndWorker(t *testing.T) {
	worker := continuationLaunchEnvironment(core.WorkOrder{WorkerID: "worker-1"}, "worker", "demo", "credential")
	run := continuationLaunchEnvironment(core.WorkOrder{}, "run", "demo", "credential")
	if worker != "worker:worker-1" || !strings.HasPrefix(run, "run:") || run == worker {
		t.Fatalf("worker=%q run=%q", worker, run)
	}
	if other := continuationLaunchEnvironment(core.WorkOrder{}, "run", "demo", "other-credential"); other == run {
		t.Fatalf("different credentials shared identity %q", run)
	}
}

func TestStartHarnessCommandFallsBackOnlyForResumeStartFailure(t *testing.T) {
	var calls int
	factory := func([]string) *exec.Cmd {
		calls++
		if calls == 1 {
			return exec.Command("/definitely/missing/conveyor-resume")
		}
		return exec.Command("sh", "-c", "exit 0")
	}
	command, resumed, resumeErr, err := startHarnessCommandWithColdFallback(true, []string{"resume"}, []string{"cold"}, factory)
	if err != nil || resumeErr == nil || resumed || calls != 2 {
		t.Fatalf("resumed=%v resumeErr=%v err=%v calls=%d", resumed, resumeErr, err, calls)
	}
	if waitErr := command.Wait(); waitErr != nil {
		t.Fatal(waitErr)
	}
}

func TestReportDispatchContinuationUsesClaimAuthorizedMCP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer credential" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var request struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Params.Name != "report_continuation" || request.Params.Arguments["session_id"] != "claim-session" || request.Params.Arguments["continuation_session_id"] != "native" {
			t.Fatalf("request=%+v", request)
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`)
	}))
	defer server.Close()
	c := &client{base: server.URL, workspace: "demo"}
	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: "order"}, Dispatch: "run"}
	err := c.reportDispatchContinuationContext(t.Context(), "credential", item, "claim-session", core.WorkOrderContinuation{
		SessionID: "native", AttemptID: "attempt", Harness: "claude", LaunchEnvironment: "run:local",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContinuationReporterFailureIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	warning := make(chan string, 1)
	reporter := newContinuationReporter(&client{base: server.URL, workspace: "demo"}, "credential", workerservice.DispatchOrder{
		Order: core.WorkOrder{ID: "order"}, Harness: config.Harness{Name: "claude"},
	}, core.WorkOrder{SessionID: "claim", AttemptID: "attempt"}, "run:local", func(message string) { warning <- message })
	reporter.Observe("native")
	reporter.Stop()
	select {
	case message := <-warning:
		if !strings.Contains(message, "report continuation metadata") {
			t.Fatalf("warning=%q", message)
		}
	case <-time.After(time.Second):
		t.Fatal("report failure was not observed")
	}
}
