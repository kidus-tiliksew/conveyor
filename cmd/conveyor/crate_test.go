package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// crateServerCall is one request the test server received.
type crateServerCall struct {
	method, path, query, workspace, auth string
	body                                 map[string]any
	rawBody                              string
}

// crateTestServer answers each route with a fixed body and records the
// requests, so a test sees exactly what a command sent.
type crateTestServer struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []crateServerCall
	status  int
	reply   string
	replyOf map[string]string
}

func newCrateTestServer(t *testing.T) *crateTestServer {
	t.Helper()
	s := &crateTestServer{status: http.StatusOK, replyOf: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := crateServerCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, workspace: r.Header.Get("X-Workspace-ID"), auth: r.Header.Get("Authorization"), rawBody: string(raw)}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &call.body); err != nil {
				t.Errorf("request body %q: %v", raw, err)
			}
		}
		s.mu.Lock()
		s.calls = append(s.calls, call)
		status, reply := s.status, s.reply
		if specific, ok := s.replyOf[r.Method+" "+r.URL.Path]; ok {
			reply = specific
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *crateTestServer) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := crateCmdWith(func() *client { return &client{base: s.URL, token: "crate-token", workspace: "alpha"} })
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(io.Discard)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

func (s *crateTestServer) only(t *testing.T) crateServerCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 {
		t.Fatalf("calls = %+v, want one", s.calls)
	}
	call := s.calls[0]
	s.calls = nil
	return call
}

const crateRowBody = `{"id":"crate-261118-abcdef","name":"Spring release","target_date":"2026-12-01","state":"open","shipped":false,"created_by":"user:usr_1","created_at":"2026-11-18T10:00:00Z","closed_at":null,"open_task_count":2,"merged_task_count":1,"total_task_count":3}`

// TestCrateCommandsCallTheirRoutes: each crate command sends one request to
// the route that holds its rule, with the workspace and credential, and the
// payload its flags state. --json prints the route's response
// (component-crates invariant 15; req-crates AC-6.3).
func TestCrateCommandsCallTheirRoutes(t *testing.T) {
	taskBody := `{"id":"261118-aaaaaa","title":"A task","state":"queued","crate":{"id":"crate-261118-abcdef","name":"Spring release"}}`
	for _, tc := range []struct {
		name   string
		args   []string
		reply  string
		method string
		path   string
		query  string
		body   string
		text   []string
	}{
		{"list", []string{"list"}, "[" + crateRowBody + "]", http.MethodGet, "/v1/crates", "", "", []string{"crate-261118-abcdef", "open", "2026-12-01", "2 open, 1 merged, 3 total", "Spring release", "1 crates"}},
		{"list closed", []string{"list", "--state", "closed"}, "[]", http.MethodGet, "/v1/crates", "state=closed", "", []string{"0 crates"}},
		{"list all", []string{"list", "--state", "all"}, "[]", http.MethodGet, "/v1/crates", "state=all", "", []string{"0 crates"}},
		{"show", []string{"show", "crate-261118-abcdef"}, `{"id":"crate-261118-abcdef","name":"Spring release","target_date":null,"state":"open","shipped":true,"open_task_count":0,"merged_task_count":1,"total_task_count":1,"requirements":[{"id":"req-a","title":"Req A","task_count":0}],"undeclared_requirements":[{"id":"req-b","title":"Req B","task_count":1}],"tasks":[{"id":"261118-aaaaaa","title":"A task","state":"merged","branch":"b","pull_request":null,"next_action":{"label":"Merged"}}]}`, http.MethodGet, "/v1/crates/crate-261118-abcdef", "", "", []string{"no target date", "Shipped", "req-a  0 tasks  Req A", "does not declare", "req-b  1 tasks  Req B", "261118-aaaaaa  merged  A task  Merged"}},
		{"create", []string{"create", "--name", "Spring release", "--target-date", "2026-12-01", "--requirement", "req-a", "--requirement", "req-b"}, crateRowBody, http.MethodPost, "/v1/crates", "", `{"name":"Spring release","requirement_ids":["req-a","req-b"],"target_date":"2026-12-01"}`, []string{"Created crate crate-261118-abcdef."}},
		{"create with name only", []string{"create", "--name", "Plain"}, crateRowBody, http.MethodPost, "/v1/crates", "", `{"name":"Plain"}`, []string{"Created crate"}},
		{"edit name", []string{"edit", "crate-261118-abcdef", "--name", "Summer"}, crateRowBody, http.MethodPatch, "/v1/crates/crate-261118-abcdef", "", `{"name":"Summer"}`, []string{"Changed crate crate-261118-abcdef."}},
		{"edit date and list", []string{"edit", "crate-261118-abcdef", "--target-date", "2027-01-15", "--requirement", "req-c"}, crateRowBody, http.MethodPatch, "/v1/crates/crate-261118-abcdef", "", `{"requirement_ids":["req-c"],"target_date":"2027-01-15"}`, []string{"Changed crate"}},
		{"edit clears", []string{"edit", "crate-261118-abcdef", "--target-date", "", "--requirement", ""}, crateRowBody, http.MethodPatch, "/v1/crates/crate-261118-abcdef", "", `{"requirement_ids":[],"target_date":null}`, []string{"Changed crate"}},
		{"edit nothing", []string{"edit", "crate-261118-abcdef"}, crateRowBody, http.MethodPatch, "/v1/crates/crate-261118-abcdef", "", `{}`, []string{"Changed crate"}},
		{"close", []string{"close", "crate-261118-abcdef"}, crateRowBody, http.MethodPatch, "/v1/crates/crate-261118-abcdef", "", `{"state":"closed"}`, []string{"Closed crate crate-261118-abcdef."}},
		{"reopen", []string{"reopen", "crate-261118-abcdef"}, crateRowBody, http.MethodPatch, "/v1/crates/crate-261118-abcdef", "", `{"state":"open"}`, []string{"Reopened crate crate-261118-abcdef."}},
		{"add", []string{"add", "261118-aaaaaa", "crate-261118-abcdef"}, taskBody, http.MethodPut, "/v1/tasks/261118-aaaaaa/crate", "", `{"crate_id":"crate-261118-abcdef"}`, []string{"Task 261118-aaaaaa is in crate crate-261118-abcdef (Spring release)."}},
		{"remove", []string{"remove", "261118-aaaaaa"}, `{"id":"261118-aaaaaa","title":"A task","state":"queued"}`, http.MethodDelete, "/v1/tasks/261118-aaaaaa/crate", "", "", []string{"Task 261118-aaaaaa is in no crate."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newCrateTestServer(t)
			server.reply = tc.reply
			out, err := server.run(t, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			call := server.only(t)
			if call.method != tc.method || call.path != tc.path || call.query != tc.query {
				t.Fatalf("request = %s %s?%s, want %s %s?%s", call.method, call.path, call.query, tc.method, tc.path, tc.query)
			}
			if call.workspace != "alpha" || call.auth != "Bearer crate-token" {
				t.Fatalf("workspace %q, authorization %q", call.workspace, call.auth)
			}
			if tc.body == "" {
				if call.rawBody != "" {
					t.Fatalf("body = %s, want none", call.rawBody)
				}
			} else {
				var want map[string]any
				if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
					t.Fatal(err)
				}
				got, _ := json.Marshal(call.body)
				wantJSON, _ := json.Marshal(want)
				if string(got) != string(wantJSON) {
					t.Fatalf("body = %s, want %s", got, wantJSON)
				}
			}
			for _, want := range tc.text {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}

			// --json prints the route's response body as sent.
			jsonOut, err := server.run(t, append(append([]string{}, tc.args...), "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			server.only(t)
			if jsonOut != tc.reply+"\n" {
				t.Fatalf("--json output = %q, want %q", jsonOut, tc.reply+"\n")
			}
		})
	}
}

// TestCrateCommandsReportRouteRefusals: a refused request returns the route's
// status and message as the command's error and prints nothing.
func TestCrateCommandsReportRouteRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		status int
		reply  string
	}{
		{"closed crate", []string{"add", "261118-aaaaaa", "crate-261118-abcdef"}, http.StatusConflict, `{"error":"crate_closed","crate_id":"crate-261118-abcdef","message":"crate crate-261118-abcdef is closed, so no task can join it"}`},
		{"missing crate", []string{"show", "crate-000000-000000"}, http.StatusNotFound, `{"error":"crate_not_found","crate_id":"crate-000000-000000","message":"crate crate-000000-000000 was not found"}`},
		{"empty name", []string{"create", "--name", " "}, http.StatusBadRequest, `{"error":"invalid_crate","field":"name","message":"name must not be empty"}`},
		{"no capability", []string{"close", "crate-261118-abcdef"}, http.StatusForbidden, `{"error":"forbidden"}`},
		{"bad state", []string{"list", "--state", "shipped", "--json"}, http.StatusBadRequest, `{"error":"invalid_crate","field":"state","message":"state must be open, closed, or all"}`},
		{"unknown requirement", []string{"edit", "crate-261118-abcdef", "--requirement", "req-missing"}, http.StatusBadRequest, `{"error":"invalid_context_reference","requirement":"req-missing"}`},
		{"remove missing task", []string{"remove", "261118-ffffff"}, http.StatusNotFound, `{"error":"task_not_found","message":"The task was not found."}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newCrateTestServer(t)
			server.status, server.reply = tc.status, tc.reply
			out, err := server.run(t, tc.args...)
			if err == nil {
				t.Fatalf("command succeeded with output %q", out)
			}
			if !strings.Contains(err.Error(), tc.reply) || !strings.Contains(err.Error(), http.StatusText(tc.status)) {
				t.Fatalf("error = %q, want the route's %d answer %s", err, tc.status, tc.reply)
			}
			if out != "" {
				t.Fatalf("a refused command printed %q", out)
			}
			server.only(t)
		})
	}
}

// TestCrateCommandIsRegistered: the root command has the crate group with
// its eight commands.
func TestCrateCommandIsRegistered(t *testing.T) {
	root := newRootCmd()
	command, _, err := root.Find([]string{"crate"})
	if err != nil || command.Name() != "crate" {
		t.Fatalf("crate command: %v", err)
	}
	var names []string
	for _, sub := range command.Commands() {
		names = append(names, sub.Name())
	}
	want := []string{"add", "close", "create", "edit", "list", "remove", "reopen", "show"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("crate commands = %v, want %v", names, want)
	}
}
