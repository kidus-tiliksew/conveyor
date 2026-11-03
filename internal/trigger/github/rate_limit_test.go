package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestRateLimitRetryAtReadsGitHubHeaders: a rate-limited answer carries the
// later of Retry-After, in seconds or as an HTTP date, and
// X-RateLimit-Reset; neither header leaves the time zero.
func TestRateLimitRetryAtReadsGitHubHeaders(t *testing.T) {
	now := time.Date(2026, 11, 3, 6, 37, 0, 0, time.UTC)
	reset := now.Add(23 * time.Minute)
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    time.Time
	}{
		{"reset", map[string]string{"X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, reset},
		{"retry seconds", map[string]string{"Retry-After": "90"}, now.Add(90 * time.Second)},
		{"retry date", map[string]string{"Retry-After": reset.Format(http.TimeFormat)}, reset},
		{"later of both", map[string]string{"Retry-After": "60", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, reset},
		{"retry later than reset", map[string]string{"Retry-After": "3600", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, now.Add(time.Hour)},
		{"neither", map[string]string{}, time.Time{}},
		{"malformed", map[string]string{"Retry-After": "soon", "X-RateLimit-Reset": "later"}, time.Time{}},
	} {
		header := http.Header{}
		for k, v := range tc.headers {
			header.Set(k, v)
		}
		if got := rateLimitRetryAt(header, now); !got.Equal(tc.want) {
			t.Errorf("%s: retry at %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestRESTRunnerCarriesRateLimitReset: a rate-limited REST answer keeps its
// category and GitHub's reset time through a wrapping caller.
func TestRESTRunnerCarriesRateLimitReset(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded for installation ID 12."}`, http.StatusForbidden)
	}))
	defer server.Close()
	run := newRESTRunner(server.Client(), server.URL, "token", "workspace demo GitHub App")
	_, err := run(t.Context(), "api", "repos/acme/app/pulls")
	retryAt, limited := RateLimitRetryAt(fmt.Errorf("view pull request: %w", err))
	if !limited || !retryAt.Equal(reset) {
		t.Fatalf("limited=%t retry at %s, want %s; err=%v", limited, retryAt, reset, err)
	}
	if _, limited := RateLimitRetryAt(PermissionError(errors.New("denied"))); limited {
		t.Fatal("a permission failure reported rate limited")
	}
}

// TestAppOperationErrorNamesCategory: the App redaction keeps no GitHub
// response body, names the failure category in its message, and keeps a rate
// limit's reset time.
func TestAppOperationErrorNamesCategory(t *testing.T) {
	_, private := appTestKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(20 * time.Minute)
	app := core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{Connected: true, WorkspaceID: "demo", AppID: 41, InstallationID: 12}, PrivateKey: private}
	for _, tc := range []struct {
		name     string
		status   int
		headers  map[string]string
		category ForgeErrorCategory
		message  string
	}{
		{"rate limited", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, ForgeRateLimited, "GitHub App request rate limited"},
		{"too many requests", http.StatusTooManyRequests, map[string]string{"Retry-After": "1200"}, ForgeRateLimited, "GitHub App request rate limited"},
		{"server error", http.StatusBadGateway, nil, ForgeStatus, "GitHub App request failed: GitHub error status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"message":"secret-bearing GitHub response body"}`)
			}))
			defer server.Close()
			client := &AppClient{Now: func() time.Time { return now }, Runner: func(token, identity string) AppRunner {
				return newRESTRunner(server.Client(), server.URL, token, identity)
			}}
			_, err := client.Token(t.Context(), "demo", app)
			if ErrorCategory(err) != tc.category || err.Error() != tc.message {
				t.Fatalf("category %s message %q, want %s %q", ErrorCategory(err), err, tc.category, tc.message)
			}
			if strings.Contains(err.Error(), "secret-bearing") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error leaked GitHub detail: %v", err)
			}
			retryAt, limited := RateLimitRetryAt(err)
			if limited != (tc.category == ForgeRateLimited) || limited && retryAt.Before(reset.Add(-time.Minute)) {
				t.Fatalf("limited=%t retry at %s", limited, retryAt)
			}
		})
	}
}

// TestReadFailureState: a rate-limited read, including a rate-limited 403,
// maps to rate_limited; other failures keep their discovery states.
func TestReadFailureState(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		want   string
	}{
		{RateLimitedError(time.Time{}, errors.New("limited")), http.StatusForbidden, "rate_limited"},
		{RateLimitedError(time.Time{}, errors.New("limited")), http.StatusTooManyRequests, "rate_limited"},
		{PermissionError(errors.New("denied")), 0, "permission"},
		{&Error{Category: ForgeStatus, Err: errors.New("forbidden")}, http.StatusForbidden, "permission"},
		{&Error{Category: ForgeStatus, Err: errors.New("missing")}, http.StatusNotFound, "unknown_revision"},
		{&Error{Category: ForgeRequest, Err: errors.New("reset")}, 0, "transport"},
	} {
		if got := readFailureState(tc.err, tc.status); got != tc.want {
			t.Errorf("%v (%d): %s, want %s", tc.err, tc.status, got, tc.want)
		}
	}
}
