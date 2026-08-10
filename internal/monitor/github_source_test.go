package monitor

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestGitHubSourceClassifiesLineageFailuresAndOutsideChanges(t *testing.T) {
	suppressed := 0
	source := GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor",
		KnownLineage: func(id string, number int, headSHA string) bool {
			return id == "known" && number == 1 && headSHA == "known-head"
		},
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			path := strings.Join(args, " ")
			switch {
			case strings.Contains(path, "/commits -f"):
				return []byte(`[
{"sha":"known-sha","html_url":"https://example/known","commit":{"message":"merge","committer":{"date":"2026-07-28T10:00:00Z"}}},
{"sha":"push-sha","html_url":"https://example/push","commit":{"message":"manual","committer":{"date":"2026-07-28T10:01:00Z"}}},
{"sha":"pr-sha","html_url":"https://example/pr-sha","commit":{"message":"feature","committer":{"date":"2026-07-28T10:02:00Z"}}},
{"sha":"pr-sha-2","html_url":"https://example/pr-sha-2","commit":{"message":"feature part 2","committer":{"date":"2026-07-28T10:02:30Z"}}},
{"sha":"masquerade-sha","html_url":"https://example/masquerade","commit":{"message":"external","committer":{"date":"2026-07-28T10:03:00Z"}}},
{"sha":"revert-sha","html_url":"https://example/revert","commit":{"message":"Revert \"bad\"","committer":{"date":"2026-07-28T10:03:00Z"}}}
]`), nil
			case strings.Contains(path, "known-sha/pulls"):
				return []byte(`[{"number":1,"html_url":"https://example/pr/1","merged_at":"2026-07-28T09:00:00Z","head":{"ref":"conveyor/task-known","sha":"known-head"}}]`), nil
			case strings.Contains(path, "known-sha/check-runs"):
				return []byte(`{"check_runs":[{"id":77,"name":"unit","html_url":"https://example/check/77","conclusion":"failure","run_attempt":2},{"id":78,"html_url":"https://example/check/78","conclusion":"success","run_attempt":1}]}`), nil
			case strings.Contains(path, "pr-sha/pulls"), strings.Contains(path, "pr-sha-2/pulls"):
				return []byte(`[{"number":2,"html_url":"https://example/pr/2","merged_at":"2026-07-28T09:00:00Z","head":{"ref":"external","sha":"external-head"}}]`), nil
			case strings.Contains(path, "masquerade-sha/pulls"):
				return []byte(`[{"number":3,"html_url":"https://example/pr/3","merged_at":"2026-07-28T09:00:00Z","head":{"ref":"conveyor/task-known","sha":"unrecorded-head"}}]`), nil
			case strings.Contains(path, "/pulls"):
				return []byte(`[]`), nil
			default:
				return nil, fmt.Errorf("unexpected args %v", args)
			}
		},
		OnSuppressed: func(context.Context, map[string]any) error { suppressed++; return nil },
	}
	observations, err := source.Observations(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 5 {
		t.Fatalf("observations=%+v", observations)
	}
	if suppressed != 1 {
		t.Fatalf("suppressed=%d", suppressed)
	}
	got := map[string]SignalKind{}
	for _, observation := range observations {
		got[observation.OccurrenceID] = observation.Kind
	}
	want := map[string]SignalKind{
		"commit:known-sha:attempt:2": PostMergeFailure,
		"push-sha":                   DirectPush,
		"pr:2":                       ExternalPRMerge,
		"pr:3":                       ExternalPRMerge,
		"revert-sha":                 Revert,
	}
	for occurrence, kind := range want {
		if got[occurrence] != kind {
			t.Fatalf("occurrence %s kind=%s want=%s; all=%+v", occurrence, got[occurrence], kind, observations)
		}
	}
	for _, observation := range observations {
		if observation.Kind == PostMergeFailure {
			if observation.CommitSHA != "known-sha" || !strings.Contains(observation.Context["failed_check_runs"], "unit (check run 77)") {
				t.Fatalf("failed check observation=%+v", observation)
			}
		}
	}
}

func TestGitHubSourceSuppressesNonActionableCheckConclusions(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "skipped", "neutral", "stale", "action_required", "success"} {
		t.Run(conclusion, func(t *testing.T) {
			source := lineagedCheckSource(fmt.Sprintf(`{"check_runs":[{"id":77,"name":"ci","html_url":"https://example/check/77","conclusion":%q,"run_attempt":1}]}`, conclusion))
			observations, err := source.Observations(context.Background(), time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if conclusion == "success" {
				if len(observations) != 1 || !observations[0].RecoveryObserved {
					t.Fatalf("success did not produce recovery observation: %+v", observations)
				}
				return
			}
			if len(observations) != 0 {
				t.Fatalf("conclusion %q produced observations %+v", conclusion, observations)
			}
		})
	}
}

func TestGitHubSourceAggregatesFailedChecksPerCommitAttempt(t *testing.T) {
	source := lineagedCheckSource(`{"check_runs":[
{"id":22,"name":"integration","html_url":"https://example/check/22","conclusion":"timed_out","run_attempt":0},
{"id":11,"name":"unit","html_url":"https://example/check/11","conclusion":"failure","run_attempt":1},
{"id":33,"name":"retry","html_url":"https://example/check/33","conclusion":"failure","run_attempt":2}
]}`)

	first, err := source.Observations(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Observations(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if first[0].OccurrenceID != "commit:known-sha:attempt:1" || second[0].OccurrenceID != first[0].OccurrenceID ||
		first[1].OccurrenceID != "commit:known-sha:attempt:2" {
		t.Fatalf("occurrences first=%+v second=%+v", first, second)
	}
	if first[0].CheckRunID != "11,22" || first[0].SourceURL != "https://example/check/11" {
		t.Fatalf("aggregate=%+v", first[0])
	}
	if first[0].IntakeKey() == first[1].IntakeKey() || first[0].Identity() == first[1].Identity() {
		t.Fatalf("attempt identity/grouping first=%+v", first)
	}
	detail := first[0].Context["failed_check_runs"]
	if !strings.Contains(detail, "unit (check run 11)") || !strings.Contains(detail, "integration (check run 22)") {
		t.Fatalf("failed check detail=%q", detail)
	}
}

func TestGitHubSourceEmitsRecoveryForGreenLineagedMerge(t *testing.T) {
	source := lineagedCheckSource(`{"check_runs":[{"id":77,"name":"ci","html_url":"https://example/check/77","conclusion":"success","run_attempt":1}]}`)
	observations, err := source.Observations(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || !observations[0].RecoveryObserved ||
		observations[0].Kind != PostMergeFailure || observations[0].CommitSHA != "known-sha" {
		t.Fatalf("recovery observations=%+v", observations)
	}
}

func TestGitHubSourceSuppressesFirstParentEmptyDirectPush(t *testing.T) {
	source := GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor",
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			path := strings.Join(args, " ")
			switch {
			case strings.Contains(path, "/commits -f"):
				return []byte(`[
{"sha":"empty-merge","html_url":"https://example/empty","parents":[{"sha":"parent-a"},{"sha":"parent-b"}],"commit":{"message":"Merge main","committer":{"date":"2026-07-28T10:00:00Z"}}},
{"sha":"content-push","html_url":"https://example/content","parents":[{"sha":"parent-c"}],"commit":{"message":"manual","committer":{"date":"2026-07-28T10:01:00Z"}}}
]`), nil
			case strings.Contains(path, "/pulls"):
				return []byte(`[]`), nil
			case strings.Contains(path, "compare/parent-a...empty-merge"):
				return []byte(`{"files":[]}`), nil
			case strings.Contains(path, "compare/parent-c...content-push"):
				return []byte(`{"files":[{"filename":"internal/monitor/types.go"}]}`), nil
			default:
				return nil, fmt.Errorf("unexpected args %v", args)
			}
		},
	}
	observations, err := source.Observations(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Kind != DirectPush || observations[0].CommitSHA != "content-push" {
		t.Fatalf("observations=%+v", observations)
	}
}

func lineagedCheckSource(checks string) GitHubSource {
	return GitHubSource{
		WorkspaceID: "demo", Repository: "conveyor", GitHubSlug: "acme/conveyor",
		KnownLineage: func(id string, number int, headSHA string) bool {
			return id == "known" && number == 1 && headSHA == "known-head"
		},
		Run: func(_ context.Context, args ...string) ([]byte, error) {
			path := strings.Join(args, " ")
			switch {
			case strings.Contains(path, "/commits -f"):
				return []byte(`[{"sha":"known-sha","html_url":"https://example/known","commit":{"message":"merge","committer":{"date":"2026-07-28T10:00:00Z"}}}]`), nil
			case strings.Contains(path, "known-sha/pulls"):
				return []byte(`[{"number":1,"html_url":"https://example/pr/1","merged_at":"2026-07-28T09:00:00Z","head":{"ref":"conveyor/task-known","sha":"known-head"}}]`), nil
			case strings.Contains(path, "known-sha/check-runs"):
				return []byte(checks), nil
			default:
				return nil, fmt.Errorf("unexpected args %v", args)
			}
		},
	}
}

func TestFetchGitHubHintsPinsRevisionAndFailsClosed(t *testing.T) {
	document := []byte("version: 1\ntriage_areas: [control-plane]\n")
	var called string
	hints, err := FetchGitHubHints(context.Background(), "acme/conveyor", "deadbeef",
		func(_ context.Context, args ...string) ([]byte, error) {
			called = strings.Join(args, " ")
			return []byte(fmt.Sprintf(`{"encoding":"base64","content":%q}`,
				base64.StdEncoding.EncodeToString(document))), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(called, "ref=deadbeef") || hints == nil ||
		hints.Revision != "deadbeef" || hints.Fingerprint == "" {
		t.Fatalf("called=%q hints=%+v", called, hints)
	}

	missing, err := FetchGitHubHints(context.Background(), "acme/conveyor", "deadbeef",
		func(context.Context, ...string) ([]byte, error) {
			return nil, fmt.Errorf("HTTP 404: not found")
		})
	if err != nil || missing != nil {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}

	if _, err = FetchGitHubHints(context.Background(), "acme/conveyor", "deadbeef",
		func(context.Context, ...string) ([]byte, error) {
			unsafe := base64.StdEncoding.EncodeToString([]byte("version: 1\ntools: [shell]\n"))
			return []byte(fmt.Sprintf(`{"encoding":"base64","content":%q}`, unsafe)), nil
		}); err == nil {
		t.Fatal("unsafe capability grant was accepted")
	}
}

func TestRecordedLineageRejectsUnrelatedRepositoryAndUnrecordedHead(t *testing.T) {
	task := core.Task{
		ID: "known", Repo: "other", Branch: "conveyor/task-known",
		GitHub: &core.GitHubLifecycle{Repository: "acme/other"},
	}
	events := []core.Event{{
		Kind: "pull_request.opened",
		Payload: core.JSONPayload(map[string]any{
			"number": 3, "head_sha": "recorded-head",
		}),
	}}
	if RecordedLineage(task, events, "conveyor", "acme/conveyor", "known", 3, "recorded-head") {
		t.Fatal("unrelated repository task was accepted as Conveyor lineage")
	}
	task.Repo, task.GitHub.Repository = "conveyor", "acme/conveyor"
	if RecordedLineage(task, events, "conveyor", "acme/conveyor", "known", 3, "unrecorded-head") {
		t.Fatal("unrecorded pull-request head was accepted as Conveyor lineage")
	}
	if !RecordedLineage(task, events, "conveyor", "acme/conveyor", "known", 3, "recorded-head") {
		t.Fatal("recorded repository, pull request, and head were not accepted")
	}
}
