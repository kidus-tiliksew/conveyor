package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Tests for conveyor order recover|redispatch (req-operator-next-action AC-5.5).

type orderRequest struct {
	method, path string
	header       http.Header
	body         map[string]any
}

func serveOrders(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*client, func() []orderRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []orderRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		request := orderRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &request.body); err != nil {
				t.Errorf("body %q: %v", raw, err)
			}
		}
		mu.Lock()
		seen = append(seen, request)
		mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL, token: "t", workspace: "demo"}, func() []orderRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]orderRequest(nil), seen...)
	}
}

func okJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"o-1","state":"queued"}`))
}

// serveOrderReads answers the order reads from listed, a JSON array of
// orders: GET /v1/work-orders with the array and GET /v1/work-orders/{id}
// with the matching order or 404. legacy answers 404 to every exact read, as
// a server that predates the route does. It reports whether it answered.
func serveOrderReads(t *testing.T, w http.ResponseWriter, r *http.Request, listed string, legacy bool) bool {
	t.Helper()
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/work-orders") {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/work-orders" {
		_, _ = w.Write([]byte(listed))
		return true
	}
	var orders []map[string]any
	if err := json.Unmarshal([]byte(listed), &orders); err != nil {
		t.Fatalf("listed orders %q: %v", listed, err)
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/work-orders/")
	for _, order := range orders {
		if order["id"] == id && !legacy {
			_ = json.NewEncoder(w).Encode(order)
			return true
		}
	}
	http.Error(w, "work order not found", http.StatusNotFound)
	return true
}

func TestOrderRecoverRequestID(t *testing.T) {
	c, seen := serveOrders(t, okJSON)
	var out bytes.Buffer
	if err := runOrderRecover(c, &out, "261011-aaaaaa-implement-1", "", "", nil, false); err != nil {
		t.Fatal(err)
	}
	requests := seen()
	if len(requests) != 1 || requests[0].method != http.MethodPost || requests[0].path != "/v1/work-orders/261011-aaaaaa-implement-1/recover" {
		t.Fatalf("requests = %+v", requests)
	}
	id, _ := requests[0].body["request_id"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("generated request_id = %q", id)
	}
	if requests[0].header.Get("X-Request-Id") != id || requests[0].header.Get("X-Idempotency-Key") != id {
		t.Fatalf("headers X-Request-Id=%q X-Idempotency-Key=%q, want %q", requests[0].header.Get("X-Request-Id"), requests[0].header.Get("X-Idempotency-Key"), id)
	}

	if err := runOrderRecover(c, &out, "261011-aaaaaa-implement-1", "use the v2 schema", "x", nil, false); err != nil {
		t.Fatal(err)
	}
	second := seen()[1]
	if second.body["request_id"] != "x" || second.header.Get("X-Request-Id") != "x" || second.header.Get("X-Idempotency-Key") != "x" {
		t.Fatalf("--request-id not sent verbatim: %+v", second)
	}
	if second.body["direction"] != "use the v2 schema" {
		t.Fatalf("direction = %v", second.body["direction"])
	}
}

// Tests for conveyor order recover --disposition: the command reads the
// disposition set from the next action of the order's task and sends one
// entry for each attempt (component-verification-service invariant 45;
// component-next-action invariant 15; DEC-133(4)).

const dispositionOrders = `[{"id":"t-verify-2","task_id":"t","state":"queued","stage":"verify"}]`

const dispositionNextAction = `{"kind":"verification_disposition","current_order":{"id":"t-verify-2","stage":"verify","state":"queued"},"choices":[{"id":"record_dispositions","method":"POST","path":"/v1/work-orders/t-verify-2/recover"}],"details":{"dispositions":[` +
	`{"context_id":"ctx-a","run_id":"run-a","operation_ids":[],"input_digest":"digest-a","subject":"kit:provider/login","state":"blocked"},` +
	`{"context_id":"ctx-b","run_id":"run-b","operation_ids":["op-1","op-2"],"input_digest":"digest-b","subject":"ordinary:checkout","state":"running"}]}}`

// serveDispositions answers the order read, the task's next action with
// nextAction, and the recover POST.
func serveDispositions(t *testing.T, nextAction string) (*client, func() []orderRequest) {
	t.Helper()
	return serveOrders(t, func(w http.ResponseWriter, r *http.Request) {
		if serveOrderReads(t, w, r, dispositionOrders, false) {
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/t/next-action" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(nextAction))
			return
		}
		okJSON(w, r)
	})
}

func posted(requests []orderRequest) []orderRequest {
	var out []orderRequest
	for _, r := range requests {
		if r.method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

func TestOrderRecoverDispositionsFromNextAction(t *testing.T) {
	c, seen := serveDispositions(t, dispositionNextAction)
	var out bytes.Buffer
	disposition := &orderRecoverDisposition{Default: "applied", Runs: []string{"run-b=not_applied"}, Reason: "Operator restored the fixtures."}
	if err := runOrderRecover(c, &out, "t-verify-2", "", "ff-72a154", disposition, false); err != nil {
		t.Fatal(err)
	}
	requests := seen()
	if len(requests) != 3 || requests[1].path != "/v1/tasks/t/next-action" || requests[2].method != http.MethodPost || requests[2].path != "/v1/work-orders/t-verify-2/recover" || requests[2].body["request_id"] != "ff-72a154" {
		t.Fatalf("requests = %+v", requests)
	}
	want := map[string]any{"reason": "Operator restored the fixtures.", "dispositions": []any{
		map[string]any{"context_id": "ctx-a", "run_id": "run-a", "operation_ids": []any{}, "input_digest": "digest-a", "disposition": "applied"},
		map[string]any{"context_id": "ctx-b", "run_id": "run-b", "operation_ids": []any{"op-1", "op-2"}, "input_digest": "digest-b", "disposition": "not_applied"},
	}}
	if got := requests[2].body["verification"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("verification = %#v, want %#v", got, want)
	}
	if _, present := requests[2].body["direction"]; present {
		t.Fatalf("empty direction was sent: %+v", requests[2].body)
	}
	for _, line := range []string{"verification authorization recovery:ff-72a154", "run run-a: applied (kit:provider/login)", "run run-b: not_applied (ordinary:checkout)", "next verifier: conveyor kit verify uses these authorizations and needs no flag"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("output %q lacks %q", out.String(), line)
		}
	}

	out.Reset()
	if err := runOrderRecover(c, &out, "t-verify-2", "retry the exercise", "", &orderRecoverDisposition{Default: "unknown", Reason: "Provider unreachable."}, true); err != nil {
		t.Fatal(err)
	}
	second := posted(seen())[1]
	for _, entry := range second.body["verification"].(map[string]any)["dispositions"].([]any) {
		if entry.(map[string]any)["disposition"] != "unknown" {
			t.Fatalf("default disposition not applied to every entry: %#v", second.body["verification"])
		}
	}
	if second.body["direction"] != "retry the exercise" {
		t.Fatalf("direction = %v", second.body["direction"])
	}
	if out.String() != `{"id":"o-1","state":"queued"}`+"\n" {
		t.Fatalf("--json output = %q, want the server body verbatim", out.String())
	}

	before := len(seen())
	if err := runOrderRecover(c, &out, "t-verify-2", "", "", nil, false); err != nil {
		t.Fatal(err)
	}
	plain := seen()[before:]
	if len(plain) != 1 || plain[0].method != http.MethodPost {
		t.Fatalf("recovery without --disposition read the next action: %+v", plain)
	}
	if _, present := plain[0].body["verification"]; present {
		t.Fatalf("recovery without --disposition sent a verification object: %+v", plain[0].body)
	}
}

func TestOrderRecoverDispositionsRefuseBeforeSending(t *testing.T) {
	complete := orderRecoverDisposition{Default: "applied", Reason: "reason"}
	flagCases := map[string]struct {
		mutate func(*orderRecoverDisposition)
		want   string
	}{
		"no disposition":           {func(d *orderRecoverDisposition) { d.Default = "" }, "require --disposition"},
		"bad disposition":          {func(d *orderRecoverDisposition) { d.Default = "skipped" }, "applied, not_applied, or unknown"},
		"missing reason":           {func(d *orderRecoverDisposition) { d.Reason = " " }, "requires --reason"},
		"malformed run":            {func(d *orderRecoverDisposition) { d.Runs = []string{"run-a"} }, "<run-id>=<disposition>"},
		"empty run value":          {func(d *orderRecoverDisposition) { d.Runs = []string{"run-a="} }, "<run-id>=<disposition>"},
		"bad run value":            {func(d *orderRecoverDisposition) { d.Runs = []string{"run-a=skipped"} }, "applied, not_applied, or unknown"},
		"run named twice":          {func(d *orderRecoverDisposition) { d.Runs = []string{"run-a=applied", "run-a=not_applied"} }, "more than once"},
		"run outside set":          {func(d *orderRecoverDisposition) { d.Runs = []string{"run-z=not_applied"} }, "run-z: the run is not in the disposition set"},
		"one override outside set": {func(d *orderRecoverDisposition) { d.Runs = []string{"run-b=not_applied", "run-z=applied"} }, "run-z"},
	}
	c, seen := serveDispositions(t, dispositionNextAction)
	for name, tc := range flagCases {
		t.Run(name, func(t *testing.T) {
			d := complete
			tc.mutate(&d)
			err := runOrderRecover(c, io.Discard, "t-verify-2", "", "", &d, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if requests := posted(seen()); len(requests) != 0 {
		t.Fatalf("refused recoveries sent %d posts", len(requests))
	}

	statements := map[string]struct {
		nextAction string
		want       string
	}{
		"no set": {`{"kind":"verification_disposition","current_order":{"id":"t-verify-2"},"details":{"refusals":[{"choice":"record_dispositions","text":"verify order t-verify-2 has no unresolved operations and no retry-gated attempts to dispose"}]}}`,
			"states no disposition set for work order t-verify-2: verify order t-verify-2 has no unresolved operations"},
		"another kind":  {`{"kind":"agent_question","current_order":{"id":"t-verify-2"},"details":{}}`, "is agent_question on work order t-verify-2, so it does not ask for dispositions on work order t-verify-2"},
		"another order": {`{"kind":"verification_disposition","current_order":{"id":"t-verify-1"},"details":{"dispositions":[{"context_id":"ctx","run_id":"run-a","operation_ids":[],"input_digest":"d","subject":"ordinary:o","state":"blocked"}]}}`, "is verification_disposition on work order t-verify-1, so it does not ask for dispositions on work order t-verify-2"},
	}
	for name, tc := range statements {
		t.Run(name, func(t *testing.T) {
			c, seen := serveDispositions(t, tc.nextAction)
			err := runOrderRecover(c, io.Discard, "t-verify-2", "", "", &orderRecoverDisposition{Default: "applied", Reason: "reason"}, false)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing was sent") {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if requests := posted(seen()); len(requests) != 0 {
				t.Fatalf("refused recovery sent %+v", requests)
			}
		})
	}
}

func TestOrderRecoverDispositionFlags(t *testing.T) {
	c, seen := serveDispositions(t, dispositionNextAction)
	for _, retired := range [][]string{{"--context-id", "ctx-a"}, {"--run-id", "run-a"}, {"--input-digest", "digest-a"}, {"--operation-id", "op-1"}} {
		args := append([]string{"order", "recover", "t-verify-2", "--disposition", "applied", "--reason", "r"}, retired...)
		stderr, code := runConveyorMain(t, c.base, args...)
		if code != 1 || !strings.Contains(stderr, "unknown flag: "+retired[0]) {
			t.Fatalf("%s: exit=%d stderr=%q", retired[0], code, stderr)
		}
	}
	stderr, code := runConveyorMain(t, c.base, "order", "recover", "t-verify-2", "--run", "run-a=applied")
	if code != 1 || !strings.Contains(stderr, "require --disposition") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if requests := seen(); len(requests) != 0 {
		t.Fatalf("refused recoveries sent %d requests", len(requests))
	}
	stderr, code = runConveyorMain(t, c.base, "order", "recover", "t-verify-2", "--request-id", "r-1", "--disposition", "not_applied", "--run", "run-a=applied", "--run", "run-b=unknown", "--reason", "No change observed.")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	requests := posted(seen())
	if len(requests) != 1 || requests[0].body["request_id"] != "r-1" {
		t.Fatalf("posts = %+v", requests)
	}
	entries := requests[0].body["verification"].(map[string]any)["dispositions"].([]any)
	if len(entries) != 2 || entries[0].(map[string]any)["disposition"] != "applied" || entries[1].(map[string]any)["disposition"] != "unknown" {
		t.Fatalf("dispositions = %#v", entries)
	}
}

// TestOrderRecoverHelpNamesNoKitVerifyFlag pins that the recover help teaches
// only the recovery command: the next verification run needs no flag of
// conveyor kit verify (component-verification-service invariant 19;
// DEC-133(3)).
func TestOrderRecoverHelpNamesNoKitVerifyFlag(t *testing.T) {
	help := orderRecoverCmd().Long
	if strings.Contains(help, "--replay-authorization") || strings.Contains(help, "kit verify --") {
		t.Fatalf("recover help names a kit verify flag:\n%s", help)
	}
	if !strings.Contains(help, "The next verification run uses these authorizations. It needs no flag.") {
		t.Fatalf("recover help does not say that the next run needs no flag:\n%s", help)
	}
}

func TestOrderRedispatchResolvesTask(t *testing.T) {
	c, seen := serveOrders(t, func(w http.ResponseWriter, r *http.Request) {
		if serveOrderReads(t, w, r, `[{"id":"261011-other-spec-1","task_id":"261011-other"},{"id":"261011-aaaaaa-spec-2","task_id":"261011-aaaaaa","state":"completed"}]`, false) {
			return
		}
		okJSON(w, r)
	})
	var out bytes.Buffer
	// A completed order still names its task through the exact read, which
	// serves every state; the list holds live orders only.
	if err := runOrderRedispatch(c, &out, "261011-aaaaaa-spec-2", "", false); err != nil {
		t.Fatal(err)
	}
	requests := seen()
	if len(requests) != 2 || requests[0].method != http.MethodGet || requests[0].path != "/v1/work-orders/261011-aaaaaa-spec-2" ||
		requests[1].method != http.MethodPost || requests[1].path != "/v1/tasks/261011-aaaaaa/redispatch" {
		t.Fatalf("requests = %+v", requests)
	}
	id, _ := requests[1].body["request_id"].(string)
	if len(id) != 32 || requests[1].header.Get("X-Request-Id") != id || requests[1].header.Get("X-Idempotency-Key") != id {
		t.Fatalf("request id %q headers %v", id, requests[1].header)
	}

	// A 404 searches the list once before reporting the order missing.
	err := runOrderRedispatch(c, &out, "261011-missing-spec-1", "", false)
	if err == nil || err.Error() != "work order 261011-missing-spec-1 not found" {
		t.Fatalf("unknown order err = %v", err)
	}
	if got := seen(); len(got) != 4 || got[2].path != "/v1/work-orders/261011-missing-spec-1" || got[3].method != http.MethodGet || got[3].path != "/v1/work-orders" {
		t.Fatalf("unknown order must read exactly, search the list, and not post: %+v", got)
	}
}

// A server that predates GET /v1/work-orders/{id} answers 404; the commands
// then resolve a live order from the list (component-work-orders, Listing).
func TestOrderCommandsFallBackToListOnOlderServer(t *testing.T) {
	listed := `[{"id":"261011-aaaaaa-implement-1","task_id":"261011-aaaaaa","state":"stale"}]`
	c, seen := serveOrders(t, func(w http.ResponseWriter, r *http.Request) {
		if serveOrderReads(t, w, r, listed, true) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r","work_order":{"id":"261011-aaaaaa-implement-1","state":"cancelled"}}`))
	})
	if err := runOrderRedispatch(c, io.Discard, "261011-aaaaaa-implement-1", "", false); err != nil {
		t.Fatal(err)
	}
	if err := runOrderRetire(c, io.Discard, "261011-aaaaaa-implement-1", "obsolete", "", false); err != nil {
		t.Fatal(err)
	}
	requests := seen()
	want := []string{
		"GET /v1/work-orders/261011-aaaaaa-implement-1", "GET /v1/work-orders", "POST /v1/tasks/261011-aaaaaa/redispatch",
		"GET /v1/work-orders/261011-aaaaaa-implement-1", "GET /v1/work-orders", "POST /v1/work-orders/261011-aaaaaa-implement-1/preempt",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %+v", requests)
	}
	for i, request := range requests {
		if got := request.method + " " + request.path; got != want[i] {
			t.Fatalf("request %d = %s, want %s", i, got, want[i])
		}
	}
}

func TestOrderCommands409Passthrough(t *testing.T) {
	const refusal = "task is already active"
	c, _ := serveOrders(t, func(w http.ResponseWriter, r *http.Request) {
		if serveOrderReads(t, w, r, `[{"id":"o-1","task_id":"t-1"}]`, false) {
			return
		}
		http.Error(w, refusal, http.StatusConflict)
	})
	for name, run := range map[string]func() error{
		"recover":    func() error { return runOrderRecover(c, io.Discard, "o-1", "", "", nil, false) },
		"redispatch": func() error { return runOrderRedispatch(c, io.Discard, "o-1", "", false) },
	} {
		err := run()
		if err == nil || err.Error() != "409 Conflict: "+refusal {
			t.Fatalf("%s err = %v, want 409 text verbatim", name, err)
		}
	}
	// main() prints `error: <err>` on stderr and exits 1; prove it end to end.
	stderr, code := runConveyorMain(t, c.base, "order", "recover", "o-1")
	if code != 1 || !strings.Contains(stderr, "409 Conflict: "+refusal) {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

// TestOrderRedispatchReportsPlanRevisionGateRefusal runs the real CLI entry
// point against the real route over a task at the gate: the command reports the
// route's refusal, exits non-zero, and claims no redispatch
// (component-work-orders invariants 39 and 44).
func TestOrderRedispatchReportsPlanRevisionGateRefusal(t *testing.T) {
	const eventCanary = "fake-redispatch-event-canary"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	pendingGate := func() (taskID, orderID string) {
		t.Helper()
		taskID = "plan-revision-cli-" + core.NewTaskID()
		orderID = taskID + "-implement-1"
		now := time.Now().UTC()
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "approved v1"})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.ApproveSpecVersion(ctx, taskID, plan.Version); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
		order := core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
		if _, err = storetest.For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
			t.Fatal(err)
		}
		claimed, err := storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "session", ClientToken: "fake-client-token", Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdRequestPlanRevision, func(lease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
			return st.RequestPlanRevisionCommand(ctx, lease, orderID, core.WorkOrderClaimIdentity{ClaimantID: claimed.ClaimantID, SessionID: claimed.SessionID}, "the API changed")
		}); err != nil {
			t.Fatal(err)
		}
		return taskID, orderID
	}
	server := httpapi.NewServer(st)
	server.Workspace, server.BearerToken = "demo", taskWaitTestToken
	dispatched := 0
	server.OnCreate = func(context.Context, string) { dispatched++ }
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	for _, test := range []struct {
		name    string
		refusal string
		corrupt bool
	}{
		{name: "pending", refusal: core.TaskRedispatchPlanRevisionRefusal},
		{name: "unreadable", refusal: core.TaskRedispatchPlanRevisionUnreadableRefusal, corrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			taskID, orderID := pendingGate()
			if test.corrupt {
				if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "task.state_changed", At: time.Now().UTC().Add(time.Second), Payload: json.RawMessage(`{"command":7,"detail":"` + eventCanary + `"}`)}); err != nil {
					t.Fatal(err)
				}
			}
			want := "409 Conflict: " + test.refusal
			stderr, code := runConveyorMain(t, httpServer.URL, "order", "redispatch", orderID)
			if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stderr, eventCanary) {
				t.Fatalf("exit=%d stderr=%q, want exit 1 with %q", code, stderr, want)
			}
			var out bytes.Buffer
			err := runOrderRedispatch(&client{base: httpServer.URL, token: taskWaitTestToken, workspace: "demo"}, &out, orderID, "", false)
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if out.Len() != 0 {
				t.Fatalf("refused command printed %q", out.String())
			}
			task, err := st.GetTask(ctx, taskID)
			if err != nil || task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanRevision {
				t.Fatalf("task after refusal = %+v err=%v", task, err)
			}
			if dispatched != 0 {
				t.Fatalf("refused redispatch dispatched %d tasks", dispatched)
			}
		})
	}
}

// runConveyorMain runs the real CLI entry point in a child process through
// TestTaskWaitProcessHelper and returns its stderr and exit status.
func runConveyorMain(t *testing.T, server string, args ...string) (string, int) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestTaskWaitProcessHelper$")
	command.Dir = t.TempDir()
	home := t.TempDir()
	args = append([]string{"--workspace", "demo"}, args...)
	command.Env = append(taskWaitChildEnvironment(),
		taskWaitHelperEnv+"=1", taskWaitHelperArgsEnv+"="+strings.Join(args, "\x1f"),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"CONVEYOR_ADDR="+server, strings.Join([]string{"CONVEYOR_API_TOKEN", taskWaitTestToken}, "="),
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stderr.String(), exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stderr.String(), 0
}

// Tests for conveyor order retire (component-work-orders "Recovery,
// redispatch, and preemption"; feature-operator-next-action NA-8).

func serveRetire(t *testing.T, listed, preempt string) (*client, func() []orderRequest) {
	t.Helper()
	return serveOrders(t, func(w http.ResponseWriter, r *http.Request) {
		if serveOrderReads(t, w, r, listed, false) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(preempt))
	})
}

func TestOrderRetireQueuedAndStale(t *testing.T) {
	for _, state := range []string{"queued", "stale"} {
		t.Run(state, func(t *testing.T) {
			preempt := `{"request_id":"r","work_order":{"id":"261011-aaaaaa-implement-2","task_id":"261011-aaaaaa","state":"cancelled"}}`
			c, seen := serveRetire(t, `[{"id":"261011-other-spec-1","state":"claimed"},{"id":"261011-aaaaaa-implement-2","task_id":"261011-aaaaaa","stage":"implement","state":"`+state+`"}]`, preempt)
			var out bytes.Buffer
			if err := runOrderRetire(c, &out, "261011-aaaaaa-implement-2", "task merged before review", "", false); err != nil {
				t.Fatal(err)
			}
			requests := seen()
			if len(requests) != 2 || requests[0].method != http.MethodGet || requests[0].path != "/v1/work-orders/261011-aaaaaa-implement-2" ||
				requests[1].method != http.MethodPost || requests[1].path != "/v1/work-orders/261011-aaaaaa-implement-2/preempt" {
				t.Fatalf("requests = %+v", requests)
			}
			id, _ := requests[1].body["request_id"].(string)
			if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) || requests[1].header.Get("X-Request-Id") != id || requests[1].header.Get("X-Idempotency-Key") != id {
				t.Fatalf("request id %q headers %v", id, requests[1].header)
			}
			if requests[1].body["reason"] != "task merged before review" || len(requests[1].body) != 2 {
				t.Fatalf("preempt body = %+v", requests[1].body)
			}
			want := "retired work order 261011-aaaaaa-implement-2: state cancelled (request " + id + ")\n"
			if out.String() != want {
				t.Fatalf("output %q, want %q", out.String(), want)
			}
		})
	}
}

// A review seat refuses before any POST: a round requires every seat it opened,
// so retiring one would keep it from settling (req-review-gates-evidence AC-2.6).
func TestOrderRetireRefusesReviewSeatBeforePosting(t *testing.T) {
	for _, state := range []string{"queued", "stale"} {
		t.Run(state, func(t *testing.T) {
			const id = "261011-aaaaaa-review-1-seat-1"
			c, seen := serveRetire(t, `[{"id":"`+id+`","task_id":"261011-aaaaaa","stage":"review","state":"`+state+`"}]`, `{}`)
			err := runOrderRetire(c, io.Discard, id, "seat is not needed", "", false)
			if err == nil || !strings.Contains(err.Error(), "is a review seat") {
				t.Fatalf("err=%v", err)
			}
			for _, request := range seen() {
				if request.method != http.MethodGet {
					t.Fatalf("retire sent %s %s for a review seat", request.method, request.path)
				}
			}
		})
	}
}

func TestOrderRetireRequestIDAndJSON(t *testing.T) {
	preempt := `{"request_id":"fixed","work_order":{"id":"o-1","state":"cancelled"}}`
	c, seen := serveRetire(t, `[{"id":"o-1","state":"queued"}]`, preempt)
	var out bytes.Buffer
	if err := runOrderRetire(c, &out, "o-1", "obsolete", "fixed", true); err != nil {
		t.Fatal(err)
	}
	post := seen()[1]
	if post.body["request_id"] != "fixed" || post.header.Get("X-Request-Id") != "fixed" || post.header.Get("X-Idempotency-Key") != "fixed" {
		t.Fatalf("--request-id not sent verbatim: %+v", post)
	}
	if out.String() != preempt+"\n" {
		t.Fatalf("--json output %q, want the server body", out.String())
	}
}

func TestOrderRetireRefusesBeforePosting(t *testing.T) {
	c, seen := serveRetire(t, `[{"id":"o-claimed","state":"claimed"},{"id":"o-timed","state":"timed_out"},{"id":"o-cancelled","state":"cancelled"},{"id":"o-queued","state":"queued"}]`, `{}`)
	for _, tc := range []struct {
		name, id, reason, want string
	}{
		{"claimed", "o-claimed", "obsolete", "work order o-claimed is claimed; retire refuses a live claim, stop its attempt with operator preemption instead"},
		{"unsupported state", "o-timed", "obsolete", "work order o-timed is timed_out; only queued or stale orders can be retired"},
		// A terminal order is outside the live list; the exact read still
		// names its state in the refusal.
		{"terminal state", "o-cancelled", "obsolete", "work order o-cancelled is cancelled; only queued or stale orders can be retired"},
		{"missing", "o-missing", "obsolete", "work order o-missing not found"},
		{"blank reason", "o-queued", "  ", "--reason is required"},
		{"blank id", " ", "obsolete", "order ID is required"},
	} {
		err := runOrderRetire(c, io.Discard, tc.id, tc.reason, "", false)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	for _, request := range seen() {
		if request.method != http.MethodGet {
			t.Fatalf("a refused retire posted: %+v", request)
		}
	}
}

func TestOrderRetireReportsNonRetiredResult(t *testing.T) {
	// The order was claimed between the read and the preempt, so the route
	// revoked the claim and requeued it; the CLI must not call that retired.
	c, _ := serveRetire(t, `[{"id":"o-1","state":"queued"}]`, `{"work_order":{"id":"o-1","state":"queued"},"revoked_session_id":"s"}`)
	var out bytes.Buffer
	err := runOrderRetire(c, &out, "o-1", "obsolete", "r-1", false)
	if err == nil || err.Error() != `work order o-1 was not retired: the server returned state "queued"` {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "work order o-1 returned state queued (request r-1)\n" || strings.Contains(out.String(), "retired work order") {
		t.Fatalf("output %q", out.String())
	}
}

func TestOrderRetireServerRefusalPassthrough(t *testing.T) {
	const refusal = "work order o-1 cannot be preempted"
	c, _ := serveOrders(t, func(w http.ResponseWriter, r *http.Request) {
		if serveOrderReads(t, w, r, `[{"id":"o-1","state":"stale"}]`, false) {
			return
		}
		http.Error(w, refusal, http.StatusConflict)
	})
	err := runOrderRetire(c, io.Discard, "o-1", "obsolete", "", false)
	if err == nil || err.Error() != "409 Conflict: "+refusal {
		t.Fatalf("err = %v, want 409 text verbatim", err)
	}
	stderr, code := runConveyorMain(t, c.base, "order", "retire", "o-1", "--reason", "obsolete")
	if code != 1 || !strings.Contains(stderr, "409 Conflict: "+refusal) {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}
