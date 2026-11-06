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
	commits []string
	pulls   map[string]string
	details map[int]string
	checks  map[string]string
	// ancestry answers compare/<stop>...<commit> for a commit outside the
	// chain, by "<stop>...<commit>". An unscripted one is an error, so a test
	// states every ancestry read it expects.
	ancestry map[string]string
	// unlisted answers GET commits/<sha> for a commit below the window with
	// its parents. An unscripted one is an error.
	unlisted map[string][]string
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
	case strings.Contains(request, "per_page=1 -H"):
		for pair, status := range f.ancestry {
			if strings.Contains(request, "/compare/"+pair+" ") {
				return []byte(fmt.Sprintf(`{"status":%q}`, status)), nil
			}
		}
		return nil, fmt.Errorf("unscripted ancestry read: %s", request)
	case strings.Contains(request, "/compare/"):
		return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
	}
	for number, detail := range f.details {
		if strings.HasSuffix(args[3], fmt.Sprintf("/pulls/%d", number)) {
			return []byte(detail), nil
		}
	}
	for sha, parents := range f.unlisted {
		if strings.HasSuffix(args[3], "/commits/"+sha) {
			return []byte(listedCommit(sha, "older than the window", parents...)), nil
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

// ancestryReads counts the comparisons read for ancestry, not for a commit's
// first-parent paths.
func (f *firstParentForge) ancestryReads() int {
	return f.requested("per_page=1 -H")
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
	observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
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
		if _, err := windowObservations(t.Context(), source, time.Now()); err == nil || reads != 0 {
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
		observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
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
		if forge.ancestryReads() != 0 {
			t.Fatalf("a branch cut at the chain's stop needs no ancestry read: %v", forge.requests)
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
		observations, err := windowObservations(t.Context(), firstParentSource(forge, &audits), time.Now().Add(-time.Hour))
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
	observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
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
			observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
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
	observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
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

// The review's graph: M has parents [X, B], and X and B share the parent D. X
// is older than the window, so the listing is [M, B, D] and the walk stops at
// X. B and D are both reached through M's second parent, yet D is on
// first-parent history and B isn't. D's direct push stays observed with its
// paths, and B stays suppressed because ancestry shows it is branch-only
// (req-260811-228be6 AC-4.3; req-delivery-and-forge REQ-4, AC-4.4).
func TestGitHubSourceSharedAncestryBehindMissingFirstParentKeepsDirectPush(t *testing.T) {
	dated := func(sha, message, date string, parents ...string) string {
		quoted := make([]string, 0, len(parents))
		for _, parent := range parents {
			quoted = append(quoted, fmt.Sprintf(`{"sha":%q}`, parent))
		}
		return fmt.Sprintf(`{"sha":%q,"html_url":"https://example/%s","parents":[%s],"commit":{"message":%q,"committer":{"date":%q}}}`,
			sha, sha, strings.Join(quoted, ","), message, date)
	}
	forge := &firstParentForge{
		commits: []string{
			dated("M", "Merge pull request #9", "2026-11-05T12:00:00Z", "X", "B"),
			dated("B", "branch work", "2026-11-05T11:00:00Z", "D"),
			dated("D", "direct push", "2026-11-05T10:00:00Z", "A"),
		},
		ancestry: map[string]string{"X...D": "behind", "X...B": "diverged"},
		unlisted: map[string][]string{"X": {"D"}},
	}
	var audits []suppressedAudit
	source := firstParentSource(forge, &audits)
	source.MergeCommitState = func(_ context.Context, _, sha string, _ time.Time) (MergeState, string, error) {
		if sha == "M" {
			return MergeRecorded, "delivering-task", nil
		}
		return MergeNone, "", nil
	}
	since, err := time.Parse(time.RFC3339, "2026-11-05T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	observations, err := windowObservations(t.Context(), source, since)
	if err != nil {
		t.Fatal(err)
	}
	if got := observedOccurrences(observations); !reflect.DeepEqual(got, []string{"direct_push:D"}) {
		t.Fatalf("observations=%v", got)
	}
	if strings.Join(observations[0].ChangedPaths, ",") != "internal/monitor/types.go" || forge.requested("compare/A...D") != 1 {
		t.Fatalf("D lost its governed paths: %+v requests=%v", observations[0], forge.requests)
	}
	if !slices.Contains(audits, suppressedAudit{"outside_first_parent", "B"}) || slices.Contains(audits, suppressedAudit{"outside_first_parent", "D"}) {
		t.Fatalf("audits=%+v", audits)
	}
	if forge.ancestryReads() != 2 || forge.requested("/commits/X ") != 1 {
		t.Fatalf("ancestry reads=%d: %v", forge.ancestryReads(), forge.requests)
	}
}

// A chain that stops at an unlisted commit leaves each other commit open until
// ancestry settles it. Only a commit shown not to be an ancestor of the stop
// is skipped (component-monitor-drift invariants 4 and 9).
func TestGitHubSourceBrokenChainFailsTowardVisibility(t *testing.T) {
	for _, test := range []struct {
		name     string
		commits  []string
		ancestry map[string]string
		unlisted map[string][]string
		want     []string
		skipped  []string
		reads    int
	}{
		{
			name: "commits behind an old-dated first parent stay visible",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("behind", "manual", "behind-older"),
				listedCommit("behind-older", "manual", "unlisted"),
			},
			ancestry: map[string]string{"old-dated...behind-older": "behind", "old-dated...behind": "behind"},
			unlisted: map[string][]string{"old-dated": {"behind"}},
			want:     []string{"direct_push:behind", "direct_push:behind-older", "direct_push:tip"},
			reads:    2,
		},
		{
			name: "a revert behind the break stays a revert",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("behind", `Revert "feature"`, "unlisted"),
			},
			ancestry: map[string]string{"old-dated...behind": "behind"},
			unlisted: map[string][]string{"old-dated": {"behind"}},
			want:     []string{"direct_push:tip", "revert:behind"},
			reads:    1,
		},
		{
			name: "a merge behind the break still owns its branch",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("behind-merge", "Merge branch", "behind-first", "behind-branch"),
				listedCommit("behind-first", "manual", "unlisted"),
				listedCommit("behind-branch", "branch", "cut-point"),
			},
			ancestry: map[string]string{
				"old-dated...behind-branch": "behind", "old-dated...behind-first": "behind", "old-dated...behind-merge": "behind",
				"unlisted...behind-branch": "diverged",
			},
			unlisted: map[string][]string{"old-dated": {"behind-merge"}},
			want:     []string{"direct_push:behind-first", "direct_push:behind-merge", "direct_push:tip"},
			skipped:  []string{"behind-branch"},
			reads:    4,
		},
		{
			name: "a branch behind an old-dated second parent is proven outside",
			commits: []string{
				listedCommit("merge", "Merge branch", "main-before", "old-dated-branch"),
				listedCommit("behind-branch", "branch", "unlisted"),
			},
			ancestry: map[string]string{"main-before...behind-branch": "ahead"},
			want:     []string{"direct_push:merge"},
			skipped:  []string{"behind-branch"},
			reads:    1,
		},
		{
			name: "an identical answer is not proof of exclusion",
			commits: []string{
				listedCommit("merge", "Merge branch", "main-before", "behind-branch"),
				listedCommit("behind-branch", "branch", "unlisted"),
			},
			ancestry: map[string]string{"main-before...behind-branch": "identical"},
			unlisted: map[string][]string{"main-before": {}},
			want:     []string{"direct_push:behind-branch", "direct_push:merge"},
			reads:    1,
		},
		{
			name: "an ancestor through an old merge's second parent is proven outside by the next stop",
			commits: []string{
				listedCommit("tip", "manual", "old-merge"),
				listedCommit("merged-branch", "branch", "unlisted"),
			},
			ancestry: map[string]string{"old-merge...merged-branch": "behind", "old-first...merged-branch": "diverged"},
			unlisted: map[string][]string{"old-merge": {"old-first", "merged-branch"}},
			want:     []string{"direct_push:tip"},
			skipped:  []string{"merged-branch"},
			reads:    2,
		},
		{
			name: "the date order of a shared ancestor and its child can't hide the child",
			commits: []string{
				listedCommit("tip", "manual", "old-dated"),
				listedCommit("parent-newer", "manual", "unlisted"),
				listedCommit("child-older", "manual", "parent-newer"),
			},
			ancestry: map[string]string{"old-dated...child-older": "behind", "old-dated...parent-newer": "behind"},
			unlisted: map[string][]string{"old-dated": {"child-older"}},
			want:     []string{"direct_push:child-older", "direct_push:parent-newer", "direct_push:tip"},
			reads:    2,
		},
		{
			name: "a branch cut before the window costs one read, whatever its length",
			commits: []string{
				listedCommit("merge", "Merge branch", "main-before", "branch-3"),
				listedCommit("branch-3", "branch", "branch-2"),
				listedCommit("branch-2", "branch", "branch-1"),
				listedCommit("branch-1", "branch", "older-than-window"),
			},
			ancestry: map[string]string{"main-before...branch-1": "diverged"},
			want:     []string{"direct_push:merge"},
			skipped:  []string{"branch-3", "branch-2", "branch-1"},
			reads:    1,
		},
		{
			name: "a branch cut from the stop needs no read",
			commits: []string{
				listedCommit("merge", "Merge branch", "main-before", "branch-head"),
				listedCommit("branch-head", "branch", "main-before"),
			},
			want:    []string{"direct_push:merge"},
			skipped: []string{"branch-head"},
		},
		{
			name: "a branch cut from the chain needs no read",
			commits: []string{
				listedCommit("merge", "Merge branch", "first", "branch-head"),
				listedCommit("branch-head", "branch", "first"),
				listedCommit("first", "manual", "main-before"),
			},
			want:    []string{"direct_push:first", "direct_push:merge"},
			skipped: []string{"branch-head"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			forge := &firstParentForge{commits: test.commits, ancestry: test.ancestry, unlisted: test.unlisted}
			var audits []suppressedAudit
			observations, err := windowObservations(t.Context(), firstParentSource(forge, &audits), time.Now().Add(-time.Hour))
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
			if got := forge.ancestryReads(); got != test.reads {
				t.Fatalf("ancestry reads=%d want %d: %v", got, test.reads, forge.requests)
			}
		})
	}
}

// A failed or unreadable ancestry answer classifies nothing. The pass fails
// with its forge category before any commit is judged, so status states the
// failure, and the next pass reads the same commits and files the direct push
// (req-delivery-and-forge AC-4.5; component-monitor-drift invariant 15).
func TestGitHubSourceAncestryReadFailureClassifiesNothing(t *testing.T) {
	commits := []string{
		listedCommit("M", "Merge pull request #9", "X", "B"),
		listedCommit("B", "branch work", "D"),
		listedCommit("D", "direct push", "A"),
	}
	limited := githubtrigger.RateLimitedError(time.Now().Add(time.Hour), errors.New("API rate limit exceeded"))
	badGateway := githubtrigger.StatusError(502, errors.New("bad gateway"))
	const ancestryRead, commitRead = "/compare/X...", "/commits/X "
	for _, test := range []struct {
		name     string
		read     string
		answer   func() ([]byte, error)
		category githubtrigger.ForgeErrorCategory
	}{
		{"ancestry read: forge failure", ancestryRead, func() ([]byte, error) { return nil, badGateway }, githubtrigger.ErrorCategory(badGateway)},
		{"ancestry read: rate limit", ancestryRead, func() ([]byte, error) { return nil, limited }, githubtrigger.ErrorCategory(limited)},
		{"ancestry read: malformed body", ancestryRead, func() ([]byte, error) { return []byte(`not json`), nil }, githubtrigger.ForgeResponse},
		{"ancestry read: no status", ancestryRead, func() ([]byte, error) { return []byte(`{"files":[]}`), nil }, githubtrigger.ForgeResponse},
		{"ancestry read: unknown status", ancestryRead, func() ([]byte, error) { return []byte(`{"status":"unrelated"}`), nil }, githubtrigger.ForgeResponse},
		{"commit read: forge failure", commitRead, func() ([]byte, error) { return nil, badGateway }, githubtrigger.ErrorCategory(badGateway)},
		{"commit read: rate limit", commitRead, func() ([]byte, error) { return nil, limited }, githubtrigger.ErrorCategory(limited)},
		{"commit read: malformed body", commitRead, func() ([]byte, error) { return []byte(`not json`), nil }, githubtrigger.ForgeResponse},
		{"commit read: another commit", commitRead, func() ([]byte, error) { return []byte(`{"sha":"Y","parents":[{"sha":"D"}]}`), nil }, githubtrigger.ForgeResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			forge := &firstParentForge{commits: commits, ancestry: map[string]string{"X...D": "behind", "X...B": "diverged"}, unlisted: map[string][]string{"X": {"D"}}}
			var audits []suppressedAudit
			source := firstParentSource(forge, &audits)
			failing := true
			source.Run = func(ctx context.Context, args ...string) ([]byte, error) {
				if failing && strings.Contains(strings.Join(args, " "), test.read) {
					return test.answer()
				}
				return forge.run(ctx, args...)
			}
			observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
			if err == nil || observations != nil || githubtrigger.ErrorCategory(err) != test.category {
				t.Fatalf("observations=%+v err=%v category=%q want %q", observations, err, githubtrigger.ErrorCategory(err), test.category)
			}
			if len(audits) != 0 || forge.requested("/commits/M/pulls") != 0 {
				t.Fatalf("a commit was judged before the failure settled: audits=%+v requests=%v", audits, forge.requests)
			}
			failing = false
			observations, err = windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if got := observedOccurrences(observations); !reflect.DeepEqual(got, []string{"direct_push:D", "direct_push:M"}) {
				t.Fatalf("retried pass observations=%v", got)
			}
			if !slices.Contains(audits, suppressedAudit{"outside_first_parent", "B"}) {
				t.Fatalf("retried pass audits=%+v", audits)
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
	observations, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour))
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
	if _, err := windowObservations(t.Context(), source, time.Now().Add(-time.Hour)); err == nil || githubtrigger.ErrorCategory(err) == "" {
		t.Fatalf("err=%v, want a categorized forge error", err)
	}
}

func TestFirstParentHistory(t *testing.T) {
	commit := func(sha string, parents ...string) githubCommit {
		listed := githubCommit{SHA: sha}
		for _, parent := range parents {
			listed.Parents = append(listed.Parents, githubCommitParent{SHA: parent})
		}
		return listed
	}
	for _, test := range []struct {
		name      string
		commits   []githubCommit
		ancestors map[string]bool
		unlisted  map[string]string
		want      []string
		reads     int
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
			name:      "the review's graph",
			commits:   []githubCommit{commit("M", "X", "B"), commit("B", "D"), commit("D", "A")},
			ancestors: map[string]bool{"D...X": true, "B...X": false},
			unlisted:  map[string]string{"X": "D"},
			want:      []string{"D", "M"},
			reads:     3,
		},
		{
			name:      "a first-parent child dated older than its parent is not excluded through that parent",
			commits:   []githubCommit{commit("M", "X", "B"), commit("B", "D"), commit("D", "A"), commit("C", "D")},
			ancestors: map[string]bool{"C...X": true, "D...X": true, "B...X": false},
			unlisted:  map[string]string{"X": "C"},
			want:      []string{"C", "D", "M"},
			reads:     4,
		},
		{
			name:      "a walk several commits below the window finds the chain again",
			commits:   []githubCommit{commit("t", "u1"), commit("d", "a")},
			ancestors: map[string]bool{"d...u1": true, "d...u2": true},
			unlisted:  map[string]string{"u1": "u2", "u2": "d"},
			want:      []string{"d", "t"},
			reads:     4,
		},
		{
			name:      "an open commit stays visible when the walk reaches a root",
			commits:   []githubCommit{commit("t", "u1"), commit("d", "a")},
			ancestors: map[string]bool{"d...u1": true},
			unlisted:  map[string]string{"u1": ""},
			want:      []string{"d", "t"},
			reads:     2,
		},
		{
			name:    "a chain reaching the root leaves an unrelated root visible with no read",
			commits: []githubCommit{commit("tip", "root"), commit("root"), commit("other")},
			want:    []string{"other", "root", "tip"},
		},
		{
			name:    "a chain reaching the root excludes a branch cut from it with no read",
			commits: []githubCommit{commit("m", "root", "branch"), commit("branch", "root"), commit("root")},
			want:    []string{"m", "root"},
		},
		{
			name:    "a parent cycle in a forged response ends",
			commits: []githubCommit{commit("a", "b"), commit("b", "a", "c"), commit("c", "c")},
			want:    []string{"a", "b", "c"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			history, err := firstParentHistory(t.Context(), test.commits,
				func(_ context.Context, candidate, descendant string) (bool, error) {
					reads++
					ancestor, scripted := test.ancestors[candidate+"..."+descendant]
					if !scripted {
						t.Errorf("unscripted ancestry read: is %s an ancestor of %s", candidate, descendant)
					}
					return ancestor, nil
				},
				func(_ context.Context, sha string) (string, bool, error) {
					reads++
					parent, scripted := test.unlisted[sha]
					if !scripted {
						t.Errorf("unscripted commit read: %s", sha)
					}
					return parent, parent != "", nil
				})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(test.commits))
			for sha := range history {
				got = append(got, sha)
			}
			sort.Strings(got)
			if !slices.Equal(got, test.want) && len(got)+len(test.want) != 0 {
				t.Fatalf("first-parent=%v want %v", got, test.want)
			}
			if reads != test.reads {
				t.Fatalf("reads=%d want %d", reads, test.reads)
			}
		})
	}
}

// A walk that never settles an open commit ends at the read bound, whatever
// the forge answers, and the commit stays visible rather than being classified
// from an answer the pass never got.
func TestFirstParentHistoryStopsAtTheReadBound(t *testing.T) {
	commits := []githubCommit{
		{SHA: "t", Parents: []githubCommitParent{{SHA: "u0"}}},
		{SHA: "hidden", Parents: []githubCommitParent{{SHA: "a"}}},
	}
	reads := 0
	history, err := firstParentHistory(t.Context(), commits,
		func(context.Context, string, string) (bool, error) { reads++; return true, nil },
		func(_ context.Context, sha string) (string, bool, error) {
			reads++
			return sha + "'", true, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if want := len(commits) + unlistedFirstParentReads; reads != want {
		t.Fatalf("reads=%d want the bound %d", reads, want)
	}
	if _, visible := history["hidden"]; !visible {
		t.Fatalf("the open commit was dropped at the bound: %v", history)
	}
}
