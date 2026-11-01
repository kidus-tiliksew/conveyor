package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCall struct {
	kind, repository, sha, ref string
	branches                   []string
	pr                         PullRequest
	issue                      Issue
}

type recordingHandlers struct {
	mu    sync.Mutex
	calls []recordedCall
	err   []error
}

func (r *recordingHandlers) next() error {
	if len(r.err) == 0 {
		return nil
	}
	err := r.err[0]
	r.err = r.err[1:]
	return err
}

func (r *recordingHandlers) handlers() WebhookHandlers {
	return WebhookHandlers{
		PullRequest: func(_ context.Context, repository string, pr PullRequest) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, recordedCall{kind: "pull_request", repository: repository, pr: pr})
			return r.next()
		},
		Checks: func(_ context.Context, repository, sha string, branches []string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, recordedCall{kind: "checks", repository: repository, sha: sha, branches: branches})
			return r.next()
		},
		Push: func(_ context.Context, repository, ref string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, recordedCall{kind: "push", repository: repository, ref: ref})
			return r.next()
		},
		ReadyIssue: func(_ context.Context, repository string, issue Issue) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, recordedCall{kind: "issue", repository: repository, issue: issue})
			return r.next()
		},
	}
}

// TestParseWebhookMapsEachEvent: each subscribed event and action maps to
// its handler with the payload's repository, pull request, commit, branches,
// ref, or issue, and every other event or action applies nothing
// (feature-quiet-idle-runtime, GitHub webhook receiver; component-runtime,
// GitHub webhook applier; req-quiet-idle-runtime AC-3.1).
func TestParseWebhookMapsEachEvent(t *testing.T) {
	const repo = `"repository":{"full_name":"acme/app"}`
	merged := `{"action":"closed","number":41,"pull_request":{"number":41,"html_url":"https://github.com/acme/app/pull/41","state":"closed","merged":true,"merge_commit_sha":"landed","merged_by":{"login":"octocat"},"head":{"sha":"head-sha","ref":"conveyor/task-1"},"base":{"sha":"base-sha"}},` + repo + `}`
	cases := []struct {
		name, event, body string
		want              *recordedCall
	}{
		{"merged pull request", "pull_request", merged, &recordedCall{kind: "pull_request", repository: "acme/app", pr: PullRequest{Number: 41, URL: "https://github.com/acme/app/pull/41", State: "closed", Merged: true, MergeCommitSHA: "landed", MergedBy: "octocat", HeadSHA: "head-sha", HeadRef: "conveyor/task-1", BaseSHA: "base-sha", Mergeable: "UNKNOWN"}}},
		{"synchronized pull request", "pull_request", `{"action":"synchronize","pull_request":{"number":41,"state":"open","head":{"sha":"new","ref":"conveyor/task-1"}},` + repo + `}`, &recordedCall{kind: "pull_request", repository: "acme/app", pr: PullRequest{Number: 41, State: "open", HeadSHA: "new", HeadRef: "conveyor/task-1", Mergeable: "UNKNOWN"}}},
		{"form-encoded pull request", "pull_request", "payload=" + url.QueryEscape(merged), &recordedCall{kind: "pull_request", repository: "acme/app", pr: PullRequest{Number: 41, URL: "https://github.com/acme/app/pull/41", State: "closed", Merged: true, MergeCommitSHA: "landed", MergedBy: "octocat", HeadSHA: "head-sha", HeadRef: "conveyor/task-1", BaseSHA: "base-sha", Mergeable: "UNKNOWN"}}},
		{"completed check suite", "check_suite", `{"action":"completed","check_suite":{"head_sha":"head-sha","head_branch":"conveyor/task-1","pull_requests":[{"head":{"ref":"conveyor/task-1"}}]},` + repo + `}`, &recordedCall{kind: "checks", repository: "acme/app", sha: "head-sha", branches: []string{"conveyor/task-1"}}},
		{"requested check suite", "check_suite", `{"action":"requested","check_suite":{"head_sha":"head-sha"},` + repo + `}`, nil},
		{"completed check run", "check_run", `{"action":"completed","check_run":{"head_sha":"run-sha","check_suite":{"head_branch":""},"pull_requests":[{"head":{"ref":"conveyor/task-2"}}]},` + repo + `}`, &recordedCall{kind: "checks", repository: "acme/app", sha: "run-sha", branches: []string{"conveyor/task-2"}}},
		{"created check run", "check_run", `{"action":"created","check_run":{"head_sha":"run-sha"},` + repo + `}`, nil},
		{"status", "status", `{"sha":"status-sha","state":"success","branches":[{"name":"conveyor/task-3"},{"name":"main"}],` + repo + `}`, &recordedCall{kind: "checks", repository: "acme/app", sha: "status-sha", branches: []string{"conveyor/task-3", "main"}}},
		{"push to a branch", "push", `{"ref":"refs/heads/main",` + repo + `}`, &recordedCall{kind: "push", repository: "acme/app", ref: "refs/heads/main"}},
		{"push of a tag", "push", `{"ref":"refs/tags/v1",` + repo + `}`, nil},
		{"branch deletion", "push", `{"ref":"refs/heads/old","deleted":true,` + repo + `}`, nil},
		{"ready label", "issues", `{"action":"labeled","label":{"name":"conveyor:ready"},"issue":{"number":7,"title":"Fix it","body":"Details","html_url":"https://github.com/acme/app/issues/7","state":"open"},` + repo + `}`, &recordedCall{kind: "issue", repository: "acme/app", issue: Issue{Number: 7, Title: "Fix it", Body: "Details", URL: "https://github.com/acme/app/issues/7"}}},
		{"other label", "issues", `{"action":"labeled","label":{"name":"bug"},"issue":{"number":7,"state":"open"},` + repo + `}`, nil},
		{"ready label on a pull request", "issues", `{"action":"labeled","label":{"name":"conveyor:ready"},"issue":{"number":8,"state":"open","pull_request":{"url":"x"}},` + repo + `}`, nil},
		{"ready label on a closed issue", "issues", `{"action":"labeled","label":{"name":"conveyor:ready"},"issue":{"number":9,"state":"closed"},` + repo + `}`, nil},
		{"opened issue", "issues", `{"action":"opened","issue":{"number":7,"state":"open"},` + repo + `}`, nil},
		{"ping", "ping", `{"zen":"Keep it logically awesome."}`, nil},
		{"installation", "installation", `{"action":"created"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, err := parseWebhook(tc.event, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if action != nil {
					t.Fatal("event mapped to an action")
				}
				return
			}
			if action == nil {
				t.Fatal("event mapped to nothing")
			}
			recorder := &recordingHandlers{}
			if err := action(context.Background(), recorder.handlers()); err != nil {
				t.Fatal(err)
			}
			if len(recorder.calls) != 1 || !reflect.DeepEqual(recorder.calls[0], *tc.want) {
				t.Fatalf("calls=%+v, want %+v", recorder.calls, *tc.want)
			}
		})
	}
	if _, err := parseWebhook("pull_request", []byte("{not json")); !errors.Is(err, errMalformedDelivery) {
		t.Fatalf("malformed payload: %v", err)
	}
}

type applierFixture struct {
	applier   *WebhookApplier
	recorder  *recordingHandlers
	mu        sync.Mutex
	lines     []string
	sleeps    []time.Duration
	forgotten []string
	delivered []string
	bound     []string
}

func newApplierFixture(forgetErr error) *applierFixture {
	f := &applierFixture{recorder: &recordingHandlers{}}
	f.applier = &WebhookApplier{
		Handlers: f.recorder.handlers(),
		Bind: func(ctx context.Context, workspace string) context.Context {
			f.mu.Lock()
			f.bound = append(f.bound, workspace)
			f.mu.Unlock()
			return ctx
		},
		Forget: func(_ context.Context, workspace, delivery string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forgotten = append(f.forgotten, workspace+"/"+delivery)
			return forgetErr
		},
		Delivered: func(workspace string) {
			f.mu.Lock()
			f.delivered = append(f.delivered, workspace)
			f.mu.Unlock()
		},
		Logf: func(format string, args ...any) {
			f.mu.Lock()
			f.lines = append(f.lines, fmt.Sprintf(format, args...))
			f.mu.Unlock()
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			f.mu.Lock()
			f.sleeps = append(f.sleeps, d)
			f.mu.Unlock()
			return nil
		},
	}
	f.applier.Start(context.Background())
	return f
}

var pushDelivery = WebhookDelivery{Workspace: "demo", ID: "delivery-7", Event: "push", Body: []byte(`{"ref":"refs/heads/main","repository":{"full_name":"acme/app"}}`)}

// TestWebhookApplierRetriesThenForgetsFailedDelivery: an application that
// keeps failing is retried after one and then two seconds, then writes one
// error-level line naming the workspace, delivery, and event and forgets the
// delivery, so GitHub's redelivery applies it again; nothing is lost
// silently (component-runtime, GitHub webhook applier).
func TestWebhookApplierRetriesThenForgetsFailedDelivery(t *testing.T) {
	f := newApplierFixture(nil)
	failure := errors.New("database unavailable")
	f.recorder.err = []error{failure, failure, failure}
	f.applier.Submit(pushDelivery)
	f.applier.Wait()
	if len(f.recorder.calls) != 3 || !reflect.DeepEqual(f.sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("calls=%d sleeps=%v", len(f.recorder.calls), f.sleeps)
	}
	if len(f.lines) != 1 || !strings.Contains(f.lines[0], `level=error msg="GitHub webhook delivery not applied" workspace=demo delivery=delivery-7 event=push`) || !strings.Contains(f.lines[0], "database unavailable") {
		t.Fatalf("lines=%v", f.lines)
	}
	if !reflect.DeepEqual(f.forgotten, []string{"demo/delivery-7"}) || !reflect.DeepEqual(f.delivered, []string{"demo"}) || !reflect.DeepEqual(f.bound, []string{"demo"}) {
		t.Fatalf("forgotten=%v delivered=%v bound=%v", f.forgotten, f.delivered, f.bound)
	}
}

// TestWebhookApplierTransientFailureApplies: a failure that clears on retry
// applies the delivery with no error line and no forget.
func TestWebhookApplierTransientFailureApplies(t *testing.T) {
	f := newApplierFixture(nil)
	f.recorder.err = []error{errors.New("transient")}
	f.applier.Submit(pushDelivery)
	f.applier.Wait()
	if len(f.recorder.calls) != 2 || len(f.lines) != 0 || len(f.forgotten) != 0 || !reflect.DeepEqual(f.sleeps, []time.Duration{time.Second}) {
		t.Fatalf("calls=%d lines=%v forgotten=%v sleeps=%v", len(f.recorder.calls), f.lines, f.forgotten, f.sleeps)
	}
}

// TestWebhookApplierReportsFailedForget: a forget that fails writes a second
// error line.
func TestWebhookApplierReportsFailedForget(t *testing.T) {
	f := newApplierFixture(errors.New("store down"))
	failure := errors.New("database unavailable")
	f.recorder.err = []error{failure, failure, failure}
	f.applier.Submit(pushDelivery)
	f.applier.Wait()
	if len(f.lines) != 2 || !strings.Contains(f.lines[1], `level=error msg="GitHub webhook delivery could not be forgotten" workspace=demo delivery=delivery-7`) {
		t.Fatalf("lines=%v", f.lines)
	}
}

// TestWebhookApplierMalformedPayloadIsNotRetried: a payload that cannot be
// parsed writes one error line and is neither retried nor forgotten.
func TestWebhookApplierMalformedPayloadIsNotRetried(t *testing.T) {
	f := newApplierFixture(nil)
	f.applier.Submit(WebhookDelivery{Workspace: "demo", ID: "bad", Event: "pull_request", Body: []byte("{")})
	f.applier.Wait()
	if len(f.recorder.calls) != 0 || len(f.sleeps) != 0 || len(f.forgotten) != 0 || len(f.lines) != 1 || !strings.Contains(f.lines[0], "delivery=bad event=pull_request") {
		t.Fatalf("calls=%d sleeps=%v forgotten=%v lines=%v", len(f.recorder.calls), f.sleeps, f.forgotten, f.lines)
	}
}

// TestWebhookApplierStopsWithServiceContext: a delivery waiting for a slot
// when the service context ends is dropped without an application.
func TestWebhookApplierStopsWithServiceContext(t *testing.T) {
	f := newApplierFixture(nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.applier = &WebhookApplier{Handlers: f.recorder.handlers(), MaxConcurrent: 1}
	f.applier.Start(ctx)
	f.applier.slots <- struct{}{}
	f.applier.Submit(pushDelivery)
	cancel()
	f.applier.Wait()
	if len(f.recorder.calls) != 0 {
		t.Fatalf("calls=%d after the service context ended", len(f.recorder.calls))
	}
}
