package storetest

// ReviewSummary conformance (req-review-gates-evidence AC-10.1, AC-10.2,
// AC-10.4, AC-10.7; DEC-79; feature-review-round-summary SU-2, SU-7). Every
// case runs unchanged on memory, PostgreSQL, and SingleStore.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runReviewSummary(t *testing.T, x Fixture) {
	t.Run("OpenRound", func(t *testing.T) { runReviewSummaryOpenRound(t, x) })
	t.Run("MissingRound", func(t *testing.T) { runReviewSummaryMissingRound(t, x) })
	t.Run("RoundZero", func(t *testing.T) { runReviewSummaryRoundZero(t, x) })
	t.Run("FieldBounds", func(t *testing.T) { runReviewSummaryFieldBounds(t, x) })
	t.Run("UnknownSeat", func(t *testing.T) { runReviewSummaryUnknownSeat(t, x) })
	t.Run("UncoveredChangesRequestedSeats", func(t *testing.T) { runReviewSummaryCoverage(t, x) })
	t.Run("ApprovedRoundNoFindings", func(t *testing.T) { runReviewSummaryApprovedRound(t, x) })
	t.Run("MultipleBlockingSeats", func(t *testing.T) { runReviewSummaryMultipleBlockingSeats(t, x) })
	t.Run("ConsecutiveVersions", func(t *testing.T) { runReviewSummaryConsecutiveVersions(t, x) })
	t.Run("IdenticalReplay", func(t *testing.T) { runReviewSummaryIdenticalReplay(t, x) })
	t.Run("ConflictingReplay", func(t *testing.T) { runReviewSummaryConflictingReplay(t, x) })
	t.Run("RoundIsolation", func(t *testing.T) { runReviewSummaryRoundIsolation(t, x) })
	t.Run("WorkspaceIsolation", func(t *testing.T) { runReviewSummaryWorkspaceIsolation(t, x) })
	t.Run("ConcurrentVersions", func(t *testing.T) { runReviewSummaryConcurrentVersions(t, x) })
	t.Run("ConcurrentIdenticalReplay", func(t *testing.T) { runReviewSummaryConcurrentReplay(t, x) })
	t.Run("InformativeOnly", func(t *testing.T) { runReviewSummaryInformativeOnly(t, x) })
}

// summaryRound creates review round `round` on task with one seat per
// verdict and settles each seat through the real acceptance path, so the
// round's review.round_completed event is the one the aggregation writes.
// A nil verdict leaves its seat open.
func summaryRound(t *testing.T, x Fixture, taskID string, round int, verdicts ...string) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= len(verdicts); seat++ {
		id := fmt.Sprintf("%s-review-%d-seat-%d", taskID, round, seat)
		jobs = append(jobs, core.Job{ID: id, TaskID: taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: core.StageReview, ReviewRound: round, ReviewSeat: seat, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)})
	}
	requireOK(t, CreateReviewRound(ctx, st, taskID, jobs, orders))
	for i, verdict := range verdicts {
		if verdict == "" {
			continue
		}
		session := orders[i].ID + "-session"
		_, err := ClaimWorkOrder(ctx, st, orders[i].ID, core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		decision := core.ReviewDecision{TaskID: taskID, JobID: orders[i].JobID, ReviewWorkOrderID: orders[i].ID, ReviewRound: round, ReviewSeat: i + 1, ClaimSession: session, Verdict: verdict, ReasonCode: "verified", Summary: "summary fixture", PolicyVersion: 1, MaxBounces: 5}
		if verdict == "changes_requested" {
			decision.ReasonCode, decision.Feedback = "defect", fmt.Sprintf("seat %d defect", i+1)
		}
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
	}
}

// settledSummaryTask creates a task whose round 1 settled with verdicts.
func settledSummaryTask(t *testing.T, x Fixture, verdicts ...string) core.Task {
	t.Helper()
	task := newAggregateTask(t, x)
	summaryRound(t, x, task.ID, 1, verdicts...)
	return task
}

func summaryActorRequest(task core.Task, round int, requestID string, findings ...store.ReviewSummaryFinding) store.ReviewSummaryRequest {
	return store.ReviewSummaryRequest{
		TaskID: task.ID, ReviewRound: round, RequestID: requestID, Summary: "  Consolidated review of round " + fmt.Sprint(round) + ".  ",
		Findings: findings,
		Author:   store.ReviewSummaryAuthor{ActorID: "user:summary-author", ActorRole: core.ActorUser, Model: "reported-model"},
	}
}

func blockingFinding(seats ...int) store.ReviewSummaryFinding {
	return store.ReviewSummaryFinding{Severity: store.ReviewSummarySeverityBlocking, Problem: "Seats found the same defect.", Location: "internal/store/review_summary.go", Fix: "Fix it.", Seats: seats}
}

func summaryEvents(t *testing.T, x Fixture, taskID string) []core.Event {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	var summaries []core.Event
	for _, event := range events {
		if event.Kind == store.ReviewSummaryRecordedEvent {
			summaries = append(summaries, event)
		}
	}
	return summaries
}

func eventCount(t *testing.T, x Fixture, taskID string) int {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	return len(events)
}

// requireSummaryRefusal records request, requires target, and requires the
// task's event list to be unchanged.
func requireSummaryRefusal(t *testing.T, x Fixture, label string, request store.ReviewSummaryRequest, target error, contains ...string) {
	t.Helper()
	before := eventCount(t, x, request.TaskID)
	_, err := x.Backend.RecordReviewSummaryCommand(x.Context, request)
	if !errors.Is(err, target) {
		t.Fatalf("%s: err=%v, want %v", label, err, target)
	}
	for _, text := range contains {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("%s: err=%q, want it to contain %q", label, err, text)
		}
	}
	if after := eventCount(t, x, request.TaskID); after != before {
		t.Fatalf("%s: refusal changed the event list %d -> %d", label, before, after)
	}
}

func recordSummary(t *testing.T, x Fixture, request store.ReviewSummaryRequest) store.ReviewSummaryResult {
	t.Helper()
	result, err := x.Backend.RecordReviewSummaryCommand(x.Context, request)
	requireOK(t, err)
	if result.TaskID != request.TaskID || result.ReviewRound != request.ReviewRound || result.EventID == 0 || result.RecordedAt.IsZero() {
		t.Fatalf("summary result=%+v for request %+v", result, request)
	}
	return result
}

func runReviewSummaryOpenRound(t *testing.T, x Fixture) {
	open := newAggregateTask(t, x)
	summaryRound(t, x, open.ID, 1, "approve", "")
	requireSummaryRefusal(t, x, "open round", summaryActorRequest(open, 1, "open"), store.ErrReviewSummaryRoundNotSettled)
}

func runReviewSummaryMissingRound(t *testing.T, x Fixture) {
	settled := settledSummaryTask(t, x, "approve")
	requireSummaryRefusal(t, x, "missing round", summaryActorRequest(settled, 2, "missing"), store.ErrReviewSummaryRoundNotSettled)
	missing := core.Task{ID: core.NewTaskID()}
	if _, err := x.Backend.RecordReviewSummaryCommand(x.Context, summaryActorRequest(missing, 1, "absent")); err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task err=%v, want not found", err)
	}
}

func runReviewSummaryRoundZero(t *testing.T, x Fixture) {
	settled := settledSummaryTask(t, x, "approve")
	requireSummaryRefusal(t, x, "round zero", summaryActorRequest(settled, 0, "zero"), store.ErrReviewSummaryRoundNotSettled)
	requireSummaryRefusal(t, x, "negative round", summaryActorRequest(settled, -1, "negative"), store.ErrReviewSummaryRoundNotSettled)
}

func runReviewSummaryFieldBounds(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "changes_requested")
	accepted := func(label string, mutate func(*store.ReviewSummaryRequest)) {
		t.Helper()
		request := summaryActorRequest(task, 1, "bounds-"+label, blockingFinding(1))
		mutate(&request)
		recordSummary(t, x, request)
	}
	refused := func(label string, mutate func(*store.ReviewSummaryRequest), contains string) {
		t.Helper()
		request := summaryActorRequest(task, 1, "bounds-"+label, blockingFinding(1))
		mutate(&request)
		requireSummaryRefusal(t, x, label, request, store.ErrReviewSummaryInvalid, contains)
	}
	// Character bounds count runes: a two-byte or three-byte character counts once.
	accepted("summary-min", func(r *store.ReviewSummaryRequest) { r.Summary = "x" })
	accepted("summary-max-unicode", func(r *store.ReviewSummaryRequest) { r.Summary = strings.Repeat("é", store.ReviewSummaryMaxSummary) })
	accepted("summary-max-trimmed", func(r *store.ReviewSummaryRequest) {
		r.Summary = "  " + strings.Repeat("s", store.ReviewSummaryMaxSummary) + "\n"
	})
	refused("summary-empty", func(r *store.ReviewSummaryRequest) { r.Summary = "" }, "summary")
	refused("summary-whitespace", func(r *store.ReviewSummaryRequest) { r.Summary = " \n\t " }, "summary")
	refused("summary-over", func(r *store.ReviewSummaryRequest) { r.Summary = strings.Repeat("é", store.ReviewSummaryMaxSummary+1) }, "summary")
	accepted("problem-min-max", func(r *store.ReviewSummaryRequest) {
		r.Findings = []store.ReviewSummaryFinding{{Severity: "blocking", Problem: "p", Seats: []int{1}}, {Severity: "non-blocking", Problem: strings.Repeat("界", store.ReviewSummaryMaxProblem), Seats: []int{1}}}
	})
	refused("problem-empty", func(r *store.ReviewSummaryRequest) { r.Findings[0].Problem = "" }, "problem")
	refused("problem-over", func(r *store.ReviewSummaryRequest) {
		r.Findings[0].Problem = strings.Repeat("界", store.ReviewSummaryMaxProblem+1)
	}, "problem")
	accepted("location-fix-max", func(r *store.ReviewSummaryRequest) {
		r.Findings[0].Location = strings.Repeat("ü", store.ReviewSummaryMaxLocation)
		r.Findings[0].Fix = strings.Repeat("ü", store.ReviewSummaryMaxFix)
	})
	accepted("location-fix-absent", func(r *store.ReviewSummaryRequest) { r.Findings[0].Location, r.Findings[0].Fix = "", "" })
	refused("location-over", func(r *store.ReviewSummaryRequest) {
		r.Findings[0].Location = strings.Repeat("ü", store.ReviewSummaryMaxLocation+1)
	}, "location")
	refused("fix-over", func(r *store.ReviewSummaryRequest) {
		r.Findings[0].Fix = strings.Repeat("ü", store.ReviewSummaryMaxFix+1)
	}, "fix")
	accepted("model-max", func(r *store.ReviewSummaryRequest) { r.Author.Model = strings.Repeat("m", store.ReviewSummaryMaxModel) })
	accepted("model-absent", func(r *store.ReviewSummaryRequest) { r.Author.Model = "" })
	refused("model-over", func(r *store.ReviewSummaryRequest) {
		r.Author.Model = strings.Repeat("m", store.ReviewSummaryMaxModel+1)
	}, "model")
	accepted("findings-max", func(r *store.ReviewSummaryRequest) {
		r.Findings = nil
		for range store.ReviewSummaryMaxFindings {
			r.Findings = append(r.Findings, blockingFinding(1))
		}
	})
	refused("findings-over", func(r *store.ReviewSummaryRequest) {
		r.Findings = nil
		for range store.ReviewSummaryMaxFindings + 1 {
			r.Findings = append(r.Findings, blockingFinding(1))
		}
	}, "findings")
	refused("severity-unknown", func(r *store.ReviewSummaryRequest) { r.Findings[0].Severity = "major" }, "severity")
	refused("seats-empty", func(r *store.ReviewSummaryRequest) { r.Findings[0].Seats = nil }, "seats")
	refused("seats-duplicate", func(r *store.ReviewSummaryRequest) { r.Findings[0].Seats = []int{1, 1} }, "repeats seat 1")
	refused("request-id-missing", func(r *store.ReviewSummaryRequest) { r.RequestID = "  " }, "request_id")
	stored := summaryEvents(t, x, task.ID)
	var payload store.ReviewSummaryPayload
	requireOK(t, json.Unmarshal(stored[2].Payload, &payload))
	if payload.Summary != strings.Repeat("s", store.ReviewSummaryMaxSummary) {
		t.Fatalf("stored summary was not trimmed: %d characters", len(payload.Summary))
	}
}

func runReviewSummaryUnknownSeat(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "approve", "approve")
	requireSummaryRefusal(t, x, "unknown seat", summaryActorRequest(task, 1, "unknown", blockingFinding(1, 3)), store.ErrReviewSummaryInvalid, "seat 3")
	requireSummaryRefusal(t, x, "seat zero", summaryActorRequest(task, 1, "zero-seat", blockingFinding(0)), store.ErrReviewSummaryInvalid, "seat 0")
}

func runReviewSummaryCoverage(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "changes_requested", "approve", "changes_requested")
	requireSummaryRefusal(t, x, "no findings", summaryActorRequest(task, 1, "none"), store.ErrReviewSummaryInvalid, "uncited seats: 1, 3")
	requireSummaryRefusal(t, x, "seat three uncited", summaryActorRequest(task, 1, "partial", blockingFinding(1, 2)), store.ErrReviewSummaryInvalid, "uncited seats: 3")
	// A non-blocking finding cites a seat as well as a blocking one does.
	nonBlocking := store.ReviewSummaryFinding{Severity: store.ReviewSummarySeverityNonBlocking, Problem: "Style.", Seats: []int{3}}
	recordSummary(t, x, summaryActorRequest(task, 1, "covered", blockingFinding(1), nonBlocking))
}

func runReviewSummaryApprovedRound(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "approve", "approve")
	result := recordSummary(t, x, summaryActorRequest(task, 1, "approved"))
	if result.Version != 1 || result.Replayed {
		t.Fatalf("approved round result=%+v", result)
	}
	events := summaryEvents(t, x, task.ID)
	if len(events) != 1 || events[0].ID != result.EventID || !events[0].At.Equal(result.RecordedAt) {
		t.Fatalf("approved round summary events=%+v result=%+v", events, result)
	}
	var payload map[string]any
	requireOK(t, json.Unmarshal(events[0].Payload, &payload))
	want := map[string]any{
		"workspace_id": x.Workspace, "review_round": float64(1), "version": float64(1), "request_id": "approved",
		"summary": "Consolidated review of round 1.", "findings": []any{},
		"author": map[string]any{"actor_id": "user:summary-author", "actor_role": string(core.ActorUser), "model": "reported-model", "model_enforcement": "self-reported"},
	}
	if !reflect.DeepEqual(payload, want) {
		t.Fatalf("payload=%v\nwant=%v", payload, want)
	}
}

func runReviewSummaryMultipleBlockingSeats(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "changes_requested", "changes_requested")
	result := recordSummary(t, x, summaryActorRequest(task, 1, "both", blockingFinding(1, 2)))
	if result.Version != 1 {
		t.Fatalf("result=%+v", result)
	}
}

// twoSummaryVersions records versions 1 and 2 of round 1 on a fresh task.
func twoSummaryVersions(t *testing.T, x Fixture) (core.Task, store.ReviewSummaryRequest, store.ReviewSummaryResult, store.ReviewSummaryResult) {
	t.Helper()
	task := settledSummaryTask(t, x, "changes_requested")
	first := summaryActorRequest(task, 1, "first", blockingFinding(1))
	v1 := recordSummary(t, x, first)
	second := summaryActorRequest(task, 1, "second", blockingFinding(1))
	second.Summary = "A revised consolidation."
	v2 := recordSummary(t, x, second)
	if v1.Version != 1 || v2.Version != 2 || v1.EventID == v2.EventID || v1.Replayed || v2.Replayed {
		t.Fatalf("versions v1=%+v v2=%+v", v1, v2)
	}
	return task, first, v1, v2
}

func runReviewSummaryConsecutiveVersions(t *testing.T, x Fixture) {
	task, _, v1, v2 := twoSummaryVersions(t, x)
	v3 := recordSummary(t, x, summaryActorRequest(task, 1, "third", blockingFinding(1)))
	if v3.Version != 3 {
		t.Fatalf("third version=%+v", v3)
	}
	// Recorded versions stay readable and unchanged through the task events.
	events := summaryEvents(t, x, task.ID)
	if len(events) != 3 || events[0].ID != v1.EventID || events[1].ID != v2.EventID || events[2].ID != v3.EventID {
		t.Fatalf("summary events=%+v", events)
	}
	for i, event := range events {
		var payload store.ReviewSummaryPayload
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if payload.Version != i+1 || payload.ReviewRound != 1 || payload.WorkspaceID != x.Workspace {
			t.Fatalf("version %d payload=%+v", i+1, payload)
		}
	}
	var first store.ReviewSummaryPayload
	requireOK(t, json.Unmarshal(events[0].Payload, &first))
	if first.RequestID != "first" || first.Summary != "Consolidated review of round 1." || len(first.Findings) != 1 {
		t.Fatalf("first version changed: %+v", first)
	}
}

func runReviewSummaryIdenticalReplay(t *testing.T, x Fixture) {
	task, first, v1, _ := twoSummaryVersions(t, x)
	before := eventCount(t, x, task.ID)
	// An identical replay after a later version returns the original event.
	replay, err := x.Backend.RecordReviewSummaryCommand(x.Context, first)
	requireOK(t, err)
	if !replay.Replayed || replay.Version != 1 || replay.EventID != v1.EventID || !replay.RecordedAt.Equal(v1.RecordedAt) || replay.TaskID != task.ID {
		t.Fatalf("replay=%+v, want the original %+v", replay, v1)
	}
	if after := eventCount(t, x, task.ID); after != before {
		t.Fatalf("replay appended events %d -> %d", before, after)
	}
}

func runReviewSummaryConflictingReplay(t *testing.T, x Fixture) {
	task, _, _, _ := twoSummaryVersions(t, x)
	for label, mutate := range map[string]func(*store.ReviewSummaryRequest){
		"summary":  func(r *store.ReviewSummaryRequest) { r.Summary = "Different." },
		"finding":  func(r *store.ReviewSummaryRequest) { r.Findings[0].Problem = "Different." },
		"findings": func(r *store.ReviewSummaryRequest) { r.Findings = append(r.Findings, blockingFinding(1)) },
		"model":    func(r *store.ReviewSummaryRequest) { r.Author.Model = "other-model" },
		"author":   func(r *store.ReviewSummaryRequest) { r.Author.ActorID = "user:someone-else" },
		"session":  func(r *store.ReviewSummaryRequest) { r.Author.WorkOrderID, r.Author.SessionID = "order", "session" },
	} {
		changed := summaryActorRequest(task, 1, "first", blockingFinding(1))
		mutate(&changed)
		requireSummaryRefusal(t, x, "conflicting replay of "+label, changed, store.ErrReviewSummaryConflict)
	}
}

func runReviewSummaryRoundIsolation(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "changes_requested")
	summaryRound(t, x, task.ID, 2, "approve")
	r1 := recordSummary(t, x, summaryActorRequest(task, 1, "shared", blockingFinding(1)))
	r2 := recordSummary(t, x, summaryActorRequest(task, 2, "shared"))
	if r1.Version != 1 || r2.Version != 1 || r2.Replayed || r1.EventID == r2.EventID {
		t.Fatalf("rounds share versions or request IDs: r1=%+v r2=%+v", r1, r2)
	}
	r1v2 := recordSummary(t, x, summaryActorRequest(task, 1, "later", blockingFinding(1)))
	if r1v2.Version != 2 {
		t.Fatalf("round one second version=%+v", r1v2)
	}
}

func runReviewSummaryWorkspaceIsolation(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "approve")
	before := eventCount(t, x, task.ID)
	foreign := store.WithWorkspace(x.Context, "summary-foreign-"+strings.ToLower(core.NewTaskID()))
	if _, err := x.Backend.RecordReviewSummaryCommand(foreign, summaryActorRequest(task, 1, "foreign")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign workspace err=%v, want not found", err)
	}
	if after := eventCount(t, x, task.ID); after != before {
		t.Fatalf("foreign workspace request appended events %d -> %d", before, after)
	}
}

// concurrentSummaries starts every request at once behind a closed channel
// and joins them all; no case sleeps.
func concurrentSummaries(t *testing.T, x Fixture, requests []store.ReviewSummaryRequest) []store.ReviewSummaryResult {
	t.Helper()
	start := make(chan struct{})
	results := make([]store.ReviewSummaryResult, len(requests))
	errs := make([]error, len(requests))
	var ready, done sync.WaitGroup
	for i := range requests {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = x.Backend.RecordReviewSummaryCommand(x.Context, requests[i])
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent request %d: %v", i, err)
		}
	}
	return results
}

func runReviewSummaryConcurrentVersions(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "approve")
	const n = 6
	var requests []store.ReviewSummaryRequest
	for i := range n {
		requests = append(requests, summaryActorRequest(task, 1, fmt.Sprintf("concurrent-%d", i)))
	}
	results := concurrentSummaries(t, x, requests)
	versions := map[int]bool{}
	for _, result := range results {
		versions[result.Version] = true
	}
	for version := 1; version <= n; version++ {
		if !versions[version] {
			t.Fatalf("concurrent versions=%v, want 1..%d", versions, n)
		}
	}
	if events := summaryEvents(t, x, task.ID); len(events) != n {
		t.Fatalf("concurrent summaries appended %d events, want %d", len(events), n)
	}
}

func runReviewSummaryConcurrentReplay(t *testing.T, x Fixture) {
	task := settledSummaryTask(t, x, "approve")
	const n = 6
	var requests []store.ReviewSummaryRequest
	for range n {
		requests = append(requests, summaryActorRequest(task, 1, "same-request"))
	}
	results := concurrentSummaries(t, x, requests)
	events := summaryEvents(t, x, task.ID)
	if len(events) != 1 {
		t.Fatalf("identical concurrent requests appended %d events", len(events))
	}
	replays := 0
	for _, result := range results {
		if result.Version != 1 || result.EventID != events[0].ID || !result.RecordedAt.Equal(events[0].At) {
			t.Fatalf("result=%+v, want the one recorded event %+v", result, events[0])
		}
		if result.Replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("replays=%d, want %d", replays, n-1)
	}
}

// reviewSummaryState is everything a summary must leave unchanged (AC-10.7).
type reviewSummaryState struct {
	Task          core.Task
	WorkOrders    []core.WorkOrder
	Jobs          []core.Job
	Interventions []core.Intervention
	Events        []core.Event
}

func reviewSummarySnapshot(t *testing.T, x Fixture, taskID string) reviewSummaryState {
	t.Helper()
	var state reviewSummaryState
	var err error
	state.Task, err = x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	state.WorkOrders, err = x.Backend.ListTaskWorkOrdersStored(x.Context, taskID)
	requireOK(t, err)
	state.Jobs, err = x.Backend.ListJobs(x.Context, taskID)
	requireOK(t, err)
	state.Interventions, err = x.Backend.ListInterventions(x.Context, taskID)
	requireOK(t, err)
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	for _, event := range events {
		if event.Kind != store.ReviewSummaryRecordedEvent {
			state.Events = append(state.Events, event)
		}
	}
	return state
}

func runReviewSummaryInformativeOnly(t *testing.T, x Fixture) {
	for _, verdicts := range [][]string{{"changes_requested", "approve"}, {"approve", "approve"}} {
		t.Run(verdicts[0], func(t *testing.T) {
			task := settledSummaryTask(t, x, verdicts...)
			before := reviewSummarySnapshot(t, x, task.ID)
			bounces := 0
			for _, event := range before.Events {
				if event.Kind == "pipeline.bounced" {
					bounces++
				}
			}
			if wantBounce := verdicts[0] == "changes_requested"; (bounces == 1) != wantBounce {
				t.Fatalf("fixture bounces=%d for verdicts %v", bounces, verdicts)
			}
			request := summaryActorRequest(task, 1, "informative", blockingFinding(1))
			first := recordSummary(t, x, request)
			if replay, err := x.Backend.RecordReviewSummaryCommand(x.Context, request); err != nil || !replay.Replayed || replay.EventID != first.EventID {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			requireSummaryRefusal(t, x, "conflict", summaryActorRequest(task, 1, "informative"), store.ErrReviewSummaryConflict)
			requireSummaryRefusal(t, x, "unsettled", summaryActorRequest(task, 2, "unsettled"), store.ErrReviewSummaryRoundNotSettled)
			after := reviewSummarySnapshot(t, x, task.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("summary changed task state:\nbefore=%+v\nafter=%+v", before, after)
			}
			if events := summaryEvents(t, x, task.ID); len(events) != 1 {
				t.Fatalf("summary events=%d, want exactly one", len(events))
			}
		})
	}
}
