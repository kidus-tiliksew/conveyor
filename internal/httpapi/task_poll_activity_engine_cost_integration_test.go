package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// engineCostPageTasks is the size of the measured page: one SingleStore
// marker chunk (markerTaskChunk) of 100 tasks.
const engineCostPageTasks = 100

// engineCostHeavySnapshotBytes is the least encoded size of the
// governance_snapshot each historical claim and submission event of the
// heavy fixture stores. Work-order events of the staging workspace that
// reported both regressions held a snapshot of 100 KB and more.
const engineCostHeavySnapshotBytes = 100 << 10

// engineCostLightSnapshotBytes is the least encoded size of the same events'
// snapshot in the light fixture.
const engineCostLightSnapshotBytes = 256

// engineCostLongRenewals is the number of lease renewals the heavy fixture
// adds to the verify job of one page task with an open review order. The
// light fixture adds them to the verify job of a task outside the page, so
// both fixtures hold the same events per kind.
const engineCostLongRenewals = 6000

// engineCostShortRenewals is the number of renewals each claim of the
// fixture's other jobs holds.
const engineCostShortRenewals = 2

// engineCostProgressUpdates is the number of progress updates each
// historical claim records: work_order.updated events of the claimed order,
// which a writer before the compact payload stored with the snapshot. Most
// of the large events of the staging workspace were these.
const engineCostProgressUpdates = 3

// engineCostWarmRounds is the number of reads of each fixture before the
// measured pairs. They pay the one-time statement compile and schema
// discovery, so the pairs measure repeated engine work.
const engineCostWarmRounds = 2

// engineCostPairs is the number of measured pairs: one read of each fixture,
// alternately heavy first and light first.
const engineCostPairs = 9

// engineCostSeedWorkers writes the fixture concurrently; each task's history
// stays in one worker.
const engineCostSeedWorkers = 8

// engineCostLongTask is the page task whose verify job holds the long
// renewal history in the heavy fixture. Its group has an open review order
// with a lapsed claim.
const engineCostLongTask = 5

// engineCostOffPageTask is the task outside the page that holds the long
// renewal history in the light fixture.
const engineCostOffPageTask = "cost-off"

// engineCostFactors bounds the median ratio of the heavy read's elapsed time
// to the light read's for each backend. Medians measured with this test on
// main dc3fdccc, on SingleStore 10.9 (the CI image) and PostgreSQL 16:
//
//	backend      read                          CI runs       local runs
//	singlestore  main                          1.45 1.41 1.69 1.46  1.33 to 1.73 (8 runs)
//	singlestore  payload text test (#1714)     -             3.95 to 4.36 (5 runs)
//	singlestore  every renewal read (#1715)    -             30.2 to 33.1 (4 runs)
//	postgres     main                          11.56 11.45 12.54 11.70  9.2 to 11.0 (6 runs)
//
// The SingleStore factor is about 1.45 times the highest clean median and
// below the lowest median of either restored defect by the same margin. On
// PostgreSQL the ratio comes from the stored snapshots: with small heavy
// snapshots it was 1.08. Its factor is about 1.44 times the highest CI
// median of 12.54, leaving a noise margin below the factor of 18.
var engineCostFactors = map[string]float64{
	"postgres":    18,
	"singlestore": 2.5,
}

// engineCostTwin is one of the two fixture databases.
type engineCostTwin struct {
	label string
	heavy bool
	name  string
	st    taskPollStore
	ctx   context.Context
	open  time.Duration
	seed  time.Duration
}

// TestTaskPollActivityEngineCostIntegration measures the work the database
// does for rows the activity read does not return.
// TestTaskPollReadCostIntegration bounds the bytes a poll receives; two
// SingleStore regressions changed the time the engine spent and not the
// bytes it returned: a text test on the stored payload in the narrow marker
// read, and a lifecycle read that decoded every renewal of a task in review.
//
// The test seeds two fresh databases on the same server with the same
// tasks, orders, and events per kind. The heavy fixture stores historical
// claim, progress, and submission events with a governance snapshot of
// 100 KiB or more, as work-order events stored before the compact payload,
// and holds a long renewal history on a verify job of a page task in
// review. The light fixture stores the same events with small snapshots and
// holds the long renewal history on a task outside the page. On SingleStore
// both fixtures' events are flushed into column segments. The test then
// times ReadActivityInputs of the same page at the same instant on each
// fixture in alternating pairs, and fails when the median heavy-to-light
// ratio is above the backend's factor. Both reads run on the same server in
// the same run, so the ratio does not depend on the speed of the runner, and
// no elapsed time is bounded.
func TestTaskPollActivityEngineCostIntegration(t *testing.T) {
	for _, backend := range taskPollBackends {
		t.Run(backend.name, func(t *testing.T) {
			raw := taskPollBackendURL(t, backend.name, backend.variable)
			const ws = "engine-cost"
			now := taskPollNow
			cfg := &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
			// Both databases are opened and migrated at once, each in its own
			// subtest, and belong to this test, which reads them after both
			// subtests end. The fixtures are then seeded one after the other:
			// on one SingleStore server, seeding both at once took twice as
			// long as in turn.
			setup := time.Now()
			outer, owner := t.Context(), t
			twins := []*engineCostTwin{{label: "heavy", heavy: true}, {label: "light"}}
			opened := t.Run("open", func(t *testing.T) {
				for _, twin := range twins {
					t.Run(twin.label, func(t *testing.T) {
						t.Parallel()
						ctx := store.WithActor(outer, store.SystemActor())
						twin.name = fmt.Sprintf("conveyor_cost_%s_%d_test", twin.label, time.Now().UnixNano())
						opening := time.Now()
						st := openTaskPollDatabase(t, owner, ctx, backend.name, raw, twin.name, true, false)
						ctx = store.WithWorkspace(ctx, ws)
						if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
							t.Fatal(err)
						}
						twin.open = time.Since(opening)
						twin.st, twin.ctx = st, ctx
					})
				}
			})
			if !opened {
				return // The opener already reported its failure.
			}
			for _, twin := range twins {
				if twin.st == nil {
					t.Skipf("the %s opener was excluded by the test filter", twin.label)
				}
			}
			for _, twin := range twins {
				started := time.Now()
				seedEngineCost(t, twin.ctx, twin.st, ws, now, twin.heavy)
				if backend.name == "singlestore" {
					settleEngineCostSingleStore(t, twin.ctx, raw, twin.name)
				}
				twin.seed = time.Since(started)
				t.Logf("opened and migrated the %s database on %s in %s, then seeded it in %s", twin.label, backend.name, twin.open.Round(time.Millisecond), twin.seed.Round(time.Millisecond))
			}
			setupTime := time.Since(setup)
			heavy, light := twins[0], twins[1]
			page := engineCostPage()

			// Every check of the fixture and the read's result runs outside
			// the timed reads.
			checking := time.Now()
			for _, twin := range twins {
				checkEngineCostStorage(t, twin)
			}
			heavyInputs := checkEngineCostRead(t, heavy, page, now)
			lightInputs := checkEngineCostRead(t, light, page, now)
			requireEngineCostTwinsAgree(t, heavyInputs, lightInputs)
			t.Logf("checked both fixtures and their reads in %s", time.Since(checking).Round(time.Millisecond))
			measuring := time.Now()

			read := func(twin *engineCostTwin) time.Duration {
				started := time.Now()
				inputs, err := twin.st.ReadActivityInputs(twin.ctx, page, now)
				elapsed := time.Since(started)
				if err != nil {
					t.Fatalf("%s read: %v", twin.label, err)
				}
				if len(inputs.Markers) != len(page) {
					t.Fatalf("%s read returned %d markers, want %d", twin.label, len(inputs.Markers), len(page))
				}
				return elapsed
			}
			for range engineCostWarmRounds {
				read(heavy)
				read(light)
			}
			type pair struct {
				HeavyFirst bool    `json:"heavy_first"`
				HeavyMS    float64 `json:"heavy_ms"`
				LightMS    float64 `json:"light_ms"`
				Ratio      float64 `json:"ratio"`
			}
			pairs := make([]pair, engineCostPairs)
			ratios := make([]float64, engineCostPairs)
			for i := range pairs {
				var h, l time.Duration
				if i%2 == 0 {
					h, l = read(heavy), read(light)
				} else {
					l, h = read(light), read(heavy)
				}
				ratio := h.Seconds() / max(l.Seconds(), 1e-6)
				pairs[i] = pair{HeavyFirst: i%2 == 0, HeavyMS: h.Seconds() * 1000, LightMS: l.Seconds() * 1000, Ratio: ratio}
				ratios[i] = ratio
			}
			median := engineCostMedian(ratios)
			factor := engineCostFactors[backend.name]
			report, err := json.Marshal(map[string]any{
				"backend":              backend.name,
				"page_tasks":           len(page),
				"heavy_snapshot_bytes": engineCostHeavySnapshotBytes,
				"progress_updates":     engineCostProgressUpdates,
				"long_renewals":        engineCostLongRenewals,
				"warm_rounds":          engineCostWarmRounds,
				"open_heavy_ms":        heavy.open.Milliseconds(),
				"open_light_ms":        light.open.Milliseconds(),
				"seed_heavy_ms":        heavy.seed.Milliseconds(),
				"seed_light_ms":        light.seed.Milliseconds(),
				"setup_ms":             setupTime.Milliseconds(),
				"measure_ms":           time.Since(measuring).Milliseconds(),
				"pairs":                pairs,
				"median_ratio":         median,
				"factor":               factor,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("activity read engine cost: %s", report)
			if os.Getenv("CONVEYOR_TASK_POLL_REPORT_ONLY") == "1" {
				t.Logf("report only: median heavy-to-light ratio %.3f; factor %.2f", median, factor)
				return
			}
			if median > factor {
				t.Fatalf("the activity read of the heavy fixture took a median %.3f times as long as the light fixture's on %s, above the factor %.2f", median, backend.name, factor)
			}
		})
	}
}

func engineCostTaskID(index int) string { return fmt.Sprintf("cost-%03d", index) }

// engineCostPage is the measured page: every fixture task except the task
// outside the page, in index order.
func engineCostPage() []string {
	page := make([]string, engineCostPageTasks)
	for i := range page {
		page[i] = engineCostTaskID(i)
	}
	return page
}

func engineCostMedian(values []float64) float64 {
	sorted := slices.Clone(values)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// engineCostWords is the vocabulary of the synthetic snapshot text. Varied
// words keep the stored snapshot from compressing as a repeated character
// would.
var engineCostWords = strings.Fields(`activity read marker fold claim renewal lease review order
	verdict round submission merge failure forge head base branch task stage plan implement verify
	snapshot governance design decision requirement statement section heading invariant boundary
	surface event ledger payload compact projection workspace page chunk statement compile engine
	column table index partition backend store command transition queue dispatch recovery horizon
	diagnostic recovery checkpoint authority confirmed version digest pin reference context`)

// engineCostSnapshot is a structured synthetic governance snapshot of at
// least size encoded bytes: designs with sections of deterministic text and
// decisions, as a claim-time snapshot holds.
func engineCostSnapshot(seed uint64, size int) json.RawMessage {
	type section struct {
		ID      string `json:"section_id"`
		Heading string `json:"heading"`
		Text    string `json:"text"`
	}
	type design struct {
		ID       string    `json:"id"`
		Title    string    `json:"title"`
		Version  int       `json:"version"`
		Sections []section `json:"sections"`
	}
	type decision struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	}
	random := rand.New(rand.NewPCG(seed, 0x6a09e667f3bcc908))
	text := func(words int) string {
		var b strings.Builder
		for i := range words {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(engineCostWords[random.IntN(len(engineCostWords))])
		}
		return b.String()
	}
	snapshot := struct {
		Designs   []design   `json:"designs"`
		Decisions []decision `json:"decisions"`
	}{}
	// Each pass adds one section of at most the words the remaining size
	// needs, four sections to a design, and one decision per design.
	for written := 0; ; {
		if len(snapshot.Designs) == 0 || len(snapshot.Designs[len(snapshot.Designs)-1].Sections) == 4 {
			snapshot.Designs = append(snapshot.Designs, design{ID: fmt.Sprintf("component-%s-%d", text(1), len(snapshot.Designs)), Title: text(4), Version: 1 + random.IntN(90)})
			snapshot.Decisions = append(snapshot.Decisions, decision{ID: fmt.Sprintf("DEC-%d", 1+random.IntN(150)), Summary: text(12)})
		}
		d := &snapshot.Designs[len(snapshot.Designs)-1]
		body := text(min(40+random.IntN(260), max(4, (size-written)/7)))
		d.Sections = append(d.Sections, section{ID: fmt.Sprintf("s%d", len(d.Sections)), Heading: text(3), Text: body})
		written += len(body) + 64
		if written < size {
			continue
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			panic(err)
		}
		if len(encoded) >= size {
			return encoded
		}
		written = len(encoded)
	}
}

// seedEngineCost writes one fixture through public store commands and
// AppendEvent. heavy selects the large snapshots and the page's long
// renewal history.
func seedEngineCost(t *testing.T, ctx context.Context, st store.Store, ws string, now time.Time, heavy bool) {
	t.Helper()
	indexes := make(chan int)
	failures := make(chan error, engineCostSeedWorkers)
	var workers sync.WaitGroup
	for range engineCostSeedWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range indexes {
				if err := seedEngineCostTask(ctx, st, ws, now, index, heavy); err != nil {
					failures <- err
					for range indexes {
					}
					return
				}
			}
		}()
	}
	// Index -1 is the task outside the page.
	for index := -1; index < engineCostPageTasks; index++ {
		indexes <- index
	}
	close(indexes)
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	// The long renewal history: on the page task in the heavy fixture and
	// on the task outside the page in the light one.
	owner := engineCostTaskID(engineCostLongTask)
	if !heavy {
		owner = engineCostOffPageTask
	}
	if err := seedEngineCostLongRenewals(ctx, st, owner, now); err != nil {
		t.Fatal(err)
	}
}

// engineCostVerifyClaimAt is when the verify claim of taskID starts. The long
// renewal history follows it, in the gap before the task's next event.
func engineCostVerifyClaimAt(taskID string, now time.Time) time.Time {
	return engineCostCreated(taskID, now).Add(30 * time.Minute)
}

func engineCostCreated(taskID string, now time.Time) time.Time {
	if taskID == engineCostOffPageTask {
		return now.Add(-time.Duration(engineCostPageTasks+1) * time.Hour)
	}
	var index int
	if _, err := fmt.Sscanf(taskID, "cost-%03d", &index); err != nil {
		panic(err)
	}
	return now.Add(-time.Duration(engineCostPageTasks-index) * time.Hour)
}

// seedEngineCostLongRenewals appends the long renewal history to the verify
// job of taskID, one renewal every 100 microseconds after its claim, in the
// simple payload the store writes: an attempt and a lease. Every lease is
// in the past at the read's instant.
func seedEngineCostLongRenewals(ctx context.Context, st store.Store, taskID string, now time.Time) error {
	jobID := taskID + "-verify-1"
	claimAt := engineCostVerifyClaimAt(taskID, now)
	indexes := make(chan int)
	failures := make(chan error, engineCostSeedWorkers)
	var workers sync.WaitGroup
	for range engineCostSeedWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range indexes {
				payload, err := json.Marshal(map[string]any{
					"attempt_id":       "attempt-" + jobID + "-long",
					"lease_expires_at": claimAt.Add(time.Duration(i+1) * time.Second).UTC(),
				})
				if err == nil {
					err = st.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: "work_order.lease_renewed", At: claimAt.Add(time.Duration(i+1) * 100 * time.Microsecond), Payload: payload})
				}
				if err != nil {
					failures <- fmt.Errorf("seed long renewal %d of %s: %w", i, taskID, err)
					for range indexes {
					}
					return
				}
			}
		}()
	}
	for i := range engineCostLongRenewals {
		indexes <- i
	}
	close(indexes)
	workers.Wait()
	close(failures)
	return <-failures
}

// The fixture's groups of ten page tasks. Each page task's group is its
// index modulo 10.
const (
	// engineCostMerged: a merged task with a plan, an implement submission,
	// a merge failure, a settled review round, and the merge.
	engineCostMerged = iota
	// engineCostMergeFailed: an implement submission whose merge failed.
	engineCostMergeFailed
	// engineCostResubmitted: a merge failure replaced by a later submission.
	engineCostResubmitted
	// engineCostRequestAnswered: a user change request followed by
	// implement claims.
	engineCostRequestAnswered
	// engineCostRequestPending: implement claims followed by a user change
	// request.
	engineCostRequestPending
	// engineCostReviewLapsed: an open review order whose claim lapsed, and a
	// verify job. Task engineCostLongTask is in this group.
	engineCostReviewLapsed
	// engineCostReviewLive: an open review order whose lease runs past the
	// read's instant.
	engineCostReviewLive
	// engineCostReviewRounds: two settled review rounds with verdicts.
	engineCostReviewRounds
	// engineCostPlanGate: a submitted plan at the plan approval gate.
	engineCostPlanGate
	// engineCostQueued: a queued implement order after an operator bounce.
	engineCostQueued
)

// engineCostLiveLease is the lease of the live review claim of the page task
// at index; its renewals extend it. The earliest claim lease of the page is
// the read's review lease horizon.
func engineCostLiveLease(index int, now time.Time) time.Time {
	return now.Add(time.Duration(10+index) * time.Minute)
}

type engineCostSeedError struct{ err error }

// seedEngineCostTask writes one fixture task and its history. Index -1 is
// the task outside the page.
func seedEngineCostTask(ctx context.Context, st store.Store, ws string, now time.Time, index int, heavy bool) (failure error) {
	defer func() {
		if r := recover(); r != nil {
			seedErr, ok := r.(engineCostSeedError)
			if !ok {
				panic(r)
			}
			failure = fmt.Errorf("seed task %d: %w", index, seedErr.err)
		}
	}()
	must := func(err error) {
		if err != nil {
			panic(engineCostSeedError{err})
		}
	}
	taskID, group := engineCostOffPageTask, engineCostReviewLapsed
	if index >= 0 {
		taskID, group = engineCostTaskID(index), index%10
	}
	created := engineCostCreated(taskID, now)
	task := core.Task{
		ID: taskID, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID,
		Title: "Engine cost fixture task " + taskID, Body: "Engine cost fixture task.", CreatedAt: created,
	}
	switch group {
	case engineCostMerged:
		task.State = core.TaskMerged
	case engineCostMergeFailed, engineCostResubmitted, engineCostRequestAnswered, engineCostRequestPending, engineCostQueued:
		task.State, task.NextStage = core.TaskRunning, core.StageImplement
	case engineCostReviewLapsed, engineCostReviewLive, engineCostReviewRounds:
		task.State, task.NextStage = core.TaskRunning, core.StageReview
	case engineCostPlanGate:
		task.State, task.GateKind, task.NextStage = core.TaskAwaiting, core.GateKindPlanApproval, core.StageImplement
	}
	must(st.CreateTaskWithDependencies(ctx, task, nil))

	at := created
	appendAt := func(when time.Time, jobID, kind string, payload json.RawMessage) {
		must(st.AppendEvent(ctx, core.Event{TaskID: taskID, JobID: jobID, Kind: kind, At: when, Payload: payload}))
	}
	appendEvent := func(jobID, kind string, payload any) {
		at = at.Add(time.Second)
		encoded, ok := payload.(json.RawMessage)
		if !ok {
			var err error
			encoded, err = json.Marshal(payload)
			must(err)
		}
		appendAt(at, jobID, kind, encoded)
	}
	appendEvent("", "task.created", map[string]any{"title": task.Title})
	newOrder := func(suffix string, stage core.Stage) core.WorkOrder {
		id := taskID + "-" + suffix
		must(st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
		order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, QueueEnteredAt: at, QueueDeadline: now.Add(48 * time.Hour)}
		must(storetest.CreateWorkOrder(ctx, st, order))
		return order
	}
	timeOut := func(orders ...core.WorkOrder) {
		for _, order := range orders {
			order.State = core.WorkOrderTimedOut
			must(storetest.UpdateWorkOrder(ctx, st, order, core.WorkOrderCmdTimeout))
		}
	}
	// legacy is a work-order event as a writer stored it before the compact
	// payload: the identity fields of store.WorkOrderEventPayload and a
	// top-level governance_snapshot body. Current writers store no body
	// (component-work-orders), so the fixture adds it only to its own copy
	// of the payload, to model the retained history the compact projection
	// reads around (component-task-lifecycle invariants 51-53).
	events := 0
	legacy := func(order core.WorkOrder) json.RawMessage {
		events++
		size := engineCostLightSnapshotBytes
		if heavy {
			size = engineCostHeavySnapshotBytes
		}
		seed := uint64(len(taskID))<<32 ^ uint64(index+1)<<16 ^ uint64(events)
		return json.RawMessage(core.AppendJSONObjectField(store.WorkOrderEventPayload(order), `"governance_snapshot":`+string(engineCostSnapshot(seed, size))))
	}
	// claim appends a historical claim of order with lease and its
	// renewals, each a minute later than the last.
	claim := func(order core.WorkOrder, attempt int, lease time.Time, renewals int) {
		at = at.Add(time.Second)
		claimed := order
		claimed.State = core.WorkOrderClaimed
		claimed.SessionID = fmt.Sprintf("session-%s-%d", order.ID, attempt)
		claimed.AttemptID = fmt.Sprintf("attempt-%s-%d", order.ID, attempt)
		claimed.ExecutionStartedAt = at
		claimed.LeaseExpiresAt = lease
		appendAt(at, order.JobID, "work_order.claimed", legacy(claimed))
		for update := range engineCostProgressUpdates {
			at = at.Add(time.Second)
			progress := claimed
			progress.Progress = fmt.Sprintf("Progress report %d of attempt %d.", update+1, attempt)
			appendAt(at, order.JobID, "work_order.updated", legacy(progress))
		}
		for renewal := range renewals {
			appendEvent(order.JobID, "work_order.lease_renewed", map[string]any{
				"attempt_id":       claimed.AttemptID,
				"lease_expires_at": lease.Add(time.Duration(renewal+1) * time.Minute).UTC(),
			})
		}
	}
	submit := func(order core.WorkOrder, head string) {
		at = at.Add(time.Second)
		submitted := order
		submitted.State = core.WorkOrderSubmitted
		submitted.HeadSHA = head
		appendAt(at, order.JobID, "work_order.updated", legacy(submitted))
	}
	past := func(hours int) time.Time { return created.Add(time.Duration(hours) * time.Hour) }
	head := func(n int) string { return fmt.Sprintf("head-%s-%d", taskID, n) }
	verdict := func(review core.WorkOrder, round int, outcome string) {
		appendEvent(review.JobID, "review.completed", map[string]any{"id": review.ID, "review_work_order_id": review.ID, "review_round": round, "review_seat": 1, "verdict": outcome})
		appendEvent("", "review.round_completed", map[string]any{"review_round": round, "verdict": outcome})
	}

	switch group {
	case engineCostMerged:
		spec := newOrder("spec-1", core.StageSpec)
		claim(spec, 0, past(1), engineCostShortRenewals)
		submit(spec, "")
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(2), engineCostShortRenewals)
		claim(implement, 1, past(3), engineCostShortRenewals)
		submit(implement, head(1))
		review := newOrder("review-1-seat-1", core.StageReview)
		claim(review, 0, past(4), engineCostShortRenewals)
		verdict(review, 1, "approve")
		appendEvent("", "merge.failed", map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict", "head_sha": head(1)})
		appendEvent("", "pull_request.merged", map[string]any{"head_sha": head(1)})
		timeOut(spec, implement, review)
	case engineCostMergeFailed:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		claim(implement, 1, past(2), engineCostShortRenewals)
		submit(implement, head(1))
		appendEvent("", "merge.failed", map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict", "head_sha": head(1)})
		timeOut(implement)
	case engineCostResubmitted:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		submit(implement, head(1))
		appendEvent("", "merge.failed", map[string]any{"forge_error_category": "merge_conflict", "error": "merge conflict", "head_sha": head(1)})
		timeOut(implement)
		// Each implement order counts once, at its first submission, so the
		// later head comes from a second order.
		fix := newOrder("implement-2", core.StageImplement)
		claim(fix, 0, past(2), engineCostShortRenewals)
		submit(fix, head(2))
		timeOut(fix)
	case engineCostRequestAnswered:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		submit(implement, head(1))
		appendEvent("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource, "feedback": "Change the read."})
		claim(implement, 1, past(2), engineCostShortRenewals)
		claim(implement, 2, past(3), engineCostShortRenewals)
		timeOut(implement)
	case engineCostRequestPending:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		claim(implement, 1, past(2), engineCostShortRenewals)
		submit(implement, head(1))
		appendEvent("", "pipeline.bounced", map[string]any{"source": store.UserRequestChangesSource, "feedback": "Change the read."})
		timeOut(implement)
	case engineCostReviewLapsed:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		submit(implement, head(1))
		verify := newOrder("verify-1", core.StageVerify)
		// The verify claim sits at a fixed instant, so the long renewal
		// history fits between it and the task's next event.
		claimed := verify
		claimed.State = core.WorkOrderClaimed
		claimed.SessionID, claimed.AttemptID = "session-"+verify.ID+"-long", "attempt-"+verify.ID+"-long"
		claimed.ExecutionStartedAt = engineCostVerifyClaimAt(taskID, now)
		claimed.LeaseExpiresAt = claimed.ExecutionStartedAt.Add(time.Minute)
		appendAt(claimed.ExecutionStartedAt, verify.JobID, "work_order.claimed", legacy(claimed))
		at = claimed.ExecutionStartedAt.Add(time.Second)
		for renewal := range engineCostShortRenewals {
			appendEvent(verify.JobID, "work_order.lease_renewed", map[string]any{"attempt_id": claimed.AttemptID, "lease_expires_at": claimed.LeaseExpiresAt.Add(time.Duration(renewal+1) * time.Minute).UTC()})
		}
		appendEvent("", "work_order.updated", legacy(core.WorkOrder{ID: verify.ID, TaskID: taskID, JobID: verify.JobID, Stage: core.StageVerify, State: core.WorkOrderSubmitted, HeadSHA: head(1)}))
		timeOut(implement, verify)
		review := newOrder("review-1-seat-1", core.StageReview)
		claim(review, 0, past(2), engineCostShortRenewals)
	case engineCostReviewLive:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		submit(implement, head(1))
		timeOut(implement)
		review := newOrder("review-1-seat-1", core.StageReview)
		lease := engineCostLiveLease(index, now)
		claim(review, 0, lease, engineCostShortRenewals)
	case engineCostReviewRounds:
		implement := newOrder("implement-1", core.StageImplement)
		claim(implement, 0, past(1), engineCostShortRenewals)
		submit(implement, head(1))
		first := newOrder("review-1-seat-1", core.StageReview)
		claim(first, 0, past(2), engineCostShortRenewals)
		verdict(first, 1, "changes_requested")
		timeOut(implement, first)
		fix := newOrder("implement-2", core.StageImplement)
		claim(fix, 0, past(3), engineCostShortRenewals)
		submit(fix, head(2))
		second := newOrder("review-2-seat-1", core.StageReview)
		claim(second, 0, past(4), engineCostShortRenewals)
		verdict(second, 2, "approve")
		timeOut(fix, second)
	case engineCostPlanGate:
		spec := newOrder("spec-1", core.StageSpec)
		claim(spec, 0, past(1), engineCostShortRenewals)
		submit(spec, "")
		timeOut(spec)
	case engineCostQueued:
		appendEvent("", "pipeline.bounced", map[string]any{"source": "operator", "feedback": "Start again."})
		newOrder("implement-1", core.StageImplement)
	}
	return nil
}

// checkEngineCostStorage proves what the fixture stored: historical claim
// and submission events with their snapshot, which the compact read leaves
// out, and the long renewal history on the verify job of the task the
// fixture names, and renewals on the review job of a task in review.
func checkEngineCostStorage(t *testing.T, twin *engineCostTwin) {
	t.Helper()
	ctx, st := twin.ctx, twin.st
	sample := engineCostTaskID(engineCostMergeFailed)
	full, err := st.ListEvents(ctx, sample)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := st.ListCompactEvents(ctx, sample)
	if err != nil {
		t.Fatal(err)
	}
	compactByID := map[int64]core.Event{}
	for _, event := range compact {
		compactByID[event.ID] = event
	}
	// The sample task's history holds two implement claims, each with its
	// progress updates, and one submission, all written with the snapshot.
	// Every one of them must store it.
	want := map[string]int{"work_order.claimed": 2, "work_order.updated": 2*engineCostProgressUpdates + 1}
	stored := map[string]int{}
	for _, event := range full {
		if event.Kind != "work_order.claimed" && event.Kind != "work_order.updated" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &fields); err != nil {
			t.Fatalf("%s %s payload: %v", twin.label, event.Kind, err)
		}
		snapshot, ok := fields["governance_snapshot"]
		if !ok {
			if event.Kind == "work_order.claimed" {
				t.Fatalf("%s claim event %d stores no governance snapshot", twin.label, event.ID)
			}
			continue
		}
		stored[event.Kind]++
		if twin.heavy && len(snapshot) < engineCostHeavySnapshotBytes {
			t.Fatalf("heavy %s event %d stores a %d-byte snapshot, want at least %d", event.Kind, event.ID, len(snapshot), engineCostHeavySnapshotBytes)
		}
		if !twin.heavy && (len(snapshot) < engineCostLightSnapshotBytes || len(snapshot) >= 4<<10) {
			t.Fatalf("light %s event %d stores a %d-byte snapshot, want a small one", event.Kind, event.ID, len(snapshot))
		}
		read, ok := compactByID[event.ID]
		if !ok {
			t.Fatalf("%s compact read has no %s event %d", twin.label, event.Kind, event.ID)
		}
		if strings.Contains(string(read.Payload), `"governance_snapshot"`) {
			t.Fatalf("%s compact read of %s event %d keeps the snapshot", twin.label, event.Kind, event.ID)
		}
	}
	for kind, count := range want {
		if stored[kind] != count {
			t.Fatalf("%s fixture stores %d %s events with a governance snapshot on %s, want %d", twin.label, stored[kind], kind, sample, count)
		}
	}

	renewals := func(taskID, jobID string) int {
		t.Helper()
		events, err := st.ListCompactEvents(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events {
			if event.Kind == "work_order.lease_renewed" && event.JobID == jobID {
				count++
			}
		}
		return count
	}
	longTask, offPage := engineCostTaskID(engineCostLongTask), engineCostOffPageTask
	longCount, offCount := renewals(longTask, longTask+"-verify-1"), renewals(offPage, offPage+"-verify-1")
	wantLong, wantOff := engineCostShortRenewals, engineCostShortRenewals+engineCostLongRenewals
	if twin.heavy {
		wantLong, wantOff = wantOff, wantLong
	}
	if longCount != wantLong || offCount != wantOff {
		t.Fatalf("%s fixture verify renewals: page task %d, off-page task %d; want %d and %d", twin.label, longCount, offCount, wantLong, wantOff)
	}
	if count := renewals(longTask, longTask+"-review-1-seat-1"); count != engineCostShortRenewals {
		t.Fatalf("%s fixture review job renewals = %d, want %d", twin.label, count, engineCostShortRenewals)
	}
}

// checkEngineCostRead reads the page once and proves the result: one marker
// per page task and no other, the open review orders, the review lease
// horizon, each marker equal to the folds over the task's complete event
// list, and the marker cases the groups seed.
func checkEngineCostRead(t *testing.T, twin *engineCostTwin, page []string, now time.Time) store.ActivityInputs {
	t.Helper()
	ctx, st := twin.ctx, twin.st
	inputs, err := st.ReadActivityInputs(ctx, page, now)
	if err != nil {
		t.Fatalf("%s read: %v", twin.label, err)
	}
	if len(inputs.Markers) != len(page) {
		t.Fatalf("%s read returned %d markers, want %d", twin.label, len(inputs.Markers), len(page))
	}
	byTask := map[string]store.ActivityMarker{}
	for _, marker := range inputs.Markers {
		if _, seen := byTask[marker.TaskID]; seen || !slices.Contains(page, marker.TaskID) {
			t.Fatalf("%s read returned task %s twice or outside the page", twin.label, marker.TaskID)
		}
		byTask[marker.TaskID] = marker
	}
	// The read returns exactly the page's orders, each in the state the
	// fixture left it.
	orders := map[string]core.WorkOrderState{}
	for _, order := range inputs.Orders {
		if _, seen := orders[order.ID]; seen {
			t.Fatalf("%s read returned order %s twice", twin.label, order.ID)
		}
		orders[order.ID] = order.State
	}
	want := engineCostExpectedOrders(page)
	var mismatches []string
	for id, state := range want {
		if got, ok := orders[id]; !ok || got != state {
			mismatches = append(mismatches, fmt.Sprintf("%s is %q, want %q", id, got, state))
		}
	}
	for id, state := range orders {
		if _, ok := want[id]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s (%s) is not a page order", id, state))
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		t.Fatalf("%s read orders differ from the fixture: %s", twin.label, strings.Join(mismatches, "; "))
	}
	if want := engineCostLiveLease(engineCostReviewLive, now); !inputs.ValidUntil.Equal(want) {
		t.Fatalf("%s read review lease horizon = %s, want the earliest live review lease %s", twin.label, inputs.ValidUntil, want)
	}

	for _, taskID := range page {
		got := byTask[taskID]
		want, last := engineCostReference(t, ctx, st, taskID, now)
		label := twin.label + " marker " + taskID
		if got.LastEventID != last.ID || !got.LastEventAt.Equal(last.At) {
			t.Fatalf("%s newest event = %d at %s, want the stored newest event %d at %s", label, got.LastEventID, got.LastEventAt, last.ID, last.At)
		}
		requireEngineCostJSON(t, label+" forge failure", got.ForgeFailure, want.ForgeFailure)
		requireEngineCostJSON(t, label+" review diagnostics", engineCostUTC(got.ReviewDiagnostics), engineCostUTC(want.ReviewDiagnostics))
		requireEngineCostJSON(t, label+" review recovery", got.ReviewRecovery, want.ReviewRecovery)
		requireEngineCostJSON(t, label+" interrupted review recovery", got.InterruptedReviewRecovery, want.InterruptedReviewRecovery)
		requireEngineCostJSON(t, label+" stalled", got.Stalled, want.Stalled)
		if got.UserChangesRequested != want.UserChangesRequested {
			t.Fatalf("%s user changes requested = %t, want %t", label, got.UserChangesRequested, want.UserChangesRequested)
		}
	}
	// The groups must reach the folds, not compare empty markers.
	for index, taskID := range page {
		marker := byTask[taskID]
		switch index % 10 {
		case engineCostMergeFailed:
			if marker.ForgeFailure == nil || marker.ForgeFailure.Category != "merge_conflict" {
				t.Fatalf("%s marker %s forge failure = %+v, want the merge failure of its submitted head", twin.label, taskID, marker.ForgeFailure)
			}
		case engineCostResubmitted:
			if marker.ForgeFailure != nil {
				t.Fatalf("%s marker %s forge failure = %+v, want none after the later submission", twin.label, taskID, marker.ForgeFailure)
			}
		case engineCostRequestAnswered:
			if marker.UserChangesRequested {
				t.Fatalf("%s marker %s: the implement claims after the change request did not clear it", twin.label, taskID)
			}
		case engineCostRequestPending:
			if !marker.UserChangesRequested {
				t.Fatalf("%s marker %s: the change request after the claims is not pending", twin.label, taskID)
			}
		case engineCostReviewLapsed:
			if len(marker.ReviewDiagnostics) != 1 || marker.ReviewDiagnostics[0].Status != store.ReviewExpiredWithoutVerdict {
				t.Fatalf("%s marker %s review diagnostics = %+v, want one expired claim", twin.label, taskID, marker.ReviewDiagnostics)
			}
		case engineCostReviewLive:
			if len(marker.ReviewDiagnostics) != 0 {
				t.Fatalf("%s marker %s review diagnostics = %+v, want none while the review lease runs", twin.label, taskID, marker.ReviewDiagnostics)
			}
		}
	}
	return inputs
}

// engineCostExpectedOrders is the order selection the fixture leaves for the
// page: each order's ID and state at the read's instant, from the groups
// seedEngineCostTask writes. Creating a second implement order cancels the
// first.
func engineCostExpectedOrders(page []string) map[string]core.WorkOrderState {
	ended, open, replaced := core.WorkOrderTimedOut, core.WorkOrderQueued, core.WorkOrderCancelled
	groups := map[int]map[string]core.WorkOrderState{
		engineCostMerged:          {"spec-1": ended, "implement-1": ended, "review-1-seat-1": ended},
		engineCostMergeFailed:     {"implement-1": ended},
		engineCostResubmitted:     {"implement-1": replaced, "implement-2": ended},
		engineCostRequestAnswered: {"implement-1": ended},
		engineCostRequestPending:  {"implement-1": ended},
		engineCostReviewLapsed:    {"implement-1": ended, "verify-1": ended, "review-1-seat-1": open},
		engineCostReviewLive:      {"implement-1": ended, "review-1-seat-1": open},
		engineCostReviewRounds:    {"implement-1": replaced, "review-1-seat-1": ended, "implement-2": ended, "review-2-seat-1": ended},
		engineCostPlanGate:        {"spec-1": ended},
		engineCostQueued:          {"implement-1": open},
	}
	out := map[string]core.WorkOrderState{}
	for index, taskID := range page {
		for suffix, state := range groups[index%10] {
			out[taskID+"-"+suffix] = state
		}
	}
	return out
}

// engineCostReference folds one task's markers from its complete compact
// event list, as the in-memory reference backend does, and returns the
// task's newest stored event by (at, id). The folds decode no snapshot
// body, so the compact list folds as the stored one does.
func engineCostReference(t *testing.T, ctx context.Context, st store.Store, taskID string, now time.Time) (store.ActivityMarker, core.Event) {
	t.Helper()
	orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := st.ListDependencyBlockers(ctx, []string{taskID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ListCompactEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for index := range orders {
		if orders[index].Stage != core.StageImplement && orders[index].Stage != core.StageVerify {
			continue
		}
		blocked := blockers[orders[index].TaskID]
		orders[index].BlockingTaskIDs = append([]string(nil), blocked.BlockingTaskIDs...)
		orders[index].UnsatisfiableTaskIDs = append([]string(nil), blocked.UnsatisfiableTaskIDs...)
		if len(orders[index].BlockingTaskIDs) > 0 {
			orders[index].Claimable = false
		}
	}
	if len(events) == 0 {
		t.Fatalf("task %s stores no event", taskID)
	}
	last := events[0]
	for _, event := range events[1:] {
		if event.At.After(last.At) || (event.At.Equal(last.At) && event.ID > last.ID) {
			last = event
		}
	}
	return store.ActivityMarker{
		TaskID:                    taskID,
		ForgeFailure:              store.LatestForgeFailure(events),
		ReviewDiagnostics:         store.ReviewVerdictDiagnostics(orders, events, now),
		ReviewRecovery:            store.ReviewRecoveryNeeded(orders, events),
		InterruptedReviewRecovery: store.InterruptedReviewRecoveryNeeded(task, store.CurrentReviewOrders(orders, events), events),
		Stalled:                   store.StalledTask(orders),
		UserChangesRequested:      store.UserRequestChangesPending(events),
	}, last
}

// requireEngineCostTwinsAgree proves both fixtures read the same page: the
// same markers apart from the newest event's ID, which each store assigns
// and checkEngineCostRead checks against the fixture's own history,
// the same orders by identity, state, and lease, and the same review lease
// horizon.
func requireEngineCostTwinsAgree(t *testing.T, heavy, light store.ActivityInputs) {
	t.Helper()
	markers := func(inputs store.ActivityInputs) []store.ActivityMarker {
		out := slices.Clone(inputs.Markers)
		for i := range out {
			out[i].LastEventID = 0
			out[i].LastEventAt = out[i].LastEventAt.UTC()
			out[i].ReviewDiagnostics = engineCostUTC(out[i].ReviewDiagnostics)
			if out[i].ForgeFailure != nil {
				failure := *out[i].ForgeFailure
				failure.At = failure.At.UTC()
				out[i].ForgeFailure = &failure
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
		return out
	}
	type order struct {
		ID, TaskID, JobID string
		Stage             core.Stage
		State             core.WorkOrderState
		Lease             time.Time
	}
	orders := func(inputs store.ActivityInputs) []order {
		out := make([]order, 0, len(inputs.Orders))
		for _, o := range inputs.Orders {
			out = append(out, order{ID: o.ID, TaskID: o.TaskID, JobID: o.JobID, Stage: o.Stage, State: o.State, Lease: o.LeaseExpiresAt.UTC()})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	requireEngineCostJSON(t, "heavy and light markers", markers(heavy), markers(light))
	requireEngineCostJSON(t, "heavy and light orders", orders(heavy), orders(light))
	if !heavy.ValidUntil.Equal(light.ValidUntil) {
		t.Fatalf("heavy review lease horizon %s, light %s", heavy.ValidUntil, light.ValidUntil)
	}
}

func engineCostUTC(diagnostics []store.ReviewVerdictDiagnostic) []store.ReviewVerdictDiagnostic {
	out := slices.Clone(diagnostics)
	for i := range out {
		out[i].ClaimedAt = out[i].ClaimedAt.UTC()
		out[i].LeaseExpiresAt = out[i].LeaseExpiresAt.UTC()
	}
	return out
}

// requireEngineCostJSON compares two values by their JSON encoding.
func requireEngineCostJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s = %s, want %s", label, gotJSON, wantJSON)
	}
}

// settleEngineCostSingleStore moves the fixture's events out of the
// columnstore's in-memory segment into column segments, as SingleStore's
// background flusher does with a live workspace's history. A recent write
// waits in the in-memory segment, where a payload is stored as text and a
// text test on it is cheap; a production history is in column segments,
// where SingleStore builds the whole document for a text test. Both fixtures
// are flushed, so both are read in the same layout. The statement moves
// rows between segments and writes no row.
func settleEngineCostSingleStore(t *testing.T, ctx context.Context, raw, name string) {
	t.Helper()
	cfg, err := taskPollSingleStoreConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	if _, err := db.ExecContext(ctx, "OPTIMIZE TABLE events_columnstore FLUSH"); err != nil {
		t.Fatalf("flush the %s events: %v", name, err)
	}
}
