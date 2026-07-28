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
				return []byte(`{"check_runs":[{"id":77,"html_url":"https://example/check/77","conclusion":"failure","run_attempt":2},{"id":78,"html_url":"https://example/check/78","conclusion":"success","run_attempt":1}]}`), nil
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
		"check:77:attempt:2": PostMergeFailure,
		"push-sha":           DirectPush,
		"pr:2":               ExternalPRMerge,
		"pr:3":               ExternalPRMerge,
		"revert-sha":         Revert,
	}
	for occurrence, kind := range want {
		if got[occurrence] != kind {
			t.Fatalf("occurrence %s kind=%s want=%s; all=%+v", occurrence, got[occurrence], kind, observations)
		}
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
