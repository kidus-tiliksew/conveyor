package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type taskMergeRequest struct {
	method, path, authorization, workspace string
}

const approvedDetail = `{"task":{"id":"task-1","state":"approved"}}`

const gateDetail = `{"task":{"id":"task-1","state":"awaiting_human","gate_kind":"merge_approval","reviewed_head_sha":"reviewed-head-sha"},"merge_readiness":{"state":"MERGEABLE","head_sha":"reviewed-head-sha","url":"https://github.test/org/repo/pull/7","number":7}}`

// runTaskMergeCommand serves detail on GET and post on POST.
func runTaskMergeCommand(t *testing.T, detail string, stdin string, args []string, post http.HandlerFunc) (string, error, []taskMergeRequest) {
	t.Helper()
	isolateLocalAuthTest(t)
	var requests []taskMergeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, taskMergeRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Workspace-ID")})
		if r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/task-1/activity" {
			_, _ = w.Write([]byte(detail))
			return
		}
		post(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "operator-secret-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	var output bytes.Buffer
	command := mergeTaskCmd()
	command.SetArgs(append([]string{"task-1"}, args...))
	command.SetIn(strings.NewReader(stdin))
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.Execute()
	return output.String(), err, requests
}

func posts(requests []taskMergeRequest) []taskMergeRequest {
	var out []taskMergeRequest
	for _, request := range requests {
		if request.method == http.MethodPost {
			out = append(out, request)
		}
	}
	return out
}

func TestTaskMergeCommandPostsExplicitMergeAndPrintsMergedState(t *testing.T) {
	output, err, requests := runTaskMergeCommand(t, approvedDetail, "", nil, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskMerged})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := taskMergeRequest{http.MethodPost, "/v1/tasks/task-1/merge", "Bearer operator-secret-token", "demo"}
	if sent := posts(requests); len(sent) != 1 || sent[0] != want {
		t.Fatalf("requests = %+v, want %+v", requests, want)
	}
	if output != "merged task task-1 (state merged)\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestTaskMergeCommandSurfacesConflictMessagesAndFails(t *testing.T) {
	for _, message := range []string{"task is not approved for merge", "merge was not confirmed by the forge"} {
		t.Run(message, func(t *testing.T) {
			output, err, requests := runTaskMergeCommand(t, approvedDetail, "", nil, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, message, http.StatusConflict)
			})
			if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), message) {
				t.Fatalf("err = %v, want the 409 message %q", err, message)
			}
			if sent := posts(requests); len(sent) != 1 || sent[0].path != "/v1/tasks/task-1/merge" {
				t.Fatalf("requests = %+v", requests)
			}
			if strings.Contains(output, "merged task") {
				t.Fatalf("refusal printed success: %q", output)
			}
		})
	}
}

func TestTaskMergeCommandRejectsUnconfirmedResponse(t *testing.T) {
	output, err, _ := runTaskMergeCommand(t, approvedDetail, "", nil, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskApproved})
	})
	if err == nil || !strings.Contains(err.Error(), `state "approved"`) {
		t.Fatalf("err = %v", err)
	}
	if output != "" {
		t.Fatalf("output = %q", output)
	}
}

// TestTaskMergeCommandAtGateConfirmsHeadAndPullRequest pins DEC-64(3): at the
// merge_approval gate the prompt names the reviewed head and pull request, a
// declined prompt sends nothing, and an accepted prompt or --yes sends one POST.
func TestTaskMergeCommandAtGateConfirmsHeadAndPullRequest(t *testing.T) {
	merged := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskMerged})
	}
	for _, tc := range []struct {
		name, stdin string
		args        []string
		wantPosts   int
		wantPrompt  bool
	}{
		{name: "declined", stdin: "no\n", wantPosts: 0, wantPrompt: true},
		{name: "accepted", stdin: "yes\n", wantPosts: 1, wantPrompt: true},
		{name: "yes flag", args: []string{"--yes"}, wantPosts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err, requests := runTaskMergeCommand(t, gateDetail, tc.stdin, tc.args, merged)
			if len(posts(requests)) != tc.wantPosts {
				t.Fatalf("requests = %+v", requests)
			}
			if !strings.Contains(output, "reviewed head reviewed-head-sha") || !strings.Contains(output, "pull request #7 https://github.test/org/repo/pull/7") {
				t.Fatalf("confirmation did not name head and PR: %q", output)
			}
			if strings.Contains(output, "Type yes") != tc.wantPrompt {
				t.Fatalf("prompt shown=%v output=%q", !tc.wantPrompt, output)
			}
			if tc.wantPosts == 0 {
				if err == nil || strings.Contains(output, "merged task") {
					t.Fatalf("declined merge err=%v output=%q", err, output)
				}
				return
			}
			if err != nil || !strings.Contains(output, "merged task task-1 (state merged)") {
				t.Fatalf("err=%v output=%q", err, output)
			}
		})
	}
}

// TestTaskMergeCommandAtGatePrintsReadinessRefusal pins the 409 answer: the
// readiness state is surfaced and success is never printed.
func TestTaskMergeCommandAtGatePrintsReadinessRefusal(t *testing.T) {
	output, err, _ := runTaskMergeCommand(t, gateDetail, "", []string{"--yes"}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"merge is not offered: readiness STALE","readiness":"STALE"}`))
	})
	if err == nil || !strings.Contains(err.Error(), "STALE") || strings.Contains(output, "merged task") {
		t.Fatalf("err=%v output=%q", err, output)
	}
}

// TestTaskMergeGateDecodesRealActivityEnvelope decodes the real handler's
// activity response, so the confirm names the served head, PR and readiness.
func TestTaskMergeGateDecodesRealActivityEnvelope(t *testing.T) {
	isolateLocalAuthTest(t)
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "task-1", Workspace: "demo", Repo: "app", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, ReviewedHeadSHA: "reviewed-head-sha"}); err != nil {
		t.Fatal(err)
	}
	server := httpapi.NewServer(st)
	server.Workspace, server.BearerToken = "demo", "operator-secret-token"
	server.OnMergeReadiness = func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
		return dispatch.MergeReadiness{State: "MERGEABLE", HeadSHA: "reviewed-head-sha", Number: 7, URL: "https://github.test/org/repo/pull/7"}, nil
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	t.Setenv("CONVEYOR_ADDR", httpServer.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "operator-secret-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	gate, err := newClient().taskMergeGate("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if gate.Task.GateKind != core.GateKindMergeApproval || gate.Task.ReviewedHeadSHA != "reviewed-head-sha" || gate.MergeReadiness == nil || gate.MergeReadiness.State != "MERGEABLE" || gate.MergeReadiness.Number != 7 {
		t.Fatalf("gate=%+v readiness=%+v", gate.Task, gate.MergeReadiness)
	}
}
