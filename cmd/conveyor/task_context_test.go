package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Tests for conveyor task context confirm|dismiss and the task status mapping
// of task-context choices (req-operator-next-action AC-5.1, AC-5.5;
// feature-operator-next-action NA-8).

type contextRequest struct {
	method, escapedPath string
	workspace, auth     string
	body                []byte
}

func serveTaskContext(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*client, func() []contextRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []contextRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, contextRequest{
			method: r.Method, escapedPath: r.URL.EscapedPath(),
			workspace: r.Header.Get("X-Workspace-ID"), auth: r.Header.Get("Authorization"), body: raw,
		})
		mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL, token: "t", workspace: "demo"}, func() []contextRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]contextRequest(nil), seen...)
	}
}

func TestTaskContextDecisionPostsBodyless(t *testing.T) {
	for _, tc := range []struct{ action, kind, target, state, want string }{
		{"confirm", "requirement", "req-review-gates-evidence", "confirmed",
			"confirmed context requirement req-review-gates-evidence on task 261016-d8423c (state confirmed)\n"},
		{"dismiss", "system_design", "component-web-dashboard", "dismissed",
			"dismissed context system_design component-web-dashboard on task 261016-d8423c (state dismissed)\n"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			c, seen := serveTaskContext(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"task_id":"261016-d8423c","target_kind":"`+tc.kind+`","target_id":"`+tc.target+`","state":"`+tc.state+`"}`)
			})
			var out bytes.Buffer
			if err := runTaskContextDecision(c, &out, tc.action, "261016-d8423c", tc.kind, tc.target); err != nil {
				t.Fatal(err)
			}
			requests := seen()
			wantPath := "/v1/tasks/261016-d8423c/context/proposals/" + tc.kind + "/" + tc.target + "/" + tc.action
			if len(requests) != 1 || requests[0].method != http.MethodPost || requests[0].escapedPath != wantPath {
				t.Fatalf("requests = %+v, want one POST %s", requests, wantPath)
			}
			if requests[0].workspace != "demo" || requests[0].auth != "Bearer t" || len(requests[0].body) != 0 {
				t.Fatalf("workspace=%q auth=%q body=%q, want demo, the user credential, and no body", requests[0].workspace, requests[0].auth, requests[0].body)
			}
			if out.String() != tc.want {
				t.Fatalf("output = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestTaskContextDecisionEscapesEachSegment(t *testing.T) {
	c, seen := serveTaskContext(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"state":"confirmed"}`)
	})
	if err := runTaskContextDecision(c, io.Discard, "confirm", "261016-d8423c", "system_design", "a b/c%d:e"); err != nil {
		t.Fatal(err)
	}
	want := "/v1/tasks/261016-d8423c/context/proposals/system_design/a%20b%2Fc%25d:e/confirm"
	if got := seen()[0].escapedPath; got != want {
		t.Fatalf("escaped path = %q, want %q", got, want)
	}
}

func TestTaskContextDecisionSurfacesRefusal(t *testing.T) {
	c, _ := serveTaskContext(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"task_terminal","message":"task context proposals can only be decided while the task is open"}`)
	})
	var out bytes.Buffer
	err := runTaskContextDecision(c, &out, "confirm", "261016-d8423c", "requirement", "req-x")
	if err == nil {
		t.Fatal("refusal returned no error")
	}
	for _, want := range []string{"409", "task_terminal", "task context proposals can only be decided while the task is open"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not carry %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("a refusal printed a receipt: %q", out.String())
	}
}

func TestTaskContextDecisionRequiresCredential(t *testing.T) {
	c, seen := serveTaskContext(t, func(http.ResponseWriter, *http.Request) {})
	c.token = ""
	if err := runTaskContextDecision(c, io.Discard, "dismiss", "261016-d8423c", "requirement", "req-x"); err == nil || !strings.Contains(err.Error(), "conveyor auth login") {
		t.Fatalf("err = %v, want a sign-in hint", err)
	}
	if len(seen()) != 0 {
		t.Fatal("a request was sent without a credential")
	}
}

func TestTaskContextCommandsRegistered(t *testing.T) {
	command := taskContextCmd()
	for _, action := range []string{"confirm", "dismiss"} {
		sub, _, err := command.Find([]string{action})
		if err != nil || sub.Name() != action {
			t.Fatalf("task context %s not registered: %v", action, err)
		}
		if err := sub.Args(sub, []string{"261016-d8423c", "requirement"}); err == nil {
			t.Fatalf("task context %s accepted two arguments", action)
		}
	}
}

func TestTaskStatusPrintsTaskContextCommands(t *testing.T) {
	const task = taskStatusTestTask
	base := "/v1/tasks/" + task + "/context/proposals/"
	cases := []struct{ id, path, want string }{
		{"confirm:task_context:requirement:req-review-gates-evidence", base + "requirement/req-review-gates-evidence/confirm",
			"cli: conveyor task context confirm " + task + " requirement req-review-gates-evidence"},
		{"dismiss:task_context:system_design:component-web-dashboard", base + "system_design/component-web-dashboard/dismiss",
			"cli: conveyor task context dismiss " + task + " system_design component-web-dashboard"},
		// A target that needs path escaping is read back from the route and
		// shell-quoted, so the printed command reproduces it.
		{"confirm:task_context:system_design:a b/c'd", base + "system_design/a%20b%2Fc%27d/confirm",
			"cli: conveyor task context confirm " + task + ` system_design 'a b/c'\''d'`},
		// A route whose action disagrees with the id prints no command.
		{"confirm:task_context:requirement:req-x", base + "requirement/req-x/dismiss",
			"no CLI command; use the dashboard: POST " + base + "requirement/req-x/dismiss"},
		// A document-proposal choice keeps its version route.
		{"confirm:system_design:design-x:3", "/v1/system-designs/design-x/versions/3/confirm",
			"no CLI command; use the dashboard: POST /v1/system-designs/design-x/versions/3/confirm"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			action := nextActionView{
				Label: "Context proposal", Actor: "operator", Kind: "context_proposal", Summary: "S",
				Choices: []nextActionChoice{{ID: tc.id, Label: "Choice", Effect: "Effect.", Method: "POST", Path: tc.path, Allowed: true}},
			}
			var out bytes.Buffer
			if err := writeTaskStatus(&out, task, action); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "      "+tc.want+"\n") {
				t.Fatalf("want line %q in:\n%s", tc.want, out.String())
			}
		})
	}
}

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"req-review-gates-evidence": "req-review-gates-evidence",
		"system_design":             "system_design",
		"a b":                       "'a b'",
		"it's":                      `'it'\''s'`,
		"":                          "''",
		"$(rm)":                     "'$(rm)'",
	} {
		if got := shellWord(in); got != want {
			t.Fatalf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}
