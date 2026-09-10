package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRESTRunnerUsesOnlyExplicitToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer explicit-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Fatalf("X-GitHub-Api-Version = %q", got)
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer server.Close()

	run := newRESTRunner(server.Client(), server.URL, "explicit-secret", "workspace demo forge token")
	raw, err := run(t.Context(), "api", "user")
	if err != nil || string(raw) != `{"login":"octocat"}` {
		t.Fatalf("run = %s, %v", raw, err)
	}
}

func TestRESTRunnerMapsPermissionWithoutLeakingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	const token = "revoked-secret"
	run := newRESTRunner(server.Client(), server.URL, token, "workspace demo forge token")
	_, err := run(t.Context(), "api", "user")
	if ErrorCategory(err) != ForgePermission {
		t.Fatalf("category = %q, error = %v", ErrorCategory(err), err)
	}
	if !strings.Contains(err.Error(), "workspace demo forge token") || !strings.Contains(err.Error(), "replace it in settings") {
		t.Fatalf("permission remedy = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("permission error leaked token: %v", err)
	}
}

func TestRESTRunnerMapsRateLimitAndMalformedResponse(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"rate limit exceeded"}`, http.StatusForbidden)
		}))
		defer server.Close()
		run := newRESTRunner(server.Client(), server.URL, "token", "user usr-1 forge token")
		_, err := run(t.Context(), "api", "user")
		if ErrorCategory(err) != ForgeRateLimited {
			t.Fatalf("category = %q, error = %v", ErrorCategory(err), err)
		}
	})

	t.Run("malformed issue listing", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"not":"an array"}`))
		}))
		defer server.Close()
		run := newRESTRunner(server.Client(), server.URL, "token", "workspace demo forge token")
		_, err := run(t.Context(), "issue", "list", "--repo", "acme/app", "--label", ReadyLabel, "--state", "open")
		if ErrorCategory(err) != ForgeResponse {
			t.Fatalf("category = %q, error = %v", ErrorCategory(err), err)
		}
	})
}

func TestRESTRunnerMapsTransportAndStatusFailuresWithoutTokenLeakage(t *testing.T) {
	const token = "explicit-secret"
	t.Run("transport", func(t *testing.T) {
		transportErr := errors.New("transport rejected " + token)
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})}
		run := newRESTRunner(client, "https://github.test", token, "user usr-1 forge token")
		_, err := run(t.Context(), "api", "user")
		if ErrorCategory(err) != ForgeRequest || !errors.Is(err, transportErr) || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatalf("transport error = %v, category = %q", err, ErrorCategory(err))
		}
	})

	t.Run("status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"upstream echoed `+token+`"}`)
		}))
		defer server.Close()
		run := newRESTRunner(server.Client(), server.URL, token, "workspace demo forge token")
		_, err := run(t.Context(), "api", "user")
		if ErrorCategory(err) != ForgeStatus || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatalf("status error = %v, category = %q", err, ErrorCategory(err))
		}
	})
}

func TestRESTRunnerPaginatesReadyIssues(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer workspace-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if requests == 1 {
			if got := r.URL.Query().Get("labels"); got != ReadyLabel {
				t.Fatalf("labels = %q", got)
			}
			w.Header().Set("Link", "<"+serverURL(r)+"?page=2>; rel=\"next\"")
			_, _ = io.WriteString(w, `[{"number":1,"title":"one","body":"first","html_url":"https://github.test/issues/1"}]`)
			return
		}
		_, _ = io.WriteString(w, `[{"number":2,"title":"two","body":"second","html_url":"https://github.test/issues/2"}]`)
	}))
	defer server.Close()

	run := newRESTRunner(server.Client(), server.URL, "workspace-token", "workspace demo forge token")
	raw, err := run(t.Context(), "issue", "list", "--repo", "acme/app", "--label", ReadyLabel, "--state", "open")
	if err != nil {
		t.Fatal(err)
	}
	var issues []Issue
	if err = json.Unmarshal(raw, &issues); err != nil {
		t.Fatal(err)
	}
	want := []Issue{{Number: 1, Title: "one", Body: "first", URL: "https://github.test/issues/1"}, {Number: 2, Title: "two", Body: "second", URL: "https://github.test/issues/2"}}
	if !reflect.DeepEqual(issues, want) || requests != 2 {
		t.Fatalf("issues = %+v, requests = %d", issues, requests)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host + r.URL.Path
}

func TestRESTRunnerPreservesUnifiedDiffAcceptHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.RawQuery, "head=acme%3Atask-branch"):
			_, _ = w.Write([]byte(`[{"number":7}]`))
		case r.URL.Path == "/repos/acme/app/pulls/7" && r.Header.Get("Accept") == "application/vnd.github.v3.diff":
			_, _ = w.Write([]byte("diff --git a/a b/a\n"))
		case r.URL.Path == "/repos/acme/app/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.test/acme/app/pull/7","state":"open","mergeable":true,"head":{"sha":"head"},"base":{"sha":"base"}}`))
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, "token", "workspace demo forge token")
	raw, err := run(t.Context(), "pr", "diff", "task-branch", "--repo", "acme/app")
	if err != nil || string(raw) != "diff --git a/a b/a\n" {
		t.Fatalf("diff = %q, %v", raw, err)
	}
}

func TestMissingContextCredentialFailsClosed(t *testing.T) {
	_, err := ListReadyIssues(context.Background(), "acme/app")
	if ErrorCategory(err) != ForgePermission {
		t.Fatalf("category = %q, error = %v", ErrorCategory(err), err)
	}
}

func TestAppMergePayloadAttributionAndResponse(t *testing.T) {
	const token = "installation-test-secret"
	const message = "Approved-by: Operator Name <operator@example.com>"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("merge did not use installation credential")
		}
		if r.Method == http.MethodPut {
			calls++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["commit_message"] != message || body["merge_method"] != "merge" {
				t.Errorf("merge payload=%v", body)
			}
			_, _ = w.Write([]byte(`{"merged":true}`))
			return
		}
		if strings.Contains(r.URL.RawQuery, "head=") {
			_, _ = w.Write([]byte(`[{"number":12}]`))
			return
		}
		_, _ = w.Write([]byte(`{"number":12,"html_url":"https://github.com/org/repo/pull/12","state":"closed","merged_at":"2026-09-10T12:00:00Z","merged_by":{"login":" github-operator "},"merge_commit_sha":" landed-sha ","head":{"sha":"head"},"base":{"sha":"base"}}`))
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, token, "workspace demo GitHub App")
	if err := mergePullRequest(WithMergeMessage(t.Context(), message), "org/repo", 12, run); err != nil {
		t.Fatal(err)
	}
	pr, err := pullRequestForBranch(t.Context(), "org/repo", "conveyor/task-test", run)
	if err != nil || !pr.Merged || pr.MergedBy != "github-operator" || pr.MergeCommitSHA != "landed-sha" {
		t.Fatalf("pull=%+v err=%v", pr, err)
	}
	if calls != 1 {
		t.Fatalf("merge calls=%d", calls)
	}
}

func TestAppMergeMessageIsPerCallAndErrorsRedactCredential(t *testing.T) {
	const token = "installation-secret-in-error"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, exists := body["commit_message"]; exists {
			t.Error("message escaped its call context")
		}
		http.Error(w, token, http.StatusForbidden)
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, token, "workspace demo GitHub App")
	err := mergePullRequest(t.Context(), "org/repo", 12, run)
	if err == nil || ErrorCategory(err) != ForgePermission || strings.Contains(err.Error(), token) {
		t.Fatalf("unsafe merge error: %v", err)
	}
}
