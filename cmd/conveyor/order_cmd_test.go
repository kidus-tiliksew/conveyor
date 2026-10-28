package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
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
	if err := runOrderRecover(c, &out, "261011-aaaaaa-implement-1", "", "", false); err != nil {
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

	if err := runOrderRecover(c, &out, "261011-aaaaaa-implement-1", "use the v2 schema", "x", false); err != nil {
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
		"recover":    func() error { return runOrderRecover(c, io.Discard, "o-1", "", "", false) },
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
			preempt := `{"request_id":"r","work_order":{"id":"261011-aaaaaa-review-1-seat-1","task_id":"261011-aaaaaa","state":"cancelled"}}`
			c, seen := serveRetire(t, `[{"id":"261011-other-spec-1","state":"claimed"},{"id":"261011-aaaaaa-review-1-seat-1","task_id":"261011-aaaaaa","state":"`+state+`"}]`, preempt)
			var out bytes.Buffer
			if err := runOrderRetire(c, &out, "261011-aaaaaa-review-1-seat-1", "task merged before review", "", false); err != nil {
				t.Fatal(err)
			}
			requests := seen()
			if len(requests) != 2 || requests[0].method != http.MethodGet || requests[0].path != "/v1/work-orders/261011-aaaaaa-review-1-seat-1" ||
				requests[1].method != http.MethodPost || requests[1].path != "/v1/work-orders/261011-aaaaaa-review-1-seat-1/preempt" {
				t.Fatalf("requests = %+v", requests)
			}
			id, _ := requests[1].body["request_id"].(string)
			if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) || requests[1].header.Get("X-Request-Id") != id || requests[1].header.Get("X-Idempotency-Key") != id {
				t.Fatalf("request id %q headers %v", id, requests[1].header)
			}
			if requests[1].body["reason"] != "task merged before review" || len(requests[1].body) != 2 {
				t.Fatalf("preempt body = %+v", requests[1].body)
			}
			want := "retired work order 261011-aaaaaa-review-1-seat-1: state cancelled (request " + id + ")\n"
			if out.String() != want {
				t.Fatalf("output %q, want %q", out.String(), want)
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
