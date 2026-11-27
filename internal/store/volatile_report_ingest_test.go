package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// CloseFeedbackReportForTest writes one volatile report as closed at the
// given time. Closing belongs to a sibling task, so the memory conformance
// entry point supplies this as Fixture.CloseFeedbackReport.
func CloseFeedbackReportForTest(t *testing.T, st Backend, ctx context.Context, reportID string, at time.Time) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	for scope, reports := range m.feedbackReportState.reports {
		if scope.workspace != workspace {
			continue
		}
		for key, report := range reports {
			if report.ID != reportID {
				continue
			}
			closed := EnvironmentTime(at)
			report.State, report.InInbox = FeedbackReportClosed, false
			report.ClosedAt, report.ClosedBy, report.CloseReason = &closed, "usr-closer", "fixed"
			reports[key] = report
			return
		}
	}
	t.Fatalf("feedback report %q is missing", reportID)
}

// SeedFeedbackReportLinkForTest writes one volatile link row as given.
// Confirming and removing belong to a sibling task, so the memory
// conformance entry point supplies this as Fixture.SeedFeedbackReportLink.
func SeedFeedbackReportLinkForTest(t *testing.T, st Backend, ctx context.Context, link FeedbackReportLink) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	if _, _, _, found := m.feedbackReportLocked(workspace, link.ReportID); !found {
		t.Fatalf("feedback report %q is missing", link.ReportID)
	}
	link.CreatedAt = EnvironmentTime(link.CreatedAt)
	if link.ConfirmedAt != nil {
		at := EnvironmentTime(*link.ConfirmedAt)
		link.ConfirmedAt = &at
	}
	if link.RemovedAt != nil {
		at := EnvironmentTime(*link.RemovedAt)
		link.RemovedAt = &at
	}
	m.feedbackReportState.links[memoryScopedKey{workspace: workspace, id: link.ID}] = feedbackReportLinkRowOf(link)
}

// TestVolatileFeedbackReportStorage proves that the memory backend allocates
// the four report tables of the durable backends, and that a report request
// writes reports only: the link, note, and line-read stores stay empty until
// the linking work writes them (component-feedback-reports, Storage;
// feature-production-feedback-listening FR-1 and FR-2).
func TestVolatileFeedbackReportStorage(t *testing.T) {
	st := NewVolatileBackend()
	m := st.(*volatileMemory)
	state := m.feedbackReportState
	if state.reports == nil || state.links == nil || state.notes == nil || state.lineReads == nil {
		t.Fatalf("report state=%+v, want four allocated stores", state)
	}
	ctx := WithActor(WithWorkspace(t.Context(), "reports"), SystemActor())
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "reports"}); err != nil {
		t.Fatal(err)
	}
	environment, err := st.CreateEnvironment(ctx, EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, environment.ID, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	batch := FeedbackReportBatch{EnvironmentID: environment.ID, KeyID: key.ID, ArrivedAt: time.Now(), Entries: []FeedbackReportEntry{
		{Kind: FeedbackReportError, Key: "crash", Summary: "Crash", File: "main.go", Line: 3, Commit: "abcdef0"},
	}}
	if err := st.RecordFeedbackReports(ctx, batch); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	// The environment's scope holds exactly the one report, and no other
	// scope holds any.
	scope := memoryScopedKey{workspace: "reports", id: environment.ID}
	reports := m.feedbackReportState.reports[scope]
	if report, ok := reports["crash"]; len(m.feedbackReportState.reports) != 1 || len(reports) != 1 || !ok || report.Key != "crash" || report.EnvironmentID != environment.ID {
		t.Fatalf("after one report: scopes=%d reports of the environment=%+v, want exactly the crash report", len(m.feedbackReportState.reports), reports)
	}
	if len(m.feedbackReportState.links) != 0 || len(m.feedbackReportState.notes) != 0 || len(m.feedbackReportState.lineReads) != 0 {
		t.Fatalf("after one report: links=%d notes=%d line reads=%d, want 0, 0, 0",
			len(m.feedbackReportState.links), len(m.feedbackReportState.notes), len(m.feedbackReportState.lineReads))
	}
}

// TestStoredEvidenceReadChecks covers the read-side checks every backend
// applies to stored evidence after decoding it once: a malformed part of
// stored line progress, a malformed stored line answer, and a malformed
// stored timing result prove nothing. None of these shapes can be written
// through a backend, whose writes refuse them (conformance
// MalformedEvidenceRefused); the checks keep a damaged row from ever
// matching (component-feedback-reports invariants 12 to 15).
func TestStoredEvidenceReadChecks(t *testing.T) {
	commit := strings.Repeat("c", 40)
	input := strings.Repeat("a", 64)
	t.Run("line progress", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			in   FeedbackLineProgress
			want FeedbackLineProgress
		}{
			{"a whole resolution", FeedbackLineProgress{Repository: "app", Slug: "acme/app", Commit: commit}, FeedbackLineProgress{Repository: "app", Slug: "acme/app", Commit: commit}},
			{"a resolution with a quoted slug", FeedbackLineProgress{Repository: "app", Slug: `"acme/app"`, Commit: commit}, FeedbackLineProgress{}},
			{"a resolution with an upper-case slug", FeedbackLineProgress{Repository: "app", Slug: "Acme/App", Commit: commit}, FeedbackLineProgress{}},
			{"a resolution with a short commit", FeedbackLineProgress{Repository: "app", Slug: "acme/app", Commit: "abc"}, FeedbackLineProgress{}},
			{"a resolution without its name", FeedbackLineProgress{Slug: "acme/app", Commit: commit}, FeedbackLineProgress{}},
			{"a resolution and a reason together", FeedbackLineProgress{Repository: "app", Slug: "acme/app", Commit: commit, Unresolved: FeedbackReasonNoMatch}, FeedbackLineProgress{}},
			{"a definitive reason", FeedbackLineProgress{Unresolved: FeedbackReasonNoMatch}, FeedbackLineProgress{Unresolved: FeedbackReasonNoMatch}},
			{"a retryable reason", FeedbackLineProgress{Unresolved: FeedbackReasonPending}, FeedbackLineProgress{}},
			{"answers without a digest input", FeedbackLineProgress{Input: "tags/v1", Candidates: map[string]string{"acme/app": commit}}, FeedbackLineProgress{}},
			{"well-formed and malformed answers", FeedbackLineProgress{Input: input, Candidates: map[string]string{
				"acme/app": commit, "acme/x": FeedbackLineAbsent, `"acme/q"`: commit, "acme/y/z": commit, "acme/w": "maybe", "Acme/U": commit,
			}}, FeedbackLineProgress{Input: input, Candidates: map[string]string{"acme/app": commit, "acme/x": FeedbackLineAbsent}}},
			{"an ended progress keeps no answers", FeedbackLineProgress{Unresolved: FeedbackReasonAmbiguous, Input: input, Candidates: map[string]string{"acme/app": commit}}, FeedbackLineProgress{Unresolved: FeedbackReasonAmbiguous}},
		} {
			if got := CheckStoredFeedbackLineProgress(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s: got %+v, want %+v", tc.name, got, tc.want)
			}
			// The durable backends decode the column once and apply the same
			// check.
			parsed, err := ParseFeedbackLineProgress([]byte(FeedbackLineProgressJSON(tc.in)))
			if err != nil || !reflect.DeepEqual(*parsed, tc.want) {
				t.Fatalf("%s: parsed %+v err %v, want %+v", tc.name, parsed, err, tc.want)
			}
		}
	})
	t.Run("line answer", func(t *testing.T) {
		base := FeedbackReportLineRead{ReportID: "rpt-1", Repository: "app", ResolvedCommit: commit, File: "a.go", Line: 1, AnsweredAt: time.Now()}
		for name, answer := range map[string]FeedbackReportLineAnswer{
			"a found file without its commit":   {FileFound: true},
			"a missing file with a commit":      {Commit: commit},
			"a missing file with pull requests": {PullRequests: []int{3}},
			"a pull request number 0":           {FileFound: true, Commit: commit, PullRequests: []int{0}},
		} {
			read := base
			read.Answer = answer
			if _, ok := CheckStoredFeedbackReportLineRead(read); ok {
				t.Fatalf("%s: a malformed stored answer was read as stored", name)
			}
		}
		read := base
		read.Answer = FeedbackReportLineAnswer{FileFound: true, Commit: commit, PullRequests: []int{9, 3, 9}}
		if got, ok := CheckStoredFeedbackReportLineRead(read); !ok || !reflect.DeepEqual(got.Answer.PullRequests, []int{3, 9}) {
			t.Fatalf("a well-formed answer: %+v %v", got, ok)
		}
	})
	t.Run("timing result", func(t *testing.T) {
		for name, raw := range map[string]string{
			"a link without its task":        `{"links":[{"task_id":"","deploy_id":"d","elapsed_minutes":0}],"notes":[]}`,
			"too_many_tasks without a count": `{"links":[],"notes":[{"kind":"too_many_tasks","deploy_id":"d"}]}`,
			"a note kind it never writes":    `{"links":[],"notes":[{"kind":"file_not_found","deploy_id":"d"}]}`,
		} {
			if got, err := ParseFeedbackTimingResult([]byte(raw)); err != nil || got != nil {
				t.Fatalf("%s: got %+v err %v, want no stored result", name, got, err)
			}
		}
	})
}

// TestMergeEventDecoders covers the decode-once merge-event decoders and the
// slug check, which decodes nothing (component-feedback-reports invariants
// 12 and 13). The conformance case MergeEventShapes runs the same shapes
// through every backend.
func TestMergeEventDecoders(t *testing.T) {
	for raw, want := range map[string]string{`"acme/app"`: "acme/app", `"Acme/App"`: "acme/app", `"\"acme/app\""`: "", `" acme/app"`: "", `"acme/app/x"`: "", `"acme"`: "", `""`: "", `null`: "", ``: "", `{"slug":"acme/app"}`: "", `["acme/app"]`: "", `acme/app`: ""} {
		if got := FeedbackMergeRepositoryJSON(raw); got != want {
			t.Fatalf("repository %s: %q, want %q", raw, got, want)
		}
	}
	for text, want := range map[string]string{"acme/app": "acme/app", `"acme/app"`: "", " acme/app": "", "acme//app": "", "acme/app/": "", "ac me/app": "", "acme/a\"pp": ""} {
		if got := FeedbackRepositorySlug(text); got != want {
			t.Fatalf("slug %q: %q, want %q", text, got, want)
		}
	}
	commit := strings.Repeat("ab", 20)
	for raw, want := range map[string]string{`"` + commit + `"`: commit, `"` + strings.ToUpper(commit) + `"`: commit, `"\"` + commit + `\""`: "", `" ` + commit + `"`: "", strings.Repeat("1", 40): "", `null`: ""} {
		if got := FeedbackMergeCommitJSON(raw); got != want {
			t.Fatalf("commit %s: %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]int{`42`: 42, `"42"`: 42, `42.0`: 42, `4.2e1`: 42, `"\"42\""`: 0, `" 42"`: 0, `"042"`: 0, `-3`: 0, `0`: 0, `42.5`: 0, `1e20`: 0, `true`: 0, `null`: 0, ``: 0, `42 43`: 0} {
		if got := FeedbackMergePullRequestJSON(raw); got != want {
			t.Fatalf("pull request %s: %d, want %d", raw, got, want)
		}
	}
}
