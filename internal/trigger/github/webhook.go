package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GitHub webhook event mapping (feature-quiet-idle-runtime, GitHub webhook
// receiver; component-runtime, GitHub webhook applier; req-quiet-idle-runtime
// AC-3.1; DEC-99(5)). The receiver verifies, records, and submits each
// delivery; the applier parses it and calls the dispatcher, monitor, or
// intake handler on its own goroutine within 10 seconds of receipt.

// WebhookEvents are the events a workspace GitHub App subscribes.
var WebhookEvents = []string{"pull_request", "check_suite", "check_run", "status", "push", "issues"}

// WebhookDelivery is one accepted delivery.
type WebhookDelivery struct {
	Workspace  string
	ID         string
	Event      string
	Body       []byte
	ReceivedAt time.Time
}

// WebhookHandlers apply the mapped events. A nil handler skips its events.
type WebhookHandlers struct {
	// PullRequest applies a pull_request delivery for its repository.
	PullRequest func(ctx context.Context, repository string, pr PullRequest) error
	// Checks applies a completed check_suite or check_run, or a status.
	Checks func(ctx context.Context, repository, sha string, branches []string) error
	// Push applies a push of ref, such as refs/heads/main.
	Push func(ctx context.Context, repository, ref string) error
	// ReadyIssue takes in an open issue labeled conveyor:ready.
	ReadyIssue func(ctx context.Context, repository string, issue Issue) error
}

// webhookAction is a parsed delivery: the handler call it maps to, or nil.
type webhookAction func(context.Context, WebhookHandlers) error

// errMalformedDelivery marks a payload that cannot be parsed; retrying the
// same bytes cannot succeed.
var errMalformedDelivery = errors.New("malformed GitHub webhook payload")

type webhookRepository struct {
	FullName string `json:"full_name"`
}

type webhookPullRequest struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	MergedBy       *struct {
		Login string `json:"login"`
	} `json:"merged_by"`
	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}

type webhookPullRequestRef struct {
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// webhookPayload decodes a JSON body, or a form body's payload field, as
// GitHub sends for a webhook configured with the form content type.
func webhookPayload(body []byte, into any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] != '{' {
		values, err := url.ParseQuery(string(trimmed))
		if err != nil || values.Get("payload") == "" {
			return errMalformedDelivery
		}
		trimmed = []byte(values.Get("payload"))
	}
	if err := json.Unmarshal(trimmed, into); err != nil {
		return fmt.Errorf("%w: %v", errMalformedDelivery, err)
	}
	return nil
}

// parseWebhook maps one delivery to its action. It returns nil for an event
// or action that applies nothing, including ping.
func parseWebhook(event string, body []byte) (webhookAction, error) {
	switch event {
	case "pull_request":
		var payload struct {
			Action      string             `json:"action"`
			PullRequest webhookPullRequest `json:"pull_request"`
			Repository  webhookRepository  `json:"repository"`
		}
		if err := webhookPayload(body, &payload); err != nil {
			return nil, err
		}
		source := payload.PullRequest
		pr := PullRequest{Number: source.Number, URL: source.HTMLURL, State: source.State, Merged: source.Merged, MergeCommitSHA: source.MergeCommitSHA, HeadSHA: source.Head.SHA, HeadRef: source.Head.Ref, BaseSHA: source.Base.SHA, Mergeable: "UNKNOWN"}
		if source.MergedBy != nil {
			pr.MergedBy = source.MergedBy.Login
		}
		repository := payload.Repository.FullName
		return func(ctx context.Context, h WebhookHandlers) error {
			if h.PullRequest == nil {
				return nil
			}
			return h.PullRequest(ctx, repository, pr)
		}, nil
	case "check_suite", "check_run":
		var payload struct {
			Action     string `json:"action"`
			CheckSuite struct {
				HeadSHA      string                  `json:"head_sha"`
				HeadBranch   string                  `json:"head_branch"`
				PullRequests []webhookPullRequestRef `json:"pull_requests"`
			} `json:"check_suite"`
			CheckRun struct {
				HeadSHA    string `json:"head_sha"`
				CheckSuite struct {
					HeadBranch string `json:"head_branch"`
				} `json:"check_suite"`
				PullRequests []webhookPullRequestRef `json:"pull_requests"`
			} `json:"check_run"`
			Repository webhookRepository `json:"repository"`
		}
		if err := webhookPayload(body, &payload); err != nil {
			return nil, err
		}
		if payload.Action != "completed" {
			return nil, nil
		}
		sha, branches, refs := payload.CheckSuite.HeadSHA, []string{payload.CheckSuite.HeadBranch}, payload.CheckSuite.PullRequests
		if event == "check_run" {
			sha, branches, refs = payload.CheckRun.HeadSHA, []string{payload.CheckRun.CheckSuite.HeadBranch}, payload.CheckRun.PullRequests
		}
		for _, ref := range refs {
			branches = append(branches, ref.Head.Ref)
		}
		return checksAction(payload.Repository.FullName, sha, branches), nil
	case "status":
		var payload struct {
			SHA      string `json:"sha"`
			Branches []struct {
				Name string `json:"name"`
			} `json:"branches"`
			Repository webhookRepository `json:"repository"`
		}
		if err := webhookPayload(body, &payload); err != nil {
			return nil, err
		}
		branches := make([]string, 0, len(payload.Branches))
		for _, branch := range payload.Branches {
			branches = append(branches, branch.Name)
		}
		return checksAction(payload.Repository.FullName, payload.SHA, branches), nil
	case "push":
		var payload struct {
			Ref        string            `json:"ref"`
			Deleted    bool              `json:"deleted"`
			Repository webhookRepository `json:"repository"`
		}
		if err := webhookPayload(body, &payload); err != nil {
			return nil, err
		}
		if payload.Deleted || !strings.HasPrefix(payload.Ref, "refs/heads/") {
			return nil, nil
		}
		repository, ref := payload.Repository.FullName, payload.Ref
		return func(ctx context.Context, h WebhookHandlers) error {
			if h.Push == nil {
				return nil
			}
			return h.Push(ctx, repository, ref)
		}, nil
	case "issues":
		var payload struct {
			Action string `json:"action"`
			Label  struct {
				Name string `json:"name"`
			} `json:"label"`
			Issue struct {
				Number      int             `json:"number"`
				Title       string          `json:"title"`
				Body        string          `json:"body"`
				HTMLURL     string          `json:"html_url"`
				State       string          `json:"state"`
				PullRequest json.RawMessage `json:"pull_request"`
			} `json:"issue"`
			Repository webhookRepository `json:"repository"`
		}
		if err := webhookPayload(body, &payload); err != nil {
			return nil, err
		}
		if payload.Action != "labeled" || !strings.EqualFold(payload.Label.Name, ReadyLabel) || payload.Issue.State != "open" || len(payload.Issue.PullRequest) > 0 || payload.Issue.Number <= 0 {
			return nil, nil
		}
		repository := payload.Repository.FullName
		issue := Issue{Number: payload.Issue.Number, Title: payload.Issue.Title, Body: payload.Issue.Body, URL: payload.Issue.HTMLURL}
		return func(ctx context.Context, h WebhookHandlers) error {
			if h.ReadyIssue == nil {
				return nil
			}
			return h.ReadyIssue(ctx, repository, issue)
		}, nil
	}
	return nil, nil
}

func checksAction(repository, sha string, branches []string) webhookAction {
	seen := map[string]bool{}
	unique := make([]string, 0, len(branches))
	for _, branch := range branches {
		if branch != "" && !seen[branch] {
			seen[branch] = true
			unique = append(unique, branch)
		}
	}
	return func(ctx context.Context, h WebhookHandlers) error {
		if h.Checks == nil {
			return nil
		}
		return h.Checks(ctx, repository, sha, unique)
	}
}

// WebhookApplier applies accepted deliveries asynchronously. Start binds the
// service context; Submit never blocks the receiver.
type WebhookApplier struct {
	Handlers WebhookHandlers
	// Bind returns the context a delivery applies under: the service
	// context bound to the delivery's workspace and the github-webhook
	// system actor.
	Bind func(ctx context.Context, workspace string) context.Context
	// Forget removes a delivery whose application failed for good, so a
	// redelivery from GitHub is accepted again.
	Forget func(ctx context.Context, workspace, deliveryID string) error
	// Delivered tells the timed GitHub fallback that the workspace has
	// webhook delivery (component-runtime, Timed GitHub fallback).
	Delivered func(workspace string)
	// Logf writes error-level lines; nil uses log.Printf.
	Logf func(string, ...any)
	// RetryDelays are the waits before the second and later attempts; nil
	// waits one and then two seconds.
	RetryDelays []time.Duration
	// Sleep waits before a retry; nil waits on a timer.
	Sleep func(context.Context, time.Duration) error
	// MaxConcurrent bounds concurrent applications; zero means eight.
	MaxConcurrent int

	once    sync.Once
	ctx     context.Context
	slots   chan struct{}
	pending sync.WaitGroup
}

// Start binds the service context. A delivery submitted before Start
// applies under context.Background.
func (a *WebhookApplier) Start(ctx context.Context) {
	a.init()
	a.ctx = ctx
}

func (a *WebhookApplier) init() {
	a.once.Do(func() {
		limit := a.MaxConcurrent
		if limit <= 0 {
			limit = 8
		}
		a.slots = make(chan struct{}, limit)
		if a.ctx == nil {
			a.ctx = context.Background()
		}
	})
}

// Submit applies one delivery on its own goroutine.
func (a *WebhookApplier) Submit(delivery WebhookDelivery) {
	a.init()
	if a.Delivered != nil {
		a.Delivered(delivery.Workspace)
	}
	a.pending.Add(1)
	go func() {
		defer a.pending.Done()
		ctx := a.ctx
		select {
		case a.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-a.slots }()
		a.apply(ctx, delivery)
	}()
}

// Wait returns once every submitted delivery has finished.
func (a *WebhookApplier) Wait() { a.pending.Wait() }

func (a *WebhookApplier) apply(ctx context.Context, delivery WebhookDelivery) {
	if a.Bind != nil {
		ctx = a.Bind(ctx, delivery.Workspace)
	}
	action, err := parseWebhook(delivery.Event, delivery.Body)
	if err != nil {
		// The same bytes would fail again, so a malformed payload is not
		// retried or forgotten.
		a.logf(`level=error msg="GitHub webhook delivery not applied" workspace=%s delivery=%s event=%s err=%v`, delivery.Workspace, delivery.ID, delivery.Event, err)
		return
	}
	if action == nil {
		return
	}
	delays := a.RetryDelays
	if delays == nil {
		delays = []time.Duration{time.Second, 2 * time.Second}
	}
	for attempt := 0; ; attempt++ {
		err = action(ctx, a.Handlers)
		if err == nil || ctx.Err() != nil {
			return
		}
		if attempt >= len(delays) {
			break
		}
		if a.sleep(ctx, delays[attempt]) != nil {
			return
		}
	}
	a.logf(`level=error msg="GitHub webhook delivery not applied" workspace=%s delivery=%s event=%s attempts=%d err=%v`, delivery.Workspace, delivery.ID, delivery.Event, len(delays)+1, err)
	if a.Forget == nil {
		return
	}
	if forgetErr := a.Forget(ctx, delivery.Workspace, delivery.ID); forgetErr != nil {
		a.logf(`level=error msg="GitHub webhook delivery could not be forgotten" workspace=%s delivery=%s event=%s err=%v`, delivery.Workspace, delivery.ID, delivery.Event, forgetErr)
	}
}

func (a *WebhookApplier) sleep(ctx context.Context, delay time.Duration) error {
	if a.Sleep != nil {
		return a.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *WebhookApplier) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
