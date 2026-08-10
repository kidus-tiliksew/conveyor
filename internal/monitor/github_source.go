package monitor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type CommandRunner func(context.Context, ...string) ([]byte, error)

type GitHubSource struct {
	WorkspaceID  string
	Repository   string
	GitHubSlug   string
	Run          CommandRunner
	KnownLineage func(taskID string, pullRequestNumber int, headSHA string) bool
	LoadHints    func(context.Context, string) (*HintContext, error)
	OnSuppressed func(context.Context, map[string]any) error
}

type githubCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

type githubPull struct {
	Number   int        `json:"number"`
	HTMLURL  string     `json:"html_url"`
	MergedAt *time.Time `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

type githubCheckRuns struct {
	CheckRuns []struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HTMLURL    string `json:"html_url"`
		Conclusion string `json:"conclusion"`
		RunAttempt int    `json:"run_attempt"`
	} `json:"check_runs"`
}

type failedCheckRun struct {
	ID   int64
	Name string
	URL  string
}

// RecordedLineage verifies that a branch-shaped pull request is actually the
// pull request Conveyor recorded for this task in this repository. A task ID
// embedded in an unrelated external PR is not lineage (design-monitor-drift).
func RecordedLineage(task core.Task, events []core.Event, repository, githubSlug, taskID string, pullRequestNumber int, headSHA string) bool {
	if task.ID != taskID || task.Repo != repository || task.Branch != "conveyor/task-"+taskID ||
		strings.TrimSpace(headSHA) == "" ||
		(task.GitHub != nil && task.GitHub.Repository != githubSlug) {
		return false
	}
	for _, event := range events {
		if event.Kind != "pull_request.opened" {
			continue
		}
		var opened struct {
			Number  int    `json:"number"`
			HeadSHA string `json:"head_sha"`
		}
		if json.Unmarshal(event.Payload, &opened) == nil &&
			opened.Number == pullRequestNumber && opened.HeadSHA == headSHA {
			return true
		}
	}
	return false
}

func (s GitHubSource) Observations(ctx context.Context, since time.Time) ([]Observation, error) {
	if s.Run == nil {
		s.Run = runGH
	}
	if strings.TrimSpace(s.Repository) == "" || strings.TrimSpace(s.GitHubSlug) == "" {
		return nil, fmt.Errorf("monitor GitHub repository name and slug are required")
	}
	var commits []githubCommit
	for page := 1; ; page++ {
		raw, err := s.Run(ctx, "api", "--method", "GET",
			"repos/"+s.GitHubSlug+"/commits",
			"-f", "since="+since.UTC().Format(time.RFC3339),
			"-f", "per_page=100", "-f", "page="+strconv.Itoa(page))
		if err != nil {
			return nil, githubtrigger.CategorizeError(err)
		}
		var batch []githubCommit
		if err = json.Unmarshal(raw, &batch); err != nil {
			return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor commits: %w", err)}
		}
		commits = append(commits, batch...)
		if len(batch) < 100 {
			break
		}
	}
	var observations []Observation
	seen := make(map[string]struct{})
	appendObservation := func(observation Observation) {
		key := string(observation.Kind) + ":" + observation.OccurrenceID
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		observations = append(observations, observation)
	}
	for _, commit := range commits {
		pulls, pullErr := s.pulls(ctx, commit.SHA)
		if pullErr != nil {
			return nil, pullErr
		}
		lineaged := false
		for _, pull := range pulls {
			taskID, ok := strings.CutPrefix(pull.Head.Ref, "conveyor/task-")
			if ok && pull.MergedAt != nil && s.KnownLineage != nil &&
				s.KnownLineage(taskID, pull.Number, pull.Head.SHA) {
				lineaged = true
				break
			}
		}
		var hints *HintContext
		if s.LoadHints != nil {
			loadedHints, loadErr := s.LoadHints(ctx, commit.SHA)
			if loadErr != nil {
				return nil, loadErr
			}
			hints = loadedHints
		}
		if lineaged {
			checks, checkErr := s.checks(ctx, commit.SHA)
			if checkErr != nil {
				return nil, checkErr
			}
			byAttempt := make(map[int][]failedCheckRun)
			green := false
			for _, check := range checks.CheckRuns {
				if check.Conclusion != "failure" && check.Conclusion != "timed_out" {
					green = green || check.Conclusion == "success"
					if s.OnSuppressed != nil {
						_ = s.OnSuppressed(ctx, map[string]any{
							"reason": "check_not_actionable", "repository": s.Repository,
							"commit_sha": commit.SHA, "check_run_id": check.ID,
							"conclusion": check.Conclusion,
						})
					}
					continue
				}
				attempt := check.RunAttempt
				if attempt <= 0 {
					attempt = 1
				}
				byAttempt[attempt] = append(byAttempt[attempt], failedCheckRun{
					ID: check.ID, Name: strings.TrimSpace(check.Name), URL: check.HTMLURL,
				})
			}
			attempts := make([]int, 0, len(byAttempt))
			for attempt := range byAttempt {
				attempts = append(attempts, attempt)
			}
			sort.Ints(attempts)
			for _, attempt := range attempts {
				failed := byAttempt[attempt]
				sort.Slice(failed, func(i, j int) bool { return failed[i].ID < failed[j].ID })
				checkIDs := make([]string, 0, len(failed))
				checkDetails := make([]string, 0, len(failed))
				for _, check := range failed {
					id := strconv.FormatInt(check.ID, 10)
					checkIDs = append(checkIDs, id)
					name := check.Name
					if name == "" {
						name = "unnamed check"
					}
					checkDetails = append(checkDetails, fmt.Sprintf("- %s (check run %s): %s", name, id, check.URL))
				}
				appendObservation(Observation{
					WorkspaceID: s.WorkspaceID, Repository: s.Repository,
					Kind:         PostMergeFailure,
					OccurrenceID: "commit:" + commit.SHA + ":attempt:" + strconv.Itoa(attempt),
					SourceURL:    failed[0].URL, CommitSHA: commit.SHA,
					CheckRunID: strings.Join(checkIDs, ","),
					Attempt:    attempt,
					ObservedAt: commit.Commit.Committer.Date, Hints: hints,
					Context: map[string]string{"failed_check_runs": strings.Join(checkDetails, "\n")},
				})
			}
			if len(byAttempt) == 0 && green {
				appendObservation(Observation{
					WorkspaceID: s.WorkspaceID, Repository: s.Repository,
					Kind: PostMergeFailure, OccurrenceID: "recovery:commit:" + commit.SHA,
					SourceURL: commit.HTMLURL, CommitSHA: commit.SHA,
					ObservedAt: commit.Commit.Committer.Date, RecoveryObserved: true,
				})
			}
			continue
		}
		kind, sourceURL, occurrenceID, prNumber := DirectPush, commit.HTMLURL, commit.SHA, 0
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(commit.Commit.Message)), "revert") {
			kind = Revert
		} else {
			for _, pull := range pulls {
				if pull.MergedAt != nil {
					kind, sourceURL, prNumber = ExternalPRMerge, pull.HTMLURL, pull.Number
					occurrenceID = "pr:" + strconv.Itoa(pull.Number)
					break
				}
			}
		}
		if kind == DirectPush && len(commit.Parents) > 0 {
			differs, diffErr := s.differsFromFirstParent(ctx, commit.Parents[0].SHA, commit.SHA)
			if diffErr != nil {
				return nil, diffErr
			}
			if !differs {
				if s.OnSuppressed != nil {
					_ = s.OnSuppressed(ctx, map[string]any{
						"reason": "first_parent_empty", "repository": s.Repository,
						"commit_sha": commit.SHA, "first_parent_sha": commit.Parents[0].SHA,
					})
				}
				continue
			}
		}
		appendObservation(Observation{
			WorkspaceID: s.WorkspaceID, Repository: s.Repository, Kind: kind,
			OccurrenceID: occurrenceID, SourceURL: sourceURL, CommitSHA: commit.SHA,
			PullRequestNumber: prNumber, ObservedAt: commit.Commit.Committer.Date, Hints: hints,
		})
	}
	return observations, nil
}

func (s GitHubSource) differsFromFirstParent(ctx context.Context, parentSHA, commitSHA string) (bool, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/compare/"+parentSHA+"..."+commitSHA,
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return false, githubtrigger.CategorizeError(err)
	}
	var comparison struct {
		Files *[]json.RawMessage `json:"files"`
	}
	if err = json.Unmarshal(raw, &comparison); err != nil {
		return false, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse first-parent comparison: %w", err)}
	}
	if comparison.Files == nil {
		return false, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse first-parent comparison: files are missing")}
	}
	return len(*comparison.Files) > 0, nil
}

func (s GitHubSource) pulls(ctx context.Context, sha string) ([]githubPull, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/commits/"+sha+"/pulls",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return nil, githubtrigger.CategorizeError(err)
	}
	var pulls []githubPull
	if err = json.Unmarshal(raw, &pulls); err != nil {
		return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor pull requests: %w", err)}
	}
	return pulls, nil
}

func (s GitHubSource) checks(ctx context.Context, sha string) (githubCheckRuns, error) {
	raw, err := s.Run(ctx, "api", "--method", "GET",
		"repos/"+s.GitHubSlug+"/commits/"+sha+"/check-runs",
		"-H", "Accept: application/vnd.github+json")
	if err != nil {
		return githubCheckRuns{}, githubtrigger.CategorizeError(err)
	}
	var checks githubCheckRuns
	if err = json.Unmarshal(raw, &checks); err != nil {
		return githubCheckRuns{}, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse monitor check runs: %w", err)}
	}
	return checks, nil
}

func runGH(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// FetchGitHubHints reads the advisory file from the exact observed revision.
// A missing file means the repository supplies no hints; every other forge or
// validation failure is operator-visible and fails closed.
func FetchGitHubHints(ctx context.Context, slug, revision string, run CommandRunner) (*HintContext, error) {
	if run == nil {
		run = runGH
	}
	raw, err := run(ctx, "api", "--method", "GET",
		"repos/"+slug+"/contents/.conveyor/hints.yaml", "-f", "ref="+revision)
	if err != nil {
		detail := strings.ToLower(err.Error())
		if strings.Contains(detail, "404") || strings.Contains(detail, "not found") {
			return nil, nil
		}
		return nil, githubtrigger.CategorizeError(err)
	}
	var response struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err = json.Unmarshal(raw, &response); err != nil || response.Encoding != "base64" {
		return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("parse advisory hints response")}
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(response.Content, "\n", ""))
	if err != nil {
		return nil, &githubtrigger.Error{Category: githubtrigger.ForgeResponse, Err: fmt.Errorf("decode advisory hints: %w", err)}
	}
	hints, err := ParseHints(data, revision)
	if err != nil {
		return nil, err
	}
	return &hints, nil
}
