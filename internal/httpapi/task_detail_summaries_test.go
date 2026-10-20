package httpapi

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Task 261015-3721c6; component-http-api "Bounded detail view";
// req-task-centric-operations-view AC-1.2, AC-2.2.

func TestHumanTimelineEventKindsMatchDashboard(t *testing.T) {
	source, err := os.ReadFile("../../web/src/lib/activity.ts")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	literal := func(name, pattern string) []string {
		t.Helper()
		match := regexp.MustCompile(pattern).FindStringSubmatch(text)
		if match == nil {
			t.Fatalf("web/src/lib/activity.ts has no %s literal matching %s", name, pattern)
		}
		var kinds []string
		for _, quoted := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(match[1], -1) {
			kinds = append(kinds, quoted[1])
		}
		if len(kinds) == 0 {
			t.Fatalf("%s literal is empty", name)
		}
		return kinds
	}
	noteKinds := literal("noteEventKinds", `export const noteEventKinds = \[([^\]]*)\]\s*as const`)
	attemptKinds := literal("attemptEventKinds", `const attemptEventKinds = new Set\(\[([^\]]*)\]\)`)
	hiddenKinds := literal("hiddenNoteKinds", `const hiddenNoteKinds = new Set\(\[([^\]]*)\]\)`)
	retryKinds := literal("automaticRetryEventKinds", `const automaticRetryEventKinds = new Set\(\[([^\]]*)\]\)`)
	if !regexp.MustCompile(`export const humanTimelineEventKinds = noteEventKinds\.filter\(\s*\(kind\) => \(!attemptEventKinds\.has\(kind\) \|\| automaticRetryEventKinds\.has\(kind\)\) && !hiddenNoteKinds\.has\(kind\),?\s*\)`).MatchString(text) {
		t.Fatal("humanTimelineEventKinds is not computed as noteEventKinds minus non-retry attemptEventKinds and hiddenNoteKinds")
	}
	// noteEventKinds must be exactly the case labels the note switch renders,
	// from noteFor through its helpers up to the next timeline section.
	start := strings.Index(text, "function noteFor(")
	end := strings.Index(text[max(start, 0):], "const genericSummaries")
	if start < 0 || end < 0 {
		t.Fatal("noteFor not found")
	}
	var labels []string
	for _, match := range regexp.MustCompile(`case '([^']+)':`).FindAllStringSubmatch(text[start:start+end], -1) {
		labels = append(labels, match[1])
	}
	if len(labels) == 0 {
		t.Fatal("no note case labels found after noteFor")
	}
	sorted := func(values []string) []string {
		values = slices.Clone(values)
		sort.Strings(values)
		return slices.Compact(values)
	}
	if !slices.Equal(sorted(noteKinds), sorted(labels)) {
		t.Fatalf("noteEventKinds=%v differs from noteFor case labels=%v", sorted(noteKinds), sorted(labels))
	}
	var want []string
	for _, kind := range noteKinds {
		if (!slices.Contains(attemptKinds, kind) || slices.Contains(retryKinds, kind)) && !slices.Contains(hiddenKinds, kind) {
			want = append(want, kind)
		}
	}
	if len(humanTimelineEventKinds) != 40 || len(sorted(humanTimelineEventKinds)) != len(humanTimelineEventKinds) {
		t.Fatalf("Go human kinds must be 40 distinct kinds: %v", humanTimelineEventKinds)
	}
	if !slices.Equal(sorted(want), sorted(humanTimelineEventKinds)) {
		t.Fatalf("dashboard human kinds=%v Go=%v", sorted(want), sorted(humanTimelineEventKinds))
	}
	// A round summary is read from summaries.review_round_summaries and renders
	// inside its review panel, never as a standalone note
	// (feature-review-round-summary SU-6).
	if slices.Contains(noteKinds, store.ReviewSummaryRecordedEvent) || slices.Contains(humanTimelineEventKinds, store.ReviewSummaryRecordedEvent) {
		t.Fatalf("%s must stay out of noteEventKinds and the human timeline kinds", store.ReviewSummaryRecordedEvent)
	}
}

var summaryEpoch = time.Date(2026, 10, 15, 7, 0, 0, 0, time.UTC)

// summaryEvent builds an event at a second offset; the slice order a test
// passes is commit order, which deriveTaskDetailSummaries must not rely on.
func summaryEvent(id int64, seconds int, kind, jobID string, payload string) core.Event {
	return core.Event{ID: id, TaskID: "summary-task", JobID: jobID, Kind: kind, ActorID: "system:dispatcher", ActorRole: core.ActorSystem,
		At: summaryEpoch.Add(time.Duration(seconds) * time.Second), Payload: json.RawMessage(payload)}
}

func summaryJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func at(seconds int) string {
	return formatTaskEventTime(summaryEpoch.Add(time.Duration(seconds) * time.Second))
}

// TestTaskDetailSummariesMatchDashboardRules holds one equivalence fixture per
// selection rule of web/src/lib/activity.ts. Expected values are what the
// dashboard computes from the complete (at, id)-ordered ledger, including
// absent, empty, and non-string values and a later event lacking a value an
// earlier one has.
func TestTaskDetailSummariesMatchDashboardRules(t *testing.T) {
	derive := func(events ...core.Event) taskDetailSummaries {
		return deriveTaskDetailSummaries(events, nil, nil)
	}
	t.Run("creator is the first task.created by (at, id)", func(t *testing.T) {
		if got := derive(summaryEvent(1, 0, "task.assignee.set", "", `{}`)).Creator; got != nil {
			t.Fatalf("creator without task.created = %+v", got)
		}
		first := summaryEvent(9, 0, "task.created", "", `{}`)
		first.ActorID, first.ActorRole = "user:usr_first", core.ActorHuman
		// Committed later with a lower ID at the same instant: it sorts first.
		lower := summaryEvent(3, 0, "task.created", "", `{}`)
		lower.ActorID, lower.ActorRole = "agent:lower", core.ActorAgent
		got := derive(first, lower).Creator
		if got == nil || got.ActorID != "agent:lower" || got.ActorRole != core.ActorAgent {
			t.Fatalf("creator=%+v", got)
		}
	})
	t.Run("pull request is the latest pull_request.opened", func(t *testing.T) {
		got := derive(
			summaryEvent(1, 0, "pull_request.opened", "", `{"url":"https://example.test/pr/1","number":1,"repository":"acme/app","head_sha":"aaa","base_sha":"bbb"}`),
			summaryEvent(2, 5, "pull_request.opened", "", `{"head_sha":"ccc","base_sha":7,"number":2}`),
		).PullRequest
		// mergeGateReview reads only the latest event: no URL there means no
		// pull request link, number, or repository, and a non-string base.
		if summaryJSON(t, got) != `{"head_sha":"ccc","event_id":"2","at":"`+at(5)+`"}` {
			t.Fatalf("pull request=%s", summaryJSON(t, got))
		}
		got = derive(
			summaryEvent(5, 1, "pull_request.opened", "", `{"url":"https://example.test/pr/9","number":"9","repository":["x"]}`),
			summaryEvent(4, 1, "pull_request.opened", "", `{"url":"https://example.test/pr/8","number":8.0}`),
		).PullRequest
		if summaryJSON(t, got) != `{"url":"https://example.test/pr/9","event_id":"5","at":"`+at(1)+`"}` {
			t.Fatalf("tied pull request=%s", summaryJSON(t, got))
		}
		got = derive(summaryEvent(6, 1, "pull_request.opened", "", `{"url":"","number":3,"repository":"acme/app"}`)).PullRequest
		if summaryJSON(t, got) != `{"event_id":"6","at":"`+at(1)+`"}` {
			t.Fatalf("empty url pull request=%s", summaryJSON(t, got))
		}
		got = derive(summaryEvent(7, 1, "pull_request.opened", "", `{"url":"u","number":12}`)).PullRequest
		if summaryJSON(t, got) != `{"url":"u","number":12,"event_id":"7","at":"`+at(1)+`"}` {
			t.Fatalf("numbered pull request=%s", summaryJSON(t, got))
		}
	})
	t.Run("review round is the latest review.round_completed", func(t *testing.T) {
		for _, test := range []struct{ payload, want string }{
			{`{"verdict":"changes_requested","summary":"  fix it ","reviews":[{},{}]}`, `{"verdict":"changes_requested","summary":"  fix it ","review_count":2`},
			{`{"verdict":"reject","summary":7,"reviews":{}}`, `{"verdict":"approve","review_count":0`},
			{`{"summary":""}`, `{"verdict":"approve","summary":"","review_count":0`},
			{`null`, `{"verdict":"approve","review_count":0`},
		} {
			got := derive(
				summaryEvent(1, 0, "review.round_completed", "", `{"verdict":"changes_requested","summary":"earlier","reviews":[{}]}`),
				summaryEvent(2, 1, "review.round_completed", "", test.payload),
			).ReviewRound
			if want := test.want + `,"event_id":"2","at":"` + at(1) + `"}`; summaryJSON(t, got) != want {
				t.Fatalf("payload %s: review round=%s want %s", test.payload, summaryJSON(t, got), want)
			}
		}
	})
	t.Run("returned for changes follows pendingUserRequestChanges", func(t *testing.T) {
		bounce := func(id int64, seconds int, payload string) core.Event {
			return summaryEvent(id, seconds, "pipeline.bounced", "", payload)
		}
		claim := func(id int64, seconds int, stage string) core.Event {
			return summaryEvent(id, seconds, "work_order.claimed", "", `{"stage":"`+stage+`","claimant_id":"worker:w"}`)
		}
		if got := derive(bounce(1, 0, `{"source":"review","feedback":"x"}`)).ReturnedForChanges; got != nil {
			t.Fatalf("non-user bounce pending=%+v", got)
		}
		got := derive(bounce(1, 0, `{"source":"user-request-changes","feedback":"  tighten tests \n"}`), claim(2, 1, "review")).ReturnedForChanges
		if summaryJSON(t, got) != `{"feedback":"tighten tests","at":"`+at(0)+`","event_id":"1"}` {
			t.Fatalf("pending=%s", summaryJSON(t, got))
		}
		if got := derive(bounce(1, 0, `{"source":"user-request-changes","feedback":"x"}`), claim(2, 1, "implement")).ReturnedForChanges; got != nil {
			t.Fatalf("implement claim did not clear: %+v", got)
		}
		// The clearing claim has a higher ID but an earlier time: it does not
		// clear the later bounce.
		got = derive(claim(9, 0, "implement"), bounce(3, 1, `{"source":"user-request-changes","feedback":["x"]}`)).ReturnedForChanges
		if summaryJSON(t, got) != `{"feedback":"","at":"`+at(1)+`","event_id":"3"}` {
			t.Fatalf("later bounce after earlier claim=%s", summaryJSON(t, got))
		}
	})
	t.Run("run implementation is the latest implement claimant", func(t *testing.T) {
		claim := func(id int64, seconds int, payload string) core.Event {
			return summaryEvent(id, seconds, "work_order.claimed", "", payload)
		}
		for _, test := range []struct {
			events []core.Event
			want   bool
		}{
			{nil, false},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":"run:usr_1/s"}`)}, true},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":"run:"}`)}, false},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":"run:\nx"}`)}, false},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":"run:x\n"}`)}, true},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":7}`)}, false},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":"run:a"}`), claim(2, 1, `{"stage":"implement","claimant_id":"worker:w"}`)}, false},
			{[]core.Event{claim(1, 0, `{"stage":"implement","claimant_id":"run:a"}`), claim(2, 1, `{"stage":"review","claimant_id":"worker:w"}`)}, true},
			// Lower ID committed later at a later time is the latest claim.
			{[]core.Event{claim(5, 0, `{"stage":"implement","claimant_id":"worker:w"}`), claim(2, 1, `{"stage":"implement","claimant_id":"run:b"}`)}, true},
		} {
			if got := derive(test.events...).RunImplementation; got != test.want {
				t.Fatalf("events=%v run=%v want %v", test.events, got, test.want)
			}
		}
	})
	t.Run("plan revision request follows latestPlanRevisionRequest", func(t *testing.T) {
		request := func(id int64, seconds int, payload string) core.Event {
			return summaryEvent(id, seconds, "work_order.plan_revision_requested", "", payload)
		}
		for _, test := range []struct{ payload, want string }{
			{`{"rationale":"  split the plan  ","plan_version":3}`, `{"rationale":"split the plan","plan_version":3`},
			{`{"rationale":"r","plan_version":"4 apples"}`, `{"rationale":"r","plan_version":4`},
			{`{"rationale":"r","plan_version":" +5"}`, `{"rationale":"r","plan_version":5`},
			{`{"rationale":"r","plan_version":[6]}`, `{"rationale":"r","plan_version":6`},
			{`{"rationale":"r","plan_version":2.0}`, `{"rationale":"r","plan_version":2`},
			{`{"rationale":"r","plan_version":2.5}`, ``},
			{`{"rationale":"r","plan_version":"0"}`, ``},
			{`{"rationale":"r","plan_version":"-1"}`, ``},
			{`{"rationale":"r","plan_version":true}`, ``},
			{`{"rationale":"r"}`, ``},
			{`{"rationale":"\u00a0\ufeff\u2028 ","plan_version":1}`, ``},
			{`{"rationale":7,"plan_version":1}`, ``},
		} {
			// An earlier valid request never stands in for an invalid latest one.
			got := derive(request(1, 0, `{"rationale":"earlier","plan_version":1}`), request(2, 1, test.payload)).PlanRevisionRequest
			want := "null"
			if test.want != "" {
				want = test.want + `,"event_id":"2","at":"` + at(1) + `"}`
			}
			if summaryJSON(t, got) != want {
				t.Fatalf("payload %s: request=%s want %s", test.payload, summaryJSON(t, got), want)
			}
		}
	})
	t.Run("latest review completed event", func(t *testing.T) {
		got := derive(summaryEvent(10, 1, "review.completed", "", `{}`), summaryEvent(4, 2, "review.completed", "", `{}`), summaryEvent(11, 0, "review.completed", "", `{}`))
		if got.LatestReviewCompletedEventID != "4" {
			t.Fatalf("latest review.completed=%q", got.LatestReviewCompletedEventID)
		}
	})
	t.Run("job results follow preferredJobSummary, rejectedOutputEvent and jobSummary", func(t *testing.T) {
		got := derive(
			summaryEvent(1, 0, "job.summary", "job-a", `{"summary":"narrated"}`),
			// A later job.summary without a string summary does not replace it.
			summaryEvent(2, 1, "job.summary", "job-a", `{"summary":null}`),
			summaryEvent(3, 2, "job.summary", "job-b", `{"summary":""}`),
			summaryEvent(4, 3, "review.completed", "job-c", `{"summary":"looks good","feedback":"  add a test  "}`),
			summaryEvent(5, 4, "review.completed", "job-c", `{"summary":""}`),
			summaryEvent(6, 5, "triage.completed", "job-d", `{"summary":"triaged","feedback":"ignored"}`),
			summaryEvent(7, 6, "review.output_invalid", "job-e", `{"error":"  bad json  "}`),
			summaryEvent(8, 7, "spec.output_invalid", "job-f", `{}`),
			summaryEvent(9, 8, "job.failed", "job-g", `{"error":"boom"}`),
			summaryEvent(10, 9, "job.failed", "job-g", `{"error":""}`),
			summaryEvent(11, 10, "job.failed", "job-h", `{"error":42}`),
			summaryEvent(12, 11, "job.summary", "", `{"summary":"taskwide"}`),
		).JobResults
		want := `{"job-a":{"summary":{"text":"narrated","event_id":"1"}},` +
			`"job-b":{"summary":{"text":"","event_id":"3"}},` +
			`"job-c":{"stage_summary":{"text":"looks good","review_feedback":"add a test","event_id":"4"}},` +
			`"job-d":{"stage_summary":{"text":"triaged","event_id":"6"}},` +
			`"job-e":{"output_rejection":{"event_id":"7","error":"bad json"}},` +
			`"job-f":{"output_rejection":{"event_id":"8","error":""}},` +
			`"job-g":{"failure_error":{"text":"boom","event_id":"9"}}}`
		if summaryJSON(t, got) != want {
			t.Fatalf("job results=%s\nwant %s", summaryJSON(t, got), want)
		}
	})
	t.Run("review results and rounds follow buildReviewPanels", func(t *testing.T) {
		got := derive(
			summaryEvent(1, 0, "review.completed", "job-1", `{"verdict":"changes_requested","summary":"s1","feedback":" f1 ","review_work_order_id":"order-1"}`),
			summaryEvent(2, 1, "review.completed", "job-2", `{"verdict":"approve","summary":7}`),
			summaryEvent(3, 2, "review.completed", "", `{"review_work_order_id":"order-3","verdict":"maybe"}`),
			// Lower ID committed later at a later time replaces order-1.
			summaryEvent(0, 3, "review.completed", "job-9", `{"review_work_order_id":"order-1","summary":"late"}`),
			summaryEvent(4, 4, "review.round_completed", "", `{"review_round":2,"verdict":"changes_requested","summary":"","reviews":[{"review_work_order_id":"order-1"},{"review_work_order_id":5},"order-x",null]}`),
			summaryEvent(5, 5, "review.round_completed", "", `{"review_round":0,"summary":3,"reviews":[{"review_work_order_id":"order-0"}]}`),
			summaryEvent(6, 6, "review.round_completed", "", `{"review_round":"3","reviews":[]}`),
			summaryEvent(7, 7, "pipeline.bounced", "", `{"review_round":2,"count":1}`),
			summaryEvent(8, 8, "pipeline.bounced", "", `{"review_round":2,"count":2}`),
			summaryEvent(9, 9, "pipeline.bounced", "", `{"review_round":2}`),
			summaryEvent(10, 10, "pipeline.bounced", "", `{"review_round":"4","count":1}`),
			summaryEvent(11, 11, "pipeline.bounced", "", `{"review_round":2.5,"count":1e0}`),
		)
		results := `{"by_order":{` +
			`"order-1":{"verdict":"approve","summary":"late","feedback":"","at":"` + at(3) + `","event_id":"0"},` +
			`"order-3":{"verdict":"approve","summary":"","feedback":"","at":"` + at(2) + `","event_id":"3"}},` +
			`"by_job":{` +
			`"job-1":{"verdict":"changes_requested","summary":"s1","feedback":" f1 ","at":"` + at(0) + `","event_id":"1"},` +
			`"job-2":{"verdict":"approve","summary":"","feedback":"","at":"` + at(1) + `","event_id":"2"},` +
			`"job-9":{"verdict":"approve","summary":"late","feedback":"","at":"` + at(3) + `","event_id":"0"}}}`
		if summaryJSON(t, got.ReviewResults) != results {
			t.Fatalf("review results=%s\nwant %s", summaryJSON(t, got.ReviewResults), results)
		}
		rounds := `{"by_round":{"2":{"event_id":"4","verdict":"changes_requested","summary":"","at":"` + at(4) + `"}},` +
			`"by_order":{"order-0":{"event_id":"5","verdict":"approve","at":"` + at(5) + `"},` +
			`"order-1":{"event_id":"4","verdict":"changes_requested","summary":"","at":"` + at(4) + `"}}}`
		if summaryJSON(t, got.ReviewRounds) != rounds {
			t.Fatalf("review rounds=%s\nwant %s", summaryJSON(t, got.ReviewRounds), rounds)
		}
		if summaryJSON(t, got.ReviewBounces) != `{"2":2,"2.5":1}` {
			t.Fatalf("review bounces=%s", summaryJSON(t, got.ReviewBounces))
		}
	})
	t.Run("triage failure follows failedTriage with (at, id) supersession", func(t *testing.T) {
		started := summaryEpoch.Add(-time.Hour)
		triage := core.Job{ID: "job-triage", Stage: core.StageTriage, State: core.JobFailed, StartedAt: started, EndedAt: summaryEpoch}
		failure := summaryEvent(50, 0, "job.failed", "job-triage", `{"error":"  budget exceeded ","diagnostic":{"phase":"attachment_validation","provider_code":7}}`)
		got := deriveTaskDetailSummaries([]core.Event{failure}, []core.Job{triage}, nil).TriageFailure
		if summaryJSON(t, got) != `{"job_id":"job-triage","event_id":"50","error":"budget exceeded","diagnostic":{"phase":"attachment_validation"},"superseded":false,"at":"`+at(0)+`"}` {
			t.Fatalf("triage failure=%s", summaryJSON(t, got))
		}
		// A lower-ID dispatch committed later at a later time supersedes; a
		// higher-ID dispatch at an earlier time does not.
		earlier := summaryEvent(99, -1, "pipeline.dispatched", "", `{}`)
		if got := deriveTaskDetailSummaries([]core.Event{failure, earlier}, []core.Job{triage}, nil).TriageFailure; got.Superseded {
			t.Fatal("an earlier higher-ID dispatch superseded the failure")
		}
		for _, kind := range []string{"intervention.approve", "pipeline.dispatched", "job.created", "work_order.claimed"} {
			later := summaryEvent(10, 1, kind, "", `{}`)
			if got := deriveTaskDetailSummaries([]core.Event{failure, later}, []core.Job{triage}, nil).TriageFailure; !got.Superseded {
				t.Fatalf("%s after the failure did not supersede", kind)
			}
		}
		tied := summaryEvent(51, 0, "job.created", "", `{}`)
		if got := deriveTaskDetailSummaries([]core.Event{failure, tied}, []core.Job{triage}, nil).TriageFailure; !got.Superseded {
			t.Fatal("a tied-time higher-ID event did not supersede")
		}
		order := core.WorkOrder{ID: "o", CreatedAt: summaryEpoch.Add(time.Second)}
		if got := deriveTaskDetailSummaries([]core.Event{failure}, []core.Job{triage}, []core.WorkOrder{order}).TriageFailure; !got.Superseded {
			t.Fatal("a later work order did not supersede")
		}
		// Without a job.failed event the failed job's end time is the bound.
		bare := deriveTaskDetailSummaries([]core.Event{summaryEvent(1, -2, "job.created", "", `{}`)}, []core.Job{triage}, nil).TriageFailure
		if summaryJSON(t, bare) != `{"job_id":"job-triage","error":"","superseded":false,"at":"`+at(0)+`"}` {
			t.Fatalf("eventless failure=%s", summaryJSON(t, bare))
		}
		// A running triage job with a job.failed event still reports it.
		running := triage
		running.State = core.JobRunning
		if got := deriveTaskDetailSummaries([]core.Event{failure}, []core.Job{running}, nil).TriageFailure; got == nil || got.EventID != "50" {
			t.Fatalf("running triage job failure=%+v", got)
		}
		if got := deriveTaskDetailSummaries(nil, []core.Job{running}, nil).TriageFailure; got != nil {
			t.Fatalf("running triage job without failure=%+v", got)
		}
		// The latest job by started_at decides; a later implement job hides it.
		implement := core.Job{ID: "job-impl", Stage: core.StageImplement, State: core.JobRunning, StartedAt: summaryEpoch.Add(time.Minute)}
		if got := deriveTaskDetailSummaries([]core.Event{failure}, []core.Job{implement, triage}, nil).TriageFailure; got != nil {
			t.Fatalf("triage failure behind a later job=%+v", got)
		}
		// Millisecond ties keep the given job order, as Array.prototype.sort.
		tiedImplement := core.Job{ID: "job-impl", Stage: core.StageImplement, State: core.JobRunning, StartedAt: started.Add(500 * time.Microsecond)}
		if got := deriveTaskDetailSummaries([]core.Event{failure}, []core.Job{tiedImplement, triage}, nil).TriageFailure; got == nil {
			t.Fatal("millisecond-tied jobs did not keep their given order")
		}
	})
	t.Run("text truncation cuts at a rune boundary and names its event", func(t *testing.T) {
		long := strings.Repeat("é", taskSummaryTextMaxBytes) // two bytes per rune
		payload, _ := json.Marshal(map[string]any{"summary": "x" + long})
		got := derive(core.Event{ID: 77, TaskID: "summary-task", JobID: "job-long", Kind: "job.summary", At: summaryEpoch, Payload: payload}).JobResults["job-long"].Summary
		if !got.Truncated || got.EventID != "77" || len(got.Text) > taskSummaryTextMaxBytes || !utf8.ValidString(got.Text) || len(got.Text) != taskSummaryTextMaxBytes-1 {
			t.Fatalf("truncated summary: truncated=%v event=%s bytes=%d valid=%v", got.Truncated, got.EventID, len(got.Text), utf8.ValidString(got.Text))
		}
		short := derive(summaryEvent(1, 0, "job.summary", "job-short", `{"summary":"ok"}`)).JobResults["job-short"].Summary
		if short.Truncated {
			t.Fatal("short summary marked truncated")
		}
	})
}

func TestJavaScriptNumberSemantics(t *testing.T) {
	for _, test := range []struct {
		value float64
		want  string
	}{
		{0, "0"}, {math.Copysign(0, -1), "0"}, {2, "2"}, {2.5, "2.5"}, {-3, "-3"}, {1e21, "1e+21"}, {1.5e21, "1.5e+21"},
		{123456789012345680000, "123456789012345680000"}, {1e-7, "1e-7"}, {0.000001, "0.000001"}, {math.Inf(1), "Infinity"},
	} {
		if got := jsNumberString(test.value); got != test.want {
			t.Fatalf("jsNumberString(%v)=%q want %q", test.value, got, test.want)
		}
	}
	for _, test := range []struct {
		text string
		want float64
	}{{"12abc", 12}, {"  7", 7}, {"+5", 5}, {"-3", -3}, {"0x10", 0}, {"1e3", 1}, {" \n9", 9}} {
		if got := jsParseInt(test.text); got != test.want {
			t.Fatalf("jsParseInt(%q)=%v want %v", test.text, got, test.want)
		}
	}
	if !math.IsNaN(jsParseInt("abc")) || !math.IsNaN(jsParseInt("")) {
		t.Fatal("parseInt without digits must be NaN")
	}
	if got := jsToString([]any{json.Number("1"), nil, []any{"a", true}, map[string]any{}}); got != "1,,a,true,[object Object]" {
		t.Fatalf("jsToString=%q", got)
	}
}

// TestTaskDetailReviewRoundSummaries pins summaries.review_round_summaries
// (req-review-gates-evidence AC-10.5; DEC-79; feature-review-round-summary
// SU-6): each round's current version by (at, id) over the complete ledger,
// its version count, provenance, and UTF-8-safe text truncation.
func TestTaskDetailReviewRoundSummaries(t *testing.T) {
	derive := func(events ...core.Event) taskRoundSummaries {
		return deriveTaskDetailSummaries(events, nil, nil).ReviewRoundSummaries
	}
	summaryRecorded := func(id int64, seconds int, payload store.ReviewSummaryPayload) core.Event {
		event := summaryEvent(id, seconds, store.ReviewSummaryRecordedEvent, "", summaryJSON(t, payload))
		event.ActorID, event.ActorRole = payload.Author.ActorID, payload.Author.ActorRole
		return event
	}
	author := store.ReviewSummaryAuthor{ActorID: "agent:agt_coordinator", ActorRole: core.ActorAgent, WorkOrderID: "task-review-1-seat-1", SessionID: "seat-session", Model: "claude-opus-5-5", ModelEnforcement: store.ReviewSummaryModelEnforcement}
	version := func(round, number int, summary string) store.ReviewSummaryPayload {
		return store.ReviewSummaryPayload{WorkspaceID: "demo", ReviewRound: round, Version: number, RequestID: summary, Summary: summary, Author: author, Findings: []store.ReviewSummaryFinding{
			{Severity: store.ReviewSummarySeverityBlocking, Problem: "Missing test for " + summary, Location: "internal/x.go:12", Fix: "Add the test.", Seats: []int{1, 3}},
			{Severity: store.ReviewSummarySeverityNonBlocking, Problem: "Naming.", Seats: []int{2}},
		}}
	}

	t.Run("absent without summary events", func(t *testing.T) {
		got := derive(summaryEvent(1, 0, "review.round_completed", "", `{"review_round":1,"verdict":"changes_requested"}`))
		if got == nil || len(got) != 0 {
			t.Fatalf("review_round_summaries=%v, want an empty map", got)
		}
		data, err := json.Marshal(deriveTaskDetailSummaries(nil, nil, nil))
		if err != nil || !strings.Contains(string(data), `"review_round_summaries":{}`) {
			t.Fatalf("serialized summaries=%s err=%v", data, err)
		}
	})

	t.Run("multiple rounds keep their current version and count", func(t *testing.T) {
		got := derive(
			summaryRecorded(10, 10, version(1, 1, "r1 v1")),
			summaryRecorded(11, 20, version(1, 2, "r1 v2")),
			summaryRecorded(12, 30, version(2, 1, "r2 v1")),
			summaryRecorded(13, 40, version(1, 3, "r1 v3")),
		)
		if len(got) != 2 {
			t.Fatalf("rounds=%v", got)
		}
		one, two := got["1"], got["2"]
		if one.Summary != "r1 v3" || one.Version != 3 || one.VersionCount != 3 || one.EventID != "13" || one.RecordedAt != at(40) || one.Truncated {
			t.Fatalf("round 1=%+v", one)
		}
		if two.Summary != "r2 v1" || two.Version != 1 || two.VersionCount != 1 || two.EventID != "12" || two.RecordedAt != at(30) {
			t.Fatalf("round 2=%+v", two)
		}
		wantAuthor := taskRoundSummaryAuthor{ActorID: author.ActorID, ActorRole: author.ActorRole, WorkOrderID: author.WorkOrderID, SessionID: author.SessionID, Model: author.Model, ModelEnforcement: "self-reported"}
		if one.Author != wantAuthor {
			t.Fatalf("author=%+v want %+v", one.Author, wantAuthor)
		}
		wantFindings := []taskRoundSummaryFinding{
			{Severity: "blocking", Problem: "Missing test for r1 v3", Location: "internal/x.go:12", Fix: "Add the test.", Seats: []int{1, 3}},
			{Severity: "non-blocking", Problem: "Naming.", Seats: []int{2}},
		}
		if !reflect.DeepEqual(one.Findings, wantFindings) {
			t.Fatalf("findings=%+v", one.Findings)
		}
	})

	t.Run("latest is the maximum (at, id) whatever the slice order", func(t *testing.T) {
		got := derive(
			// Committed later with a lower ID at the same instant: it sorts first.
			summaryRecorded(20, 10, version(1, 2, "higher id")),
			summaryRecorded(5, 10, version(1, 1, "lower id")),
			// A later timestamp wins over a higher ID.
			summaryRecorded(99, 5, version(2, 1, "earlier")),
			summaryRecorded(30, 6, version(2, 2, "later")),
		)
		if got["1"].Summary != "higher id" || got["1"].EventID != "20" || got["1"].VersionCount != 2 {
			t.Fatalf("round 1=%+v", got["1"])
		}
		if got["2"].Summary != "later" || got["2"].EventID != "30" || got["2"].VersionCount != 2 {
			t.Fatalf("round 2=%+v", got["2"])
		}
	})

	t.Run("an unreadable round is skipped", func(t *testing.T) {
		got := derive(
			summaryEvent(1, 0, store.ReviewSummaryRecordedEvent, "", `{"review_round":0,"summary":"zero"}`),
			summaryEvent(2, 1, store.ReviewSummaryRecordedEvent, "", `{"review_round":"1","summary":"string"}`),
			summaryEvent(3, 2, store.ReviewSummaryRecordedEvent, "", `{"summary":"none"}`),
			summaryEvent(4, 3, store.ReviewSummaryRecordedEvent, "", `not json`),
		)
		if len(got) != 0 {
			t.Fatalf("review_round_summaries=%+v", got)
		}
	})

	t.Run("text truncation cuts at a rune boundary and names its event", func(t *testing.T) {
		long := "x" + strings.Repeat("é", taskSummaryTextMaxBytes) // two bytes per rune
		payload := version(1, 1, long)
		payload.Findings[0].Problem, payload.Findings[0].Fix, payload.Findings[1].Location = long, long, long
		payload.Author.Model = long
		event := summaryRecorded(77, 0, payload)
		original := append(json.RawMessage(nil), event.Payload...)
		got := derive(event)["1"]
		texts := []string{got.Summary, got.Findings[0].Problem, got.Findings[0].Fix, got.Findings[1].Location, got.Author.Model}
		for i, text := range texts {
			if len(text) != taskSummaryTextMaxBytes-1 || !utf8.ValidString(text) {
				t.Fatalf("text %d: bytes=%d valid=%v", i, len(text), utf8.ValidString(text))
			}
		}
		if !got.Truncated || got.EventID != "77" || got.Findings[0].Location != "internal/x.go:12" {
			t.Fatalf("entry truncated=%v event=%s location=%q", got.Truncated, got.EventID, got.Findings[0].Location)
		}
		if string(event.Payload) != string(original) {
			t.Fatal("deriving summaries modified the event payload")
		}
		short := derive(summaryRecorded(1, 0, version(1, 1, "short")))["1"]
		if short.Truncated {
			t.Fatal("short summary marked truncated")
		}
	})
}
