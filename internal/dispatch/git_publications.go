package dispatch

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func PRBody(task core.Task, evidence ...core.Artifact) string {
	body := fmt.Sprintf("<!-- conveyor:task-link -->\nConveyor task `%s`\n\nSource: %s\n", task.ID, task.Source)
	if task.GitHub != nil && task.GitHub.IssueNumber > 0 {
		body += fmt.Sprintf("\nCloses #%d\n", task.GitHub.IssueNumber)
	}
	if len(evidence) > 0 {
		body += "\n<!-- conveyor:verification-evidence -->\n### Verification evidence\n\n"
		for _, artifact := range evidence {
			if !artifact.EligibleVerificationEvidence() || artifact.TaskID != task.ID {
				continue
			}
			name := strings.NewReplacer("`", "'", "\r", " ", "\n", " ").Replace(strings.TrimSpace(artifact.Name))
			body += fmt.Sprintf("- `%s` — `%s`, %d bytes, SHA-256 `%s`\n", name, artifact.ContentType, artifact.SizeBytes, artifact.ID)
		}
		body += "\nEvidence media remains in Conveyor's task-scoped artifact store. This PR mirror intentionally publishes durable metadata only—no control-plane credentials or private artifact URLs.\n"
	}
	return body
}

func (d *Dispatcher) queueApprovedIssue(ctx context.Context, task core.Task, spec core.SpecVersion) error {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || strings.TrimSpace(repo.GitHub) == "" {
		return nil
	}
	sourceNumber, err := sourceIssueNumber(repo.GitHub, task.Source)
	if err != nil {
		return err
	}
	return d.Store.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{
		TaskID: task.ID, Repository: repo.GitHub, SpecVersion: spec.Version,
		Source: task.Source, SourceIssueNumber: sourceNumber,
	})
}

// ReconcileGitHubLifecycles repairs the narrow approval-to-outbox gap after a
// process restart. Remote side effects remain owned by the durable queue job.
func (d *Dispatcher) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	tasks, err := d.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, task := range tasks {
		if task.SupersededBy != "" {
			if err = d.queueStartedOverPR(ctx, task); err != nil {
				return repaired, err
			}
		}
		spec, ok, getErr := d.Store.GetLatestSpecVersion(ctx, task.ID)
		if getErr != nil {
			return repaired, getErr
		}
		if !ok || !spec.Approved {
			continue
		}
		if _, exists, getErr := d.Store.GetGitHubLifecycle(ctx, task.ID); getErr != nil {
			return repaired, getErr
		} else if exists {
			continue
		}
		if err = d.queueApprovedIssue(ctx, task, spec); err != nil {
			return repaired, err
		}
		if _, exists, getErr := d.Store.GetGitHubLifecycle(ctx, task.ID); getErr != nil {
			return repaired, getErr
		} else if exists {
			repaired++
		}
	}
	return repaired, nil
}

func sourceIssueNumber(repository, source string) (int, error) {
	slug, rawNumber := "", ""
	if value, ok := strings.CutPrefix(strings.TrimSpace(source), "github:"); ok {
		slug, rawNumber, _ = strings.Cut(value, "#")
	} else if parsed, err := url.Parse(strings.TrimSpace(source)); err == nil && strings.EqualFold(parsed.Host, "github.com") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 4 && parts[2] == "issues" {
			slug, rawNumber = parts[0]+"/"+parts[1], parts[3]
		}
	}
	if slug == "" || rawNumber == "" {
		return 0, nil
	}
	if slug != repository {
		return 0, fmt.Errorf("GitHub source repository %q does not match configured repository %q", slug, repository)
	}
	number, err := strconv.Atoi(rawNumber)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("invalid GitHub source issue %q", source)
	}
	return number, nil
}
