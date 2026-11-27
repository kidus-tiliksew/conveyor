package feedback

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Repository is one repository of the workspace configuration: its name and
// its GitHub slug.
type Repository struct {
	Name string
	Slug string
}

// CompareResult is GitHub's comparison of base with head. Commits lists
// commits that arrived in head when the caller asked for them, and Complete
// is true only when it lists every one.
type CompareResult struct {
	Status   string
	Commits  []string
	Complete bool
}

// ErrNoGitHubApp marks a forge whose workspace has no usable GitHub App. A
// version that a claim lets resolve keeps reason no_github_app.
var ErrNoGitHubApp = errors.New("the workspace has no usable GitHub App")

// ReadyForge is a forge that can report, before any request, that it
// cannot read GitHub, for example because the installation token request
// failed. The background work then charges no comparison for a request that
// cannot start.
type ReadyForge interface {
	Forge
	Ready(ctx context.Context) error
}

// Forge is the GitHub boundary the background work reads through. Its
// implementation binds the workspace App's installation token
// (component-production-feedback invariant 9; DEC-59(3); DEC-95). It runs no
// git command and holds no clone.
type Forge interface {
	// ReadCommit returns the full commit ID that ref names: a commit ID,
	// heads/<branch>, or tags/<tag>. A ref the repository does not hold
	// answers an error that github.ErrCommitNotFound matches.
	ReadCommit(ctx context.Context, slug, ref string) (string, error)
	// Compare compares base with head and lists up to arrived commits that
	// head holds and base lacks.
	Compare(ctx context.Context, slug, base, head string, arrived int) (CompareResult, error)
	// ReadLineChange asks which commit last changed a line of a file as of
	// a full commit ID, and which merged pull requests of the repository
	// GitHub lists for that commit, in one request
	// (component-feedback-reports invariant 12). A file the commit does not
	// hold answers FileFound false. Every other incomplete answer is a
	// categorized forge failure.
	ReadLineChange(ctx context.Context, slug, commit, path string, line int) (github.LineChange, error)
}

// ResolveRequest is one version to resolve.
type ResolveRequest struct {
	Version      store.FeedbackVersion
	Service      string
	Rule         *store.EnvironmentVersionRule
	Repositories []Repository
}

// Resolution is the outcome of one attempt. A resolved version holds the
// repository name and the full commit ID. Otherwise Reason names why, and
// RetryAt is a later time than the 10-minute rule that GitHub set for the
// next request, when it set one.
type Resolution struct {
	Repository string
	Commit     string
	Reason     string
	RetryAt    time.Time
}

// Resolve resolves one reported version (component-production-feedback
// invariant 7; req-production-feedback AC-1.4; req-environments AC-1.2). A
// stated commit comes first. Without one the environment's version rule reads
// a commit or a tag from the version. A named repository is the only
// candidate; otherwise the version must match in exactly one configured
// repository. Two matches are ambiguous, even for one commit. A forge failure
// in a candidate proves neither absence nor uniqueness, so it leaves the
// version unresolved with the failure's category.
func Resolve(ctx context.Context, forge Forge, request ResolveRequest) Resolution {
	candidates := []Repository{}
	for _, repository := range request.Repositories {
		if request.Version.Repository != "" && repository.Name != request.Version.Repository {
			continue
		}
		if strings.TrimSpace(repository.Slug) != "" {
			candidates = append(candidates, repository)
		}
	}
	if len(candidates) == 0 {
		return Resolution{Reason: store.FeedbackReasonNoRepository}
	}
	ref, prefix := "", ""
	switch {
	case request.Version.Commit != "":
		prefix = strings.ToLower(request.Version.Commit)
		ref = prefix
	case request.Rule == nil:
		return Resolution{Reason: store.FeedbackReasonNoVersionRule}
	default:
		target, ok, err := ApplyVersionRule(*request.Rule, request.Version.Version, request.Service)
		if err != nil || !ok {
			return Resolution{Reason: store.FeedbackReasonRuleMismatch}
		}
		if target.Commit != "" {
			prefix, ref = target.Commit, target.Commit
		} else {
			ref = "tags/" + target.Tag
		}
	}
	var matched []Resolution
	var failure *Resolution
	for _, candidate := range candidates {
		commit, err := forge.ReadCommit(ctx, candidate.Slug, ref)
		switch {
		case err == nil && store.FeedbackCommitID(commit) && strings.HasPrefix(commit, prefix):
			matched = append(matched, Resolution{Repository: candidate.Name, Commit: commit})
		case err == nil, errors.Is(err, github.ErrCommitNotFound):
			// The repository holds no such commit or tag.
		default:
			outcome := forgeFailure(err)
			if failure == nil || outcome.RetryAt.After(failure.RetryAt) {
				failure = &outcome
			}
		}
	}
	switch {
	case len(matched) > 1:
		return Resolution{Reason: store.FeedbackReasonAmbiguous}
	case failure != nil:
		return *failure
	case len(matched) == 1:
		return matched[0]
	}
	return Resolution{Reason: store.FeedbackReasonNoMatch}
}

// forgeFailure is the unresolved outcome of a forge failure: its category,
// and the time GitHub allows the next request when it named one. A failure
// to get the installation token is a forge failure too, so it records the
// same outcome under the version's claim.
func forgeFailure(err error) Resolution {
	if errors.Is(err, ErrNoGitHubApp) {
		return Resolution{Reason: store.FeedbackReasonNoGitHubApp}
	}
	category := github.ErrorCategory(github.CategorizeError(err))
	if category == "" {
		category = github.ForgeRequest
	}
	retryAt, _ := github.RateLimitRetryAt(err)
	return Resolution{Reason: string(category), RetryAt: retryAt}
}
