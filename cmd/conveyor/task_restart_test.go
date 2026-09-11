package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/spf13/cobra"
)

type restartTransport func(*http.Request) (*http.Response, error)

func (f restartTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type restartFixture struct {
	client      *client
	requests    []core.TaskStartOverRequest
	reads       int
	failPath    string
	failStatus  int
	forgeBody   string
	forgeStatus int
	empty       bool
	task        core.Task
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	f := &restartFixture{forgeBody: `[{"number":7,"html_url":"https://github.com/acme/app/pull/7","head":{"ref":"conveyor/task-old","sha":"head"},"base":{"ref":"main","sha":"base"}}]`, task: core.Task{ID: "old", Title: "Repair preview", Repo: "app", Branch: "conveyor/task-old", Workspace: "demo"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reads++
		if r.Header.Get("Authorization") != "Bearer api-token" || r.Header.Get("X-Workspace-ID") != "demo" {
			t.Errorf("missing API authentication/workspace")
		}
		if r.URL.Path == f.failPath {
			http.Error(w, "server refused the operation", f.failStatus)
			return
		}
		switch r.URL.Path {
		case "/v1/tasks/old":
			json.NewEncoder(w).Encode(f.task)
		case "/v1/tasks/old/jobs":
			io.WriteString(w, `[]`)
		case "/v1/tasks/old/spec":
			http.NotFound(w, r)
		case "/v1/tasks/old/activity":
			if f.empty {
				io.WriteString(w, `{}`)
				return
			}
			io.WriteString(w, `{"work_orders":[{"id":"queued","state":"queued","stage":"implement"},{"id":"claimed","state":"claimed"},{"id":"submitted","state":"submitted"},{"id":"stale","state":"stale"},{"id":"timed-out","state":"timed_out"},{"id":"finished-order","state":"completed"},{"id":"cancelled-order","state":"cancelled"}],"events":[{"kind":"pull_request.opened","payload":{"number":7,"url":"https://github.com/acme/app/pull/7"}}]}`)
		case "/v1/pending-proposals":
			if f.empty {
				io.WriteString(w, `{}`)
				return
			}
			io.WriteString(w, `{"items":[{"id":"design-one","title":"Runtime","tier":"system_design","version":2,"origin_type":"task","origin_id":"old"},{"id":"other-task-proposal","origin_type":"task","origin_id":"another"},{"id":"operator-proposal","origin_type":"operator","origin_id":"old"}]}`)
		case "/v1/workspace/config":
			io.WriteString(w, `{"document":{"repos":[{"name":"app","github":"acme/app"}]}}`)
		case "/v1/tasks/old/restart":
			if r.Method != http.MethodPost {
				t.Errorf("method %s", r.Method)
			}
			var request core.TaskStartOverRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			f.requests = append(f.requests, request)
			json.NewEncoder(w).Encode(core.TaskStartOverResult{Successor: core.Task{ID: "next", Branch: "conveyor/task-next", Workspace: "demo"}, Created: len(f.requests) == 1})
		default:
			t.Errorf("unexpected API read %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	original := http.DefaultClient
	oldTransport := original.Transport
	original.Transport = restartTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.github.com" {
			if oldTransport != nil {
				return oldTransport.RoundTrip(r)
			}
			return http.DefaultTransport.RoundTrip(r)
		}
		if r.Header.Get("Authorization") != "Bearer forge-token" {
			t.Errorf("missing local forge credential")
		}
		if r.URL.Path != "/repos/acme/app/pulls" || r.URL.Query().Get("head") != "acme:conveyor/task-old" || r.URL.Query().Get("state") != "open" {
			t.Errorf("forge request %s", r.URL)
		}
		status := f.forgeStatus
		if status == 0 {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(f.forgeBody)), Request: r}, nil
	})
	t.Cleanup(func() { original.Transport = oldTransport })

	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "api-token")
	t.Setenv("CONVEYOR_GIT_TOKEN", "forge-token")
	oldWorkspace := workspaceFlag
	workspaceFlag = "demo"
	t.Cleanup(func() { workspaceFlag = oldWorkspace })
	f.client = &client{base: server.URL, token: "api-token", workspace: "demo"}
	return f
}

func executeRestart(args ...string) (string, error) {
	command := restartTaskCmd()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader("y\n"))
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

func TestTaskRestartPreviewAndRetry(t *testing.T) {
	f := newRestartFixture(t)
	output, err := executeRestart("old", "--reason", "try again", "--note", "keep context", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Repair preview", "queued (implement, queued)", "claimed", "submitted", "stale", "timed-out", "design-one v2", "Recorded pull request: #7", "Open pull request to close: #7", "Successor: next", "Branch: conveyor/task-next", "conveyor --workspace demo run next"} {
		if !strings.Contains(output, want) {
			t.Errorf("missing %q in %s", want, output)
		}
	}
	for _, unwanted := range []string{"finished-order", "cancelled-order", "other-task-proposal", "operator-proposal"} {
		if strings.Contains(output, unwanted) {
			t.Errorf("unexpected %s", unwanted)
		}
	}
	if len(f.requests) != 1 {
		t.Fatalf("requests=%v", f.requests)
	}
	first := f.requests[0]
	if len(first.RequestID) != 32 || !strings.Contains(output, first.RequestID) || first.Reason != "try again" || first.Note != "keep context" {
		t.Fatalf("request=%+v", first)
	}
	_, err = executeRestart("old", "--reason", first.Reason, "--note", first.Note, "--request-id", first.RequestID, "--yes")
	if err != nil || len(f.requests) != 2 || f.requests[1] != first {
		t.Fatalf("retry=%v requests=%v", err, f.requests)
	}
}

func TestTaskRestartConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, input            string
		terminal, yes, success bool
	}{
		{"confirm", "y\n", true, false, true}, {"decline", "n\n", true, false, false}, {"full word", "yes\n", true, false, false}, {"empty", "", true, false, false}, {"pipe", "y\n", false, false, false}, {"yes flag", "", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			command := &cobra.Command{}
			command.SetContext(t.Context())
			var out bytes.Buffer
			command.SetOut(&out)
			command.SetIn(strings.NewReader(tc.input))
			err := runTaskRestart(command, f.client, core.TaskStartOverRequest{TaskID: "old", RequestID: "stable", Reason: "again"}, tc.yes, tc.terminal)
			if (err == nil) != tc.success || (len(f.requests) == 1) != tc.success {
				t.Fatalf("error=%v requests=%v", err, f.requests)
			}
			if !tc.terminal && !tc.yes && (err == nil || !strings.Contains(err.Error(), "stdin is not a terminal")) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestTaskRestartRefusals(t *testing.T) {
	for _, tc := range []struct {
		path   string
		status int
	}{{"/v1/tasks/old", 404}, {"/v1/tasks/old/activity", 500}, {"/v1/pending-proposals", 403}, {"/v1/workspace/config", 403}, {"/v1/tasks/old/restart", 409}, {"/v1/tasks/old/restart", 403}, {"/v1/tasks/old/restart", 400}} {
		t.Run(fmt.Sprintf("%s-%d", tc.path, tc.status), func(t *testing.T) {
			f := newRestartFixture(t)
			f.failPath = tc.path
			f.failStatus = tc.status
			output, err := executeRestart("old", "--reason", "again", "--yes", "--request-id", "retry-key")
			if err == nil || !strings.Contains(err.Error(), "server refused the operation") || len(f.requests) != 0 || !strings.Contains(output, "retry-key") {
				t.Fatalf("error=%v output=%s requests=%v", err, output, f.requests)
			}
		})
	}
}

func TestTaskRestartUnknownAndEmptyPR(t *testing.T) {
	for _, tc := range []struct {
		body    string
		status  int
		success bool
	}{{`[]`, 200, true}, {`null`, 200, false}, {`broken`, 200, false}, {`{"message":"unavailable"}`, 503, false}, {`[{"number":7}]`, 200, false}} {
		t.Run(tc.body, func(t *testing.T) {
			f := newRestartFixture(t)
			f.forgeBody = tc.body
			f.forgeStatus = tc.status
			f.empty = true
			output, err := executeRestart("old", "--reason", "again", "--yes")
			if (err == nil) != tc.success || (len(f.requests) == 1) != tc.success {
				t.Fatalf("error=%v output=%s", err, output)
			}
			want := "Open pull request: unknown"
			if tc.success {
				want = "Open pull request: none"
			}
			if !strings.Contains(output, want) {
				t.Fatalf("output=%s", output)
			}
		})
	}
}

func TestTaskRestartInvalidInputMakesNoReads(t *testing.T) {
	for _, args := range [][]string{{"../old", "--reason", "again"}, {"old"}, {"old", "--reason", " "}, {"old", "--reason", strings.Repeat("x", 201)}, {"old", "--reason", "again", "--note", "", "--note-file", "x"}, {"old", "--reason", "again", "--note", strings.Repeat("ሀ", 2001)}, {"old", "--reason", "again", "--request-id", " "}, {"old", "--reason", "again", "--request-id", strings.Repeat("x", 201)}, {"old", "--reason", "again", "--note-file", "/missing-note-file"}} {
		t.Run(strings.Join(args[:1], ""), func(t *testing.T) {
			f := newRestartFixture(t)
			_, err := executeRestart(args...)
			if err == nil || f.reads != 0 {
				t.Fatalf("error=%v reads=%d", err, f.reads)
			}
		})
	}
}

func TestTaskRestartNoteFileAndNonTerminal(t *testing.T) {
	f := newRestartFixture(t)
	path := filepath.Join(t.TempDir(), "note.txt")
	note := "First line\nSecond line\n"
	if err := os.WriteFile(path, []byte(note), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := executeRestart("old", "--reason", "again", "--note-file", path, "--yes")
	if err != nil || len(f.requests) != 1 || f.requests[0].Note != note {
		t.Fatalf("error=%v requests=%v", err, f.requests)
	}
	_, err = executeRestart("old", "--reason", "again")
	if err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") || len(f.requests) != 1 {
		t.Fatalf("error=%v requests=%v", err, f.requests)
	}
}

func TestTaskShowSupersession(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprint(successor), func(t *testing.T) {
			f := newRestartFixture(t)
			want := "Started over as next"
			f.task.SupersededBy = "next"
			if successor {
				f.task.SupersededBy = ""
				f.task.Supersedes = "previous"
				f.task.IntakeOperatorDirection = "Start-over reason: retry\nOperator note: context"
				want = "Restarted from previous"
			}
			command := taskCmd()
			var output, diagnostics bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&diagnostics)
			command.SetArgs([]string{"show", "old"})
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(output.Bytes()) || !strings.Contains(diagnostics.String(), want) {
				t.Fatalf("stdout=%s stderr=%s", output.String(), diagnostics.String())
			}
			if successor && !strings.Contains(diagnostics.String(), f.task.IntakeOperatorDirection) {
				t.Fatal("missing operator reason/note")
			}
		})
	}
}

func TestTaskRestartLocalCredentials(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			f := newRestartFixture(t)
			t.Setenv("CONVEYOR_GIT_TOKEN", "")
			t.Setenv(gitAskPassModeEnv, "")
			t.Setenv(gitAskPassTokenEnv, "")
			bin := t.TempDir()
			script := "#!/bin/sh\n[ \"$1 $2\" = 'credential fill' ] || exit 2\n[ \"$GIT_TERMINAL_PROMPT\" = 0 ] || exit 3\ncat >/dev/null\nprintf 'password=forge-token\\n'\n"
			if !available {
				script = "#!/bin/sh\necho forge-token >&2\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := executeRestart("old", "--reason", "again", "--yes")
			if available {
				if err != nil || len(f.requests) != 1 {
					t.Fatalf("error=%v", err)
				}
			} else {
				if err == nil || len(f.requests) != 0 || !strings.Contains(output, "Open pull request: unknown") || strings.Contains(err.Error(), "forge-token") {
					t.Fatalf("error=%v output=%s", err, output)
				}
			}
		})
	}
}

func TestTaskRestartRecordedPRIsNotCurrentState(t *testing.T) {
	f := newRestartFixture(t)
	f.forgeBody = `[]`
	output, err := executeRestart("old", "--reason", "again", "--yes")
	if err != nil || !strings.Contains(output, "Recorded pull request: #7") || !strings.Contains(output, "Open pull request: none") {
		t.Fatalf("error=%v output=%s", err, output)
	}
}

func TestTaskRestartCharacterDeviceIsNotTerminal(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if restartInputIsTerminal(file) {
		t.Fatal("/dev/null is not an interactive terminal")
	}
}
