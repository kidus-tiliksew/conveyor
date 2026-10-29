package dispatch

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// PRBodyContext carries the deployment and repository facts the pull-request
// body renders beyond the task itself.
type PRBodyContext struct {
	// PublicURL is the deployment's configured public dashboard origin
	// (config.PublicURLEnv). Callers inject it; the body never reads the
	// environment at render time.
	PublicURL string
	// Repository is the task repository's GitHub owner/repo slug.
	Repository string
}

// PRBody renders the Conveyor-managed pull-request body region. The task ID
// links to the task's workspace-scoped dashboard page when the deployment has
// a valid public URL, and stays plain otherwise (req-delivery-and-forge
// AC-2.5; req-workspace-addresses AC-1.1, AC-1.10). A task filed from an issue
// in its own repository closes that issue whether or not issue publication
// published an association, and one issue never gets two closing lines
// (req-delivery-and-forge AC-8.6; component-git-delivery).
func PRBody(task core.Task, bodyContext PRBodyContext, evidence ...core.Artifact) string {
	ref := "`" + task.ID + "`"
	if link := TaskDashboardURL(bodyContext.PublicURL, task.Workspace, task.ID); link != "" {
		ref = "[" + ref + "](" + link + ")"
	}
	body := fmt.Sprintf("<!-- conveyor:task-link -->\nConveyor task %s\n\nSource: %s\n", ref, task.Source)
	var closes []int
	if task.GitHub != nil && task.GitHub.IssueNumber > 0 {
		closes = append(closes, task.GitHub.IssueNumber)
	}
	if number := sameRepositorySourceIssue(bodyContext.Repository, task.Source); number > 0 && !slices.Contains(closes, number) {
		closes = append(closes, number)
	}
	for _, number := range closes {
		body += fmt.Sprintf("\nCloses #%d\n", number)
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

// TaskDashboardURL returns the task's dashboard page under its workspace,
// <public URL>/w/<workspace>/tasks/<task-id> (req-workspace-addresses AC-1.1,
// AC-1.10), or "" when the public URL is unset or invalid or the task names no
// workspace. A valid public URL is http or https with a host and no user info,
// query, or fragment, as verification delivery requires
// (component-verification-publication).
func TaskDashboardURL(publicURL, workspace, taskID string) string {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	workspace, taskID = strings.TrimSpace(workspace), strings.TrimSpace(taskID)
	if base == "" || len(base) > 512 || workspace == "" || taskID == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return ""
	}
	link := u.JoinPath("w", workspace, "tasks", taskID).String()
	// The link is a Markdown destination; refuse anything that could end it.
	if strings.ContainsAny(link, " \t\r\n()<>[]`") {
		return ""
	}
	return link
}

// sameRepositorySourceIssue returns the source issue number when the task was
// filed from an issue in the task repository, compared case-insensitively as
// GitHub compares slugs, and 0 for any other source.
func sameRepositorySourceIssue(repository, source string) int {
	repository = strings.ToLower(strings.TrimSpace(repository))
	if repository == "" {
		return 0
	}
	number, err := sourceIssueNumber(repository, strings.ToLower(source))
	if err != nil {
		return 0
	}
	return number
}

// queueApprovedIssue queues the approved plan's issue publication only while
// workspace issue publication is on and only for a plan approved at or after
// the last turn-on, so an approval with the setting off creates and edits no
// issue, including the task's source issue, even when the setting turns on
// between the approval commit and this read (req-delivery-and-forge AC-8.2,
// AC-8.3; DEC-91). Callers pass the pre-approval plan value, so the approval
// time is read from the persisted version.
func (d *Dispatcher) queueApprovedIssue(ctx context.Context, task core.Task, spec core.SpecVersion) error {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return err
	}
	since, on := cfg.IssuePublication.ApprovedSince()
	if !on {
		return nil
	}
	approved, exists, err := d.Store.GetLatestSpecVersion(ctx, task.ID)
	if err != nil {
		return err
	}
	if !exists || !approved.Approved || approved.Version != spec.Version || approved.ApprovedAt.Before(since) {
		return nil
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
//
// The repair covers only a plan approved at or after the time issue
// publication was last turned on, and none while it is off, so turning the
// setting on never publishes for a task approved earlier
// (req-delivery-and-forge AC-8.3; component-git-delivery).
//
// The store selects only the tasks the loop can act on. Its rule reads
// terminal tasks where it needs them: a start-over retires its task, so every
// superseded task stays a candidate for the pull-request close it queues, and
// an approved plan without a lifecycle row is repaired whatever the task's
// state. Every other task skipped the loop body before the store read
// narrowed (component-runtime, Reconcile tick; component-persistence).
func (d *Dispatcher) ReconcileGitHubLifecycles(ctx context.Context) (int, error) {
	cfg, err := d.currentConfig(ctx)
	if err != nil {
		return 0, err
	}
	var approvedSince *time.Time
	if since, on := cfg.IssuePublication.ApprovedSince(); on {
		approvedSince = &since
	}
	taskIDs, err := d.Store.ListGitHubLifecycleReconcileTaskIDs(ctx, approvedSince)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, taskID := range taskIDs {
		task, err := d.Store.GetTask(ctx, taskID)
		if err != nil {
			return repaired, err
		}
		if task.SupersededBy != "" {
			if err = d.queueStartedOverPR(ctx, task); err != nil {
				return repaired, err
			}
		}
		spec, ok, getErr := d.Store.GetLatestSpecVersion(ctx, task.ID)
		if getErr != nil {
			return repaired, getErr
		}
		if !ok || !spec.Approved || approvedSince == nil || spec.ApprovedAt.Before(*approvedSince) {
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
