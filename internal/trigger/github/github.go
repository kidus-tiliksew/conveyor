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
	"strings"
)

// ReadyLabel dispatches an issue into the factory (spec §9).
const ReadyLabel = "conveyor:ready"

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

// OpenPR pushes the task branch and opens a PR against base. The PR
// body records provenance (task ID, source issue) for the audit chain.
func OpenPR(ctx context.Context, worktreeDir, repo, branch, base, title, body string) (string, error) {
	if err := run(ctx, worktreeDir, "git", "push", "--set-upstream", "origin", branch); err != nil {
		return "", err
	}
	out, err := gh(ctx, "pr", "create",
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
