package monitor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// listedCommit is one entry of GitHub's commit list, whose order the tests
// write newest first as GitHub returns it.
func listedCommit(sha, message string, parents ...string) string {
	quoted := make([]string, 0, len(parents))
	for _, parent := range parents {
		quoted = append(quoted, fmt.Sprintf(`{"sha":%q}`, parent))
	}
	return fmt.Sprintf(`{"sha":%q,"html_url":"https://example/%s","parents":[%s],"commit":{"message":%q,"committer":{"date":"2026-11-05T12:00:00Z"}}}`,
		sha, sha, strings.Join(quoted, ","), message)
}

// firstParentForge scripts the monitor's GitHub reads and records them. A
// commit with no scripted pulls has none, as a lagging lookup answers.
type firstParentForge struct {
	commits  []string
	pulls    map[string]string
	details  map[int]string
	checks   map[string]string
	requests []string
}

func (f *firstParentForge) run(_ context.Context, args ...string) ([]byte, error) {
	request := strings.Join(args, " ")
	f.requests = append(f.requests, request)
	switch {
	case strings.Contains(request, "/commits -f"):
		return []byte("[" + strings.Join(f.commits, ",") + "]"), nil
	case strings.Contains(request, "/check-runs"):
		for sha, checks := range f.checks {
			if strings.Contains(request, "/commits/"+sha+"/check-runs") {
				return []byte(checks), nil
			}
		}
		return nil, fmt.Errorf("unscripted check runs: %s", request)
	case strings.Contains(request, "/compare/"):
		return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
	}
	for number, detail := range f.details {
		if strings.HasSuffix(args[3], fmt.Sprintf("/pulls/%d", number)) {
			return []byte(detail), nil
		}
	}
	if strings.HasSuffix(args[3], "/pulls") {
		for sha, pulls := range f.pulls {
			if strings.Contains(request, "/commits/"+sha+"/pulls") {
				return []byte(pulls), nil
			}
		}
		return []byte(`[]`), nil
	}
	return nil, fmt.Errorf("unscripted request: %s", request)
}

func (f *firstParentForge) requested(fragment string) int {
	count := 0
	for _, request := range f.requests {
		if strings.Contains(request, fragment) {
			count++
		}
	}
	return count
}

type suppressedAudit struct {
	reason, sha string
}

func firstParentSource(forge *firstParentForge, audits *[]suppressedAudit) GitHubSource {
	return GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: "main",
		ResolveTask: resolvePrefixID, Run: forge.run,
		OnSuppressed: func(_ context.Context, payload map[string]any) error {
			reason, _ := payload["reason"].(string)
			sha, _ := payload["commit_sha"].(string)
			*audits = append(*audits, suppressedAudit{reason: reason, sha: sha})
			return nil
		},
	}
}

func observedOccurrences(observations []Observation) []string {
	occurrences := make([]string, 0, len(observations))
	for _, observation := range observations {
		occurrences = append(occurrences, string(observation.Kind)+":"+observation.OccurrenceID)
	}
	sort.Strings(occurrences)
	return occurrences
}

// Every page of the commit read names the configured base, and a source with
// no base refuses to read rather than fall back to GitHub's default branch
// (req-delivery-and-forge AC-4.9).
func TestGitHubSourceRequestCarriesConfiguredBase(t *testing.T) {
	var pages []string
	source := GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: "release/2",
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			request := strings.Join(args, " ")
			switch {
			case strings.Contains(request, "/commits -f"):
				pages = append(pages, request)
				if len(pages) > 1 {
					return []byte("[" + listedCommit("sha-100", "manual") + "]"), nil
				}
				commits := make([]string, 0, 100)
				for i := range 100 {
					commits = append(commits, listedCommit(fmt.Sprintf("sha-%d", i), "manual", fmt.Sprintf("sha-%d", i+1)))
				}
				return []byte("[" + strings.Join(commits, ",") + "]"), nil
			case strings.Contains(request, "/pulls"):
				return []byte(`[]`), nil
			case strings.Contains(request, "/compare/"):
				return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
			}
			return nil, fmt.Errorf("unscripted request: %s", request)
		},
	}
	observations, err := source.Observations(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 101 {
		t.Fatalf("observations=%d, want the whole chain across both pages", len(observations))
	}
	if len(pages) != 2 {
		t.Fatalf("commit pages=%d", len(pages))
	}
	for _, page := range pages {
		if !strings.Contains(page, "-f sha=release/2") {
			t.Fatalf("page does not name the configured base: %s", page)
		}
	}
	for _, base := range []string{"", "  "} {
		reads := 0
		source := GitHubSource{Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: base,
			Run: func(context.Context, ...string) ([]byte, error) { reads++; return []byte(`[]`), nil }}
		if _, err := source.Observations(t.Context(), time.Now()); err == nil || reads != 0 {
			t.Fatalf("base %q: err=%v reads=%d, want a refusal with no read", base, err, reads)
		}
	}
}

// A lookup lagging on the merge's second parent leaves its branch commits
// with no merged pull request. They stay out of direct_push, the audit names
// each, and they cost no comparison or hints read (req-260811-228be6 AC-4.1;
// task 261104-c2a830).
func TestGitHubSourceLaggingLookupOnSecondParentIsNotADirectPush(t *testing.T) {
	commits := []string{
		listedCommit("merge", "Merge pull request #9", "main-before", "branch-head"),
		listedCommit("branch-head", "second", "branch-first"),
		listedCommit("branch-first", "first", "main-before"),
	}
	t.Run("recorded merge", func(t *testing.T) {
		forge := &firstParentForge{commits: commits}
		var audits []suppressedAudit
		source := firstParentSource(forge, &audits)
		source.MergeCommitState = func(_ context.Context, _, sha string, _ time.Time) (MergeState, string, error) {
			if sha != "merge" {
				t.Errorf("merge state asked about %s", sha)
			}
			return MergeRecorded, "t1", nil
		}
		hints := 0
		source.LoadHints = func(_ context.Context, sha string) (*HintContext, error) {
			hints++
			if sha != "merge" {
				t.Errorf("hints read for %s", sha)
			}
			return nil, nil
		}
		observations, err := source.Observations(t.Context(), time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(observations) != 0 {
			t.Fatalf("observations=%+v", observations)
		}
		want := []suppressedAudit{{"conveyor_merge_recorded", ""}, {"outside_first_parent", "branch-head"}, {"outside_first_parent", "branch-first"}}
		if audits[0].reason != want[0].reason || !reflect.DeepEqual(audits[1:], want[1:]) {
			t.Fatalf("audits=%+v", audits)
		}
		if forge.requested("/compare/") != 0 || hints != 1 {
			t.Fatalf("branch commits were read: compares=%d hints=%d", forge.requested("/compare/"), hints)
		}
		// Reconciliation still looks each one up.
		for _, sha := range []string{"merge", "branch-head", "branch-first"} {
			if forge.requested("/commits/"+sha+"/pulls") != 1 {
				t.Fatalf("no pull lookup for %s: %v", sha, forge.requests)
			}
		}
	})
	// An unrecorded merge is the one change: its first-parent comparison
	// holds the whole branch, so skipping the branch commits loses nothing.
	t.Run("unrecorded merge stays visible with the branch's diff", func(t *testing.T) {
		forge := &firstParentForge{commits: commits}
		var audits []suppressedAudit
		observations, err := firstParentSource(forge, &audits).Observations(t.Context(), time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if got := observedOccurrences(observations); !reflect.DeepEqual(got, []string{"direct_push:merge"}) {
			t.Fatalf("observations=%v", got)
		}
		if forge.requested("compare/main-before...merge") != 1 || len(observations[0].ChangedPaths) == 0 {
			t.Fatalf("merge lost its comparison: requests=%v observation=%+v", forge.requests, observations[0])
		}
	})
}

// A fast-forward onto a recorded head leaves that head on first-parent
// history, and a recorded head_sha isn't merge proof, so it stays visible
// (req-260811-228be6 AC-4.3; component-monitor-drift invariant 11).
func TestGitHubSourceFastForwardOntoRecordedHeadStaysVisible(t *testing.T) {
	recorded := []core.Event{{Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{
		"repository": "acme/conveyor", "pull_request": 9, "head_sha": "recorded-head", "merge_commit_sha": "landed",
	})}}
	forge := &firstParentForge{commits: []string{
		listedCommit("recorded-head", "feature", "branch-first"),
		listedCommit("branch-first", "first", "main-before"),
	}}
	var audits []suppressedAudit
	source := firstParentSource(forge, &audits)
	source.MergeCommitState = func(_ context.Context, slug, sha string, _ time.Time) (MergeState, string, error) {
		for _, event := range recorded {
			if RecordedMergeCommit(event, slug, sha) {
				return MergeRecorded, "t1", nil
			}
		}
		return MergeNone, "", nil
	}
	observations, err := source.Observations(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"direct_push:branch-first", "direct_push:recorded-head"}
	if got := observedOccurrences(observations); !reflect.DeepEqual(got, want) || len(audits) != 0 {
		t.Fatalf("observations=%v audits=%+v", got, audits)
	}
	if !RecordedMergeCommit(recorded[0], "acme/conveyor", "landed") {
		t.Fatal("the recorded merge commit itself no longer matches")
	}
}

// A branch commit of an accepted pull request is still reconciled, but its
// failed check counts for nothing and its runs aren't read, while the merge
// commit's own failure and recovery stay visible (req-delivery-and-forge
// AC-4.6; component-monitor-drift invariant 10).
func TestGitHubSourceBranchCommitCheckFailureCountsForNothing(t *testing.T) {
	pull := `{"number":9,"html_url":"https://example/pr/9","merged_at":"2026-11-05T12:00:00Z","merged_by":{"login":"operator"},"merge_commit_sha":"merge","head":{"ref":"conveyor/task-known","sha":"branch-head"}}`
	failed := `{"check_runs":[{"id":7,"name":"ci","html_url":"https://example/check/7","conclusion":"failure","run_attempt":1}]}`
	for _, test := range []struct {
		name, mergeChecks string
		want              []string
	}{
		{"merge commit fails too", failed, []string{"post_merge_failure:commit:merge:attempt:1"}},
		{"merge commit recovers", `{"check_runs":[{"id":8,"conclusion":"success","run_attempt":1}]}`, []string{"post_merge_failure:recovery:commit:merge"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			forge := &firstParentForge{
				commits: []string{
					listedCommit("merge", "Merge pull request #9", "main-before", "branch-head"),
					listedCommit("branch-head", "second", "main-before"),
				},
				pulls:   map[string]string{"merge": "[" + pull + "]", "branch-head": "[" + pull + "]"},
				details: map[int]string{9: pull},
				checks:  map[string]string{"merge": test.mergeChecks, "branch-head": failed},
			}
			var audits []suppressedAudit
			source := firstParentSource(forge, &audits)
			reconciled := 0
			source.ReconcileMerged = func(context.Context, string, githubtrigger.PullRequest) (bool, error) {
				reconciled++
				return true, nil
			}
			observations, err := source.Observations(t.Context(), time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if got := observedOccurrences(observations); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("observations=%v want %v", got, test.want)
			}
			if reconciled != 2 {
				t.Fatalf("reconciled=%d, want both commits", reconciled)
			}
			if forge.requested("/commits/branch-head/check-runs") != 0 {
				t.Fatalf("read the branch commit's runs: %v", forge.requests)
			}
			if !slices.Contains(audits, suppressedAudit{"outside_first_parent", "branch-head"}) {
				t.Fatalf("audits=%+v", audits)
			}
		})
	}
}

// A revert message on a branch commit counts for nothing, in any case, while
// a revert on first-parent history is still a revert (req-delivery-and-forge
// AC-4.7).
func TestGitHubSourceBranchCommitRevertMessageCountsForNothing(t *testing.T) {
	forge := &firstParentForge{commits: []string{
		listedCommit("real-revert", `Revert "feature"`, "merge"),
		listedCommit("merge", "Merge pull request #9", "main-before", "branch-revert"),
		listedCommit("branch-revert", `REVERT "experiment"`, "branch-first"),
		listedCommit("branch-first", `revert "older"`, "main-before"),
	}}
	var audits []suppressedAudit
	source := firstParentSource(forge, &audits)
	source.MergeCommitState = func(_ context.Context, _, sha string, _ time.Time) (MergeState, string, error) {
		if sha != "merge" {
			t.Errorf("merge state asked about %s", sha)
		}
		return MergeRecorded, "t1", nil
	}
	observations, err := source.Observations(t.Context(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := observedOccurrences(observations); !reflect.DeepEqual(got, []string{"revert:real-revert"}) {
		t.Fatalf("observations=%v", got)
	}
	for _, sha := range []string{"branch-revert", "branch-first"} {
		if !slices.Contains(audits, suppressedAudit{"outside_first_parent", sha}) {
			t.Fatalf("no audit for %s: %+v", sha, audits)
		}
	}
}

// A chain that breaks on a commit older than the window must not silently
// drop the first-parent commits listed behind it. Only a commit proven to be
// reached through another parent is skipped (component-monitor-drift
// invariants 4 and 9; req-delivery-and-forge REQ-4).
func TestGitHubSourceBrokenChainFailsTowardVisibility(t *testing.T) {
	for _, test := range []struct {
		name    string
		commits []string
		want    []string
		skipped []string
	}{
		{
			name: "commits behind an old-dated first parent stay visible",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("behind", "manual", "behind-older"),
				listedCommit("behind-older", "manual", "unlisted"),
			},
			want: []string{"direct_push:behind", "direct_push:behind-older", "direct_push:tip"},
		},
		{
			name: "a revert behind the break stays a revert",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("behind", `Revert "feature"`, "unlisted"),
			},
			want: []string{"direct_push:tip", "revert:behind"},
		},
		{
			name: "a merge behind the break still owns its branch",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("behind-merge", "Merge branch", "behind-first", "behind-branch"),
				listedCommit("behind-first", "manual", "unlisted"),
				listedCommit("behind-branch", "branch", "unlisted"),
			},
			want:    []string{"direct_push:behind-first", "direct_push:behind-merge", "direct_push:tip"},
			skipped: []string{"behind-branch"},
		},
		{
			name: "a commit reached only through an unlisted second parent stays visible",
			commits: []string{
				listedCommit("merge", "Merge branch", "main-before", "old-dated-branch"),
				listedCommit("behind-branch", "branch", "unlisted"),
			},
			want: []string{"direct_push:behind-branch", "direct_push:merge"},
		},
		{
			name: "an ordinary window end skips the merged branch",
			commits: []string{
				listedCommit("merge", "Merge branch", "main-before", "branch-head"),
				listedCommit("branch-head", "branch", "branch-older-than-window"),
			},
			want:    []string{"direct_push:merge"},
			skipped: []string{"branch-head"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			forge := &firstParentForge{commits: test.commits}
			var audits []suppressedAudit
			observations, err := firstParentSource(forge, &audits).Observations(t.Context(), time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if got := observedOccurrences(observations); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("observations=%v want %v", got, test.want)
			}
			var skipped []string
			for _, audit := range audits {
				if audit.reason == "outside_first_parent" {
					skipped = append(skipped, audit.sha)
				}
			}
			if !slices.Equal(skipped, test.skipped) {
				t.Fatalf("skipped=%v want %v", skipped, test.skipped)
			}
		})
	}
}

// A skipped commit whose audit can't be written fails the pass instead of
// leaving the skip unrecorded, so the next pass reads the commit again
// (req-260811-228be6 AC-4.4; req-delivery-and-forge AC-4.5).
func TestGitHubSourceOutsideFirstParentAuditFailureFailsThePass(t *testing.T) {
	sentinel := errors.New("store unavailable")
	forge := &firstParentForge{commits: []string{
		listedCommit("merge", "Merge branch", "main-before", "branch-head"),
		listedCommit("branch-head", "branch", "main-before"),
	}}
	var audits []suppressedAudit
	source := firstParentSource(forge, &audits)
	source.OnSuppressed = func(_ context.Context, payload map[string]any) error {
		if payload["reason"] == "outside_first_parent" {
			return sentinel
		}
		return nil
	}
	observations, err := source.Observations(t.Context(), time.Now().Add(-time.Hour))
	if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "branch-head") || observations != nil {
		t.Fatalf("observations=%+v err=%v", observations, err)
	}
}

// A failed lookup on a branch commit fails the pass too: reconciliation must
// still see it, and the filter must not mask the failure.
func TestGitHubSourceBranchCommitLookupFailureFailsThePass(t *testing.T) {
	forge := &firstParentForge{commits: []string{
		listedCommit("merge", "Merge branch", "main-before", "branch-head"),
		listedCommit("branch-head", "branch", "main-before"),
	}}
	source := GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor", Base: "main",
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "/commits/branch-head/pulls") {
				return nil, githubtrigger.StatusError(502, errors.New("bad gateway"))
			}
			return forge.run(ctx, args...)
		},
	}
	if _, err := source.Observations(t.Context(), time.Now().Add(-time.Hour)); err == nil || githubtrigger.ErrorCategory(err) == "" {
		t.Fatalf("err=%v, want a categorized forge error", err)
	}
}

func TestFirstParentHistory(t *testing.T) {
	commit := func(sha string, parents ...string) githubCommit {
		listed := githubCommit{SHA: sha}
		for _, parent := range parents {
			listed.Parents = append(listed.Parents, struct {
				SHA string `json:"sha"`
			}{SHA: parent})
		}
		return listed
	}
	for _, test := range []struct {
		name    string
		commits []githubCommit
		want    []string
	}{
		{name: "empty"},
		{name: "root commit", commits: []githubCommit{commit("root")}, want: []string{"root"}},
		{name: "linear", commits: []githubCommit{commit("c", "b"), commit("b", "a"), commit("a", "unlisted")}, want: []string{"a", "b", "c"}},
		{
			name:    "octopus merge",
			commits: []githubCommit{commit("m", "first", "two", "three"), commit("first", "x"), commit("two", "x"), commit("three", "x")},
			want:    []string{"first", "m"},
		},
		{
			name:    "merge of a merge stays outside through every parent",
			commits: []githubCommit{commit("m", "x", "inner"), commit("inner", "left", "right"), commit("left", "x"), commit("right", "x")},
			want:    []string{"m"},
		},
		{
			name:    "base merged into the branch keeps the base commit on the chain",
			commits: []githubCommit{commit("m", "y", "branch-merge"), commit("branch-merge", "branch", "y"), commit("y", "x"), commit("branch", "x")},
			want:    []string{"m", "y"},
		},
		{
			name:    "a branch commit listed before the first parent it branched from",
			commits: []githubCommit{commit("m", "y", "branch"), commit("branch", "y"), commit("y", "x")},
			want:    []string{"m", "y"},
		},
		{
			name:    "a commit a shifted page lists twice",
			commits: []githubCommit{commit("c", "b"), commit("b", "a"), commit("b", "a"), commit("a")},
			want:    []string{"a", "b", "c"},
		},
		{
			name:    "a parent cycle in a forged response ends",
			commits: []githubCommit{commit("a", "b"), commit("b", "a", "c"), commit("c", "c")},
			want:    []string{"a", "b"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := make([]string, 0, len(test.commits))
			for sha := range firstParentHistory(test.commits) {
				got = append(got, sha)
			}
			sort.Strings(got)
			if !slices.Equal(got, test.want) && len(got)+len(test.want) != 0 {
				t.Fatalf("first-parent=%v want %v", got, test.want)
			}
		})
	}
}
