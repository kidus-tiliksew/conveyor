package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
