// Package github implements the Phase 1 trigger and output: issues
// labeled conveyor:ready become tasks, and completed tasks open PRs
// (spec §9, §19 Phase 1: "GitHub issue → PR").
//
// Phase 1 shells out to the gh CLI (already authenticated on the
// user's machine); webhook ingestion arrives with the HTTP API in
// later phases.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ReadyLabel dispatches an issue into the factory (spec §9).
const ReadyLabel = "conveyor:ready"

// DispatchedLabel is the durable claim marker for a GitHub issue. Moving
// an issue from ReadyLabel to DispatchedLabel prevents a conveyord restart
// from replaying it after the Phase 1 in-memory store is lost.
const DispatchedLabel = "conveyor:dispatched"

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

// ListReadyIssues polls a repo for issues carrying the ready label.
func ListReadyIssues(ctx context.Context, repo string) ([]Issue, error) {
	out, err := gh(ctx, "issue", "list",
		"--repo", repo,
		"--label", ReadyLabel,
		"--state", "open",
		"--json", "number,title,body,url")
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parse gh issue list: %w", err)
	}
	return issues, nil
}

// MarkIssueDispatched durably claims an issue before it is enqueued. The
// label is maintained by Conveyor so scratch repositories need no manual
// label setup beyond conveyor:ready.
func MarkIssueDispatched(ctx context.Context, repo string, number int, taskID string) error {
	return markIssueDispatched(ctx, repo, number, taskID, gh)
}

type ghRunner func(context.Context, ...string) ([]byte, error)

func markIssueDispatched(ctx context.Context, repo string, number int, taskID string, run ghRunner) error {
	if _, err := run(ctx, "label", "create", DispatchedLabel,
		"--repo", repo,
		"--color", "1D76DB",
		"--description", "Claimed by Conveyor for dispatch",
		"--force"); err != nil {
		return fmt.Errorf("ensure %s label: %w", DispatchedLabel, err)
	}
	if _, err := run(ctx, "issue", "edit", strconv.Itoa(number),
		"--repo", repo,
		"--remove-label", ReadyLabel,
		"--add-label", DispatchedLabel); err != nil {
		return fmt.Errorf("move issue labels: %w", err)
	}
	_, _ = run(ctx, "issue", "comment", strconv.Itoa(number),
		"--repo", repo,
		"--body", fmt.Sprintf("<!-- conveyor:dispatched -->\nConveyor accepted this issue as task `%s`.", taskID))
	// The label transition is the deduplication boundary. The comment is
	// audit-friendly, but its failure must not suppress dispatch after the
	// ready label has already been removed.
	return nil
}

// OpenPR pushes the task branch and opens a PR against base. The PR
// body records provenance (task ID, source issue) for the audit chain.
func OpenPR(ctx context.Context, worktreeDir, repo, branch, base, title, body string) (string, error) {
	return openPR(ctx, worktreeDir, repo, branch, base, title, body, run, gh)
}

type gitRunner func(context.Context, string, string, ...string) error

func openPR(ctx context.Context, worktreeDir, repo, branch, base, title, body string, runGit gitRunner, runGH ghRunner) (string, error) {
	if err := runGit(ctx, worktreeDir, "git", "push", "--set-upstream", "origin", branch); err != nil {
		return "", err
	}
	// Redirect/re-dispatch pushes new commits to the existing task branch.
	// Reuse its open PR rather than failing `gh pr create` on the second job.
	existing, err := runGH(ctx, "pr", "list",
		"--repo", repo,
		"--head", branch,
		"--state", "open",
		"--json", "url",
		"--jq", ".[0].url")
	if err != nil {
		return "", fmt.Errorf("find existing PR: %w", err)
	}
	if url := strings.TrimSpace(string(existing)); url != "" {
		return url, nil
	}
	out, err := runGH(ctx, "pr", "create",
		"--repo", repo,
		"--head", branch,
		"--base", base,
		"--title", title,
		"--body", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil // gh prints the PR URL
}

func gh(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, ee.Stderr)
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}
