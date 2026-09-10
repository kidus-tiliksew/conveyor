package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type submitTransport struct {
	target    *url.URL
	transport http.RoundTripper
}

func (s submitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	u := *r.URL
	clone.URL = &u
	if u.Host == "api.github.com" {
		clone.URL.Scheme = s.target.Scheme
		clone.URL.Host = s.target.Host
	}
	return s.transport.RoundTrip(clone)
}

func TestSubmitTaskPushCreateReuseAndRedaction(t *testing.T) {
	directory := t.TempDir()
	origin := filepath.Join(directory, "origin.git")
	primary := filepath.Join(directory, "primary")
	taskdir := filepath.Join(directory, "task")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(directory, "init", "--bare", origin)
	git(directory, "init", "-b", "main", primary)
	git(primary, "config", "user.name", "Test")
	git(primary, "config", "user.email", "test@example.com")
	git(primary, "commit", "--allow-empty", "-m", "base")
	git(primary, "remote", "add", "origin", "file://"+origin)
	git(primary, "push", "origin", "main")
	git(primary, "worktree", "add", "-b", "conveyor/task-one", taskdir, "main")
	git(taskdir, "commit", "--allow-empty", "-m", "task")
	head := git(taskdir, "rev-parse", "HEAD")
	// A host credential helper supplies the same credential to Git and the API.
	secret := "local-secret<&>"
	helper := filepath.Join(directory, "credential-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' 'username=test' 'password="+secret+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git(primary, "config", "credential.helper", helper)
	t.Setenv(localGitTokenEnv, "")
	t.Setenv(gitAskPassModeEnv, "")
	t.Setenv(gitAskPassTokenEnv, "")
	t.Setenv("GIT_CONFIG_COUNT", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	exists, failSubmit, failPR := false, true, false
	creates, submits := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pull-request-template"):
			if r.URL.Query().Get("session_id") != "session" || r.URL.Query().Get("workspace_id") != "demo" {
				t.Errorf("template query %s", r.URL)
			}
			json.NewEncoder(w).Encode(workorder.PullRequestTemplate{TaskID: "one", Repository: "acme/app", RepositoryURL: "file://" + origin, Branch: "conveyor/task-one", Base: "main", Title: "Task", Body: "Task body"})
		case strings.HasSuffix(r.URL.Path, "/submit-for-review"):
			submits++
			var request map[string]string
			json.NewDecoder(r.Body).Decode(&request)
			if request["head_sha"] != head || git(origin, "rev-parse", "refs/heads/conveyor/task-one") != head {
				t.Error("submission did not name pushed head")
			}
			if failSubmit {
				http.Error(w, "retry submission", 409)
				return
			}
			fmt.Fprint(w, `{"pr_url":"https://github.com/acme/app/pull/7","await_review":true}`)
		case r.URL.Path == "/repos/acme/app/pulls":
			if r.Header.Get("Authorization") != "Bearer "+secret {
				t.Errorf("wrong local credential")
			}
			if failPR {
				encoded, _ := json.Marshal(secret)
				w.WriteHeader(http.StatusUnprocessableEntity)
				json.NewEncoder(w).Encode(map[string]string{"message": secret + " " + base64.StdEncoding.EncodeToString([]byte(secret)) + " " + string(encoded)})
				return
			}
			if r.Method == http.MethodPost {
				creates++
				exists = true
				fmt.Fprint(w, `{"html_url":"https://github.com/acme/app/pull/7"}`)
				return
			}
			if !exists {
				fmt.Fprint(w, `[]`)
				return
			}
			fmt.Fprintf(w, `[{"number":7,"html_url":"https://github.com/acme/app/pull/7","head":{"sha":%q,"ref":"conveyor/task-one"},"base":{"sha":"base-sha","ref":"main"}}]`, head)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = submitTransport{target: target, transport: http.DefaultTransport}
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
	c, err := (&client{base: server.URL, token: "conveyor-bearer", workspace: "demo"}).withLocalGitCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.submitTask(context.Background(), "one", "order", "session", taskdir); err == nil || !strings.Contains(err.Error(), "retry submission") {
		t.Fatalf("first err=%v", err)
	}
	failSubmit = false
	result, err := c.submitTask(t.Context(), "one", "order", "session", taskdir)
	if err != nil || result["pr_url"] == nil || creates != 1 || submits != 2 {
		t.Fatalf("retry result=%v err=%v creates=%d submits=%d", result, err, creates, submits)
	}
	if git(taskdir, "config", "branch.conveyor/task-one.remote") != "origin" || git(taskdir, "config", "branch.conveyor/task-one.merge") != "refs/heads/conveyor/task-one" {
		t.Fatal("task branch has no upstream tracking")
	}
	failPR = true
	_, err = c.submitTask(t.Context(), "one", "order", "session", taskdir)
	jsonSecret, _ := json.Marshal(secret)
	if err == nil || strings.Contains(err.Error(), string(jsonSecret[1:len(jsonSecret)-1])) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(secret))) || !strings.Contains(err.Error(), submitCredentialRemedy) {
		t.Fatalf("unsafe/unhelpful error=%v", err)
	}
	if git(origin, "rev-parse", "refs/heads/conveyor/task-one") != head {
		t.Fatal("failure changed pushed work")
	}
	failPR = false
	hook := filepath.Join(origin, "hooks", "pre-receive")
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s\\n' '"+secret+" "+encoded+"' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git(taskdir, "commit", "--allow-empty", "-m", "next")
	_, err = c.submitTask(t.Context(), "one", "order", "session", taskdir)
	if err == nil || !strings.Contains(err.Error(), "push task branch") || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), encoded) || !strings.Contains(err.Error(), submitCredentialRemedy) {
		t.Fatalf("push failure error=%v", err)
	}
	if git(origin, "rev-parse", "refs/heads/conveyor/task-one") != head {
		t.Fatal("refused push changed remote head")
	}
	if _, err = c.submitTask(t.Context(), "one", "order", "session", primary); err == nil || !strings.Contains(err.Error(), "dedicated task worktree") {
		t.Fatalf("primary err=%v", err)
	}
}
