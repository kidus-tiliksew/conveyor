package storetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunReviewPolicyStatusConformance proves on every backend that an enforcing
// task holds at most one conveyor/review-policy row per head; that a queue
// writes the row, its event, and the head's review_policy_status job together
// and writes nothing when the row already holds the state; that an update
// follows the row lifecycle and refuses a stale revision; that off and record
// tasks queue nothing; that concurrent queues for one head record one write
// per change; and that a merge bypass commits with the completion it explains,
// once per task and head, crediting no approval, and rolls back with it
// (req-risk-based-review AC-10.1, AC-10.3; component-git-delivery invariant
// 37; component-submission-merge invariant 66).
func RunReviewPolicyStatusConformance(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	fileTask := func(t *testing.T, mode riskpolicy.Mode, gate bool) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
			State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "policy-head", PolicyVersion: 1, MergeApproval: true, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
		task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: mode, Tiers: riskpolicy.DefaultTable()}, Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}
		requireOK(t, st.CreateTask(ctx, task))
		if gate {
			out, err := taskops.New(st).Perform(ctx, id, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
			requireOK(t, err)
			if out.Task.State != core.TaskAwaiting {
				t.Fatalf("gate task=%+v", out.Task)
			}
		}
		stored, err := st.GetTask(ctx, id)
		requireOK(t, err)
		return stored
	}
	countEvents := func(t *testing.T, taskID, kind string) int {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		n := 0
		for _, event := range events {
			if event.Kind == kind {
				n++
			}
		}
		return n
	}
	activeJob := func(t *testing.T, taskID, head string) bool {
		t.Helper()
		args := queue.ReviewPolicyStatusArgs{TaskID: taskID, HeadSHA: head}
		job, err := logqueue.Load(ctx, st.Log(), x.Workspace, logqueue.StreamFor(args.Kind(), args.UniqueKey()))
		requireOK(t, err)
		return job.Active()
	}

	t.Run("QueueUpdateAndReopen", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, false)
		row, wrote, err := st.QueueReviewPolicyStatus(ctx, task.ID, "head-1", core.PolicyStatusPending)
		if err != nil || !wrote || row.State != core.ReviewPolicyStatusQueued || row.Desired != core.PolicyStatusPending || row.Revision != 1 || row.CreatedAt.IsZero() {
			t.Fatalf("first queue row=%+v wrote=%t err=%v", row, wrote, err)
		}
		if !activeJob(t, task.ID, "head-1") {
			t.Fatal("the queue enqueued no review_policy_status job")
		}
		if _, wrote, err = st.QueueReviewPolicyStatus(ctx, task.ID, "head-1", core.PolicyStatusPending); err != nil || wrote {
			t.Fatalf("replayed queue wrote=%t err=%v", wrote, err)
		}
		if n := countEvents(t, task.ID, core.ReviewPolicyStatusQueuedEvent); n != 1 {
			t.Fatalf("queued events=%d, want 1", n)
		}
		row.State, row.Attempts = core.ReviewPolicyStatusRetrying, 1
		retrying, err := st.UpdateReviewPolicyStatus(ctx, row)
		if err != nil || retrying.Revision != 2 || retrying.State != core.ReviewPolicyStatusRetrying {
			t.Fatalf("retrying=%+v err=%v", retrying, err)
		}
		// The revision guard refuses an update prepared from an older read.
		if _, err = st.UpdateReviewPolicyStatus(ctx, row); !errors.Is(err, store.ErrReviewPolicyStatusStale) {
			t.Fatalf("stale update err=%v", err)
		}
		retrying.State, retrying.Published = core.ReviewPolicyStatusPublished, core.PolicyStatusPending
		published, err := st.UpdateReviewPolicyStatus(ctx, retrying)
		if err != nil || published.State != core.ReviewPolicyStatusPublished || published.Published != core.PolicyStatusPending {
			t.Fatalf("published=%+v err=%v", published, err)
		}
		if _, wrote, err = st.QueueReviewPolicyStatus(ctx, task.ID, "head-1", core.PolicyStatusPending); err != nil || wrote {
			t.Fatalf("queue of the published state wrote=%t err=%v", wrote, err)
		}
		open, err := st.ListOpenReviewPolicyStatuses(ctx)
		requireOK(t, err)
		for _, candidate := range open {
			if candidate.TaskID == task.ID {
				t.Fatalf("a published row is listed open: %+v", candidate)
			}
		}
		// A changed state reopens the published row.
		reopened, wrote, err := st.QueueReviewPolicyStatus(ctx, task.ID, "head-1", core.PolicyStatusSuccess)
		if err != nil || !wrote || reopened.State != core.ReviewPolicyStatusQueued || reopened.Desired != core.PolicyStatusSuccess || reopened.Published != core.PolicyStatusPending || reopened.Revision != published.Revision+1 {
			t.Fatalf("reopened=%+v wrote=%t err=%v", reopened, wrote, err)
		}
		open, err = st.ListOpenReviewPolicyStatuses(ctx)
		requireOK(t, err)
		found := false
		for _, candidate := range open {
			found = found || (candidate.TaskID == task.ID && candidate.HeadSHA == "head-1")
		}
		if !found {
			t.Fatalf("the reopened row is not listed open: %+v", open)
		}
		// A published row names the state GitHub accepted, and a desired
		// state is pending or success; a refused update changes nothing.
		bad := reopened
		bad.State, bad.Published = core.ReviewPolicyStatusPublished, ""
		if _, err = st.UpdateReviewPolicyStatus(ctx, bad); err == nil {
			t.Fatal("a published row without a published state was accepted")
		}
		bad = reopened
		bad.State, bad.Desired = core.ReviewPolicyStatusRetrying, "failure"
		if _, err = st.UpdateReviewPolicyStatus(ctx, bad); err == nil {
			t.Fatal("a desired state other than pending or success was accepted")
		}
		if got, _, _ := st.GetReviewPolicyStatus(ctx, task.ID, "head-1"); got.Revision != reopened.Revision {
			t.Fatalf("a refused update changed the row: %+v", got)
		}
		if _, wrote, err = st.QueueReviewPolicyStatus(ctx, task.ID, "head-2", core.PolicyStatusPending); err != nil || !wrote {
			t.Fatalf("second head wrote=%t err=%v", wrote, err)
		}
		rows, err := st.ListReviewPolicyStatuses(ctx, task.ID)
		requireOK(t, err)
		if len(rows) != 2 || rows[0].HeadSHA != "head-1" || rows[1].HeadSHA != "head-2" {
			t.Fatalf("rows=%+v", rows)
		}
		got, ok, err := st.GetReviewPolicyStatus(ctx, task.ID, "head-1")
		if err != nil || !ok || got.Revision != reopened.Revision || got.Desired != core.PolicyStatusSuccess {
			t.Fatalf("get=%+v ok=%t err=%v", got, ok, err)
		}
		if _, ok, err = st.GetReviewPolicyStatus(ctx, task.ID, "head-unknown"); err != nil || ok {
			t.Fatalf("unknown head ok=%t err=%v", ok, err)
		}
		if n := countEvents(t, task.ID, core.ReviewPolicyStatusPublishedEvent); n != 1 {
			t.Fatalf("published events=%d, want 1", n)
		}
	})

	t.Run("RefusesOffRecordAndBadInput", func(t *testing.T) {
		for _, mode := range []riskpolicy.Mode{riskpolicy.ModeOff, riskpolicy.ModeRecord} {
			task := fileTask(t, mode, false)
			if _, wrote, err := st.QueueReviewPolicyStatus(ctx, task.ID, "head-1", core.PolicyStatusPending); !errors.Is(err, store.ErrRefusal) || wrote {
				t.Fatalf("%s task queue wrote=%t err=%v", mode, wrote, err)
			}
			rows, err := st.ListReviewPolicyStatuses(ctx, task.ID)
			if err != nil || len(rows) != 0 || countEvents(t, task.ID, core.ReviewPolicyStatusQueuedEvent) != 0 {
				t.Fatalf("%s task rows=%+v err=%v", mode, rows, err)
			}
		}
		task := fileTask(t, riskpolicy.ModeEnforce, false)
		for _, bad := range []struct{ head, desired string }{{"", core.PolicyStatusPending}, {"  ", core.PolicyStatusSuccess}, {"head-1", "failure"}, {"head-1", ""}} {
			if _, wrote, err := st.QueueReviewPolicyStatus(ctx, task.ID, bad.head, bad.desired); !errors.Is(err, store.ErrRefusal) || wrote {
				t.Fatalf("queue %+v wrote=%t err=%v", bad, wrote, err)
			}
		}
		if _, _, err := st.QueueReviewPolicyStatus(ctx, "no-such-task", "head-1", core.PolicyStatusPending); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown task err=%v", err)
		}
		if _, err := st.UpdateReviewPolicyStatus(ctx, core.ReviewPolicyStatus{TaskID: task.ID, HeadSHA: "head-missing", Desired: core.PolicyStatusPending, State: core.ReviewPolicyStatusRetrying, Revision: 1}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("update of a missing row err=%v", err)
		}
		if rows, err := st.ListReviewPolicyStatuses(ctx, task.ID); err != nil || len(rows) != 0 {
			t.Fatalf("refused input wrote rows=%+v err=%v", rows, err)
		}
	})

	// Concurrent queues for one head serialize under the task lock: in each
	// round every writer asks for the same changed state, exactly one writes,
	// and the revisions and events count one write per round.
	t.Run("ConcurrentQueuesSerialize", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, false)
		const rounds, writers = 12, 8
		for round := 1; round <= rounds; round++ {
			desired := core.PolicyStatusPending
			if round%2 == 0 {
				desired = core.PolicyStatusSuccess
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			wrote, failures := 0, []error{}
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, ok, err := st.QueueReviewPolicyStatus(ctx, task.ID, "head-c", desired)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						failures = append(failures, err)
					}
					if ok {
						wrote++
					}
				}()
			}
			wg.Wait()
			if len(failures) > 0 || wrote != 1 {
				t.Fatalf("round %d wrote=%d failures=%v", round, wrote, failures)
			}
		}
		row, ok, err := st.GetReviewPolicyStatus(ctx, task.ID, "head-c")
		if err != nil || !ok || row.Revision != rounds || row.Desired != core.PolicyStatusSuccess {
			t.Fatalf("row=%+v ok=%t err=%v, want revision %d", row, ok, err, rounds)
		}
		if n := countEvents(t, task.ID, core.ReviewPolicyStatusQueuedEvent); n != rounds {
			t.Fatalf("queued events=%d, want %d", n, rounds)
		}
		if rows, err := st.ListReviewPolicyStatuses(ctx, task.ID); err != nil || len(rows) != 1 {
			t.Fatalf("rows=%+v err=%v", rows, err)
		}
	})

	// A raise can leave a head short of the success it published, so the
	// raise's own transaction reopens every published or failed row with a
	// job; a queued row keeps its job (component-git-delivery invariant 37).
	t.Run("RaiseReopensPublishedRows", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, false)
		published, _, err := st.QueueReviewPolicyStatus(ctx, task.ID, "head-p", core.PolicyStatusSuccess)
		requireOK(t, err)
		published.State, published.Published = core.ReviewPolicyStatusPublished, core.PolicyStatusSuccess
		published, err = st.UpdateReviewPolicyStatus(ctx, published)
		requireOK(t, err)
		queued, _, err := st.QueueReviewPolicyStatus(ctx, task.ID, "head-q", core.PolicyStatusPending)
		requireOK(t, err)
		before := countEvents(t, task.ID, core.ReviewPolicyStatusQueuedEvent)
		if _, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierHigh, Reason: "touches billing", ActorID: "user:operator"}, store.RiskSeatSettings{}); err != nil {
			t.Fatal(err)
		}
		reopened, ok, err := st.GetReviewPolicyStatus(ctx, task.ID, "head-p")
		if err != nil || !ok || reopened.State != core.ReviewPolicyStatusQueued || reopened.Revision != published.Revision+1 || reopened.Published != core.PolicyStatusSuccess {
			t.Fatalf("reopened=%+v ok=%t err=%v", reopened, ok, err)
		}
		if !activeJob(t, task.ID, "head-p") {
			t.Fatal("the raise enqueued no job for the reopened head")
		}
		if kept, _, _ := st.GetReviewPolicyStatus(ctx, task.ID, "head-q"); kept.Revision != queued.Revision {
			t.Fatalf("the raise rewrote a queued row: %+v", kept)
		}
		if n := countEvents(t, task.ID, core.ReviewPolicyStatusQueuedEvent); n != before+1 {
			t.Fatalf("queued events=%d, want %d", n, before+1)
		}
		// A refused raise reopens nothing.
		if _, err := st.RaiseTaskRiskTier(ctx, core.RiskRating{TaskID: task.ID, Tier: riskpolicy.TierMedium, Reason: "lower", ActorID: "user:operator"}, store.RiskSeatSettings{}); err == nil {
			t.Fatal("a lower raise was accepted")
		}
		if n := countEvents(t, task.ID, core.ReviewPolicyStatusQueuedEvent); n != before+1 {
			t.Fatalf("a refused raise appended queued events: %d", n)
		}
	})

	bypass := core.MergeBypass{ForgeActor: "octo-merger", HeadSHA: "policy-head", MergeCommitSHA: "landed", RequiredHumanApprovals: 1, MissingHumanApprovals: 1, RequiredAgentReviews: 1, RecordedAgentReviews: 1}

	t.Run("BypassCommitsWithTheCompletion", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, true)
		record := bypass
		out, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskMergeRecover, ProjectStages: true, Bypass: &record})
		if err != nil || out.Task.State != core.TaskMerged {
			t.Fatalf("bypass completion task=%+v err=%v", out.Task, err)
		}
		if n := countEvents(t, task.ID, core.MergeBypassRecordedEvent); n != 1 {
			t.Fatalf("bypass events=%d, want 1", n)
		}
		approvals, err := st.ListTaskApprovals(ctx, task.ID)
		if err != nil || len(approvals) != 0 {
			t.Fatalf("a bypass credited approvals=%+v err=%v", approvals, err)
		}
		// A replayed completion is refused by the merged task and appends no
		// second bypass.
		if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskMergeRecover, ProjectStages: true, Bypass: &record}); err == nil {
			t.Fatal("a second completion of a merged task was accepted")
		}
		if n := countEvents(t, task.ID, core.MergeBypassRecordedEvent); n != 1 {
			t.Fatalf("bypass events after replay=%d, want 1", n)
		}
	})

	t.Run("RefusedBypassRollsBackTheCompletion", func(t *testing.T) {
		final := func(taskID string) *core.TaskApproval {
			return &core.TaskApproval{TaskID: taskID, Kind: core.TaskApprovalPerson, HeadSHA: "policy-head", ReviewRound: 1, UserID: "usr-final", Source: "merge"}
		}
		empty := bypass
		empty.MissingHumanApprovals, empty.MissingAgentReviews = 0, 0
		noActor := bypass
		noActor.ForgeActor = " "
		for name, tc := range map[string]struct {
			mode    riskpolicy.Mode
			command core.TaskCommand
			bypass  core.MergeBypass
			final   bool
		}{
			"RecordTask":       {riskpolicy.ModeRecord, core.TaskMergeRecover, bypass, false},
			"OffTask":          {riskpolicy.ModeOff, core.TaskMergeRecover, bypass, false},
			"WithApproval":     {riskpolicy.ModeEnforce, core.TaskMergeRecover, bypass, true},
			"NothingMissing":   {riskpolicy.ModeEnforce, core.TaskMergeRecover, empty, false},
			"NoForgeActor":     {riskpolicy.ModeEnforce, core.TaskMergeRecover, noActor, false},
			"NotACompletion":   {riskpolicy.ModeEnforce, core.TaskInterventionApproveReview, bypass, false},
			"InterventionPath": {riskpolicy.ModeEnforce, core.TaskInterventionMerge, bypass, false},
		} {
			t.Run(name, func(t *testing.T) {
				task := fileTask(t, tc.mode, true)
				record := tc.bypass
				command := taskops.Command{Kind: tc.command, ProjectStages: true, Bypass: &record}
				if tc.final {
					command.FinalApproval = final(task.ID)
				}
				if _, err := taskops.New(st).Perform(ctx, task.ID, command); err == nil {
					t.Fatalf("command %s with bypass %+v was accepted", tc.command, tc.bypass)
				}
				current, err := st.GetTask(ctx, task.ID)
				requireOK(t, err)
				if current.State != core.TaskAwaiting || countEvents(t, task.ID, core.MergeBypassRecordedEvent) != 0 {
					t.Fatalf("a refused bypass left task=%s bypasses=%d", current.State, countEvents(t, task.ID, core.MergeBypassRecordedEvent))
				}
				if approvals, err := st.ListTaskApprovals(ctx, task.ID); err != nil || len(approvals) != 0 {
					t.Fatalf("a refused bypass recorded approvals=%+v err=%v", approvals, err)
				}
			})
		}
	})

	// Concurrent observed-merge completions record one bypass per task and
	// head: one completion wins and every other is refused.
	t.Run("ConcurrentCompletionsRecordOneBypass", func(t *testing.T) {
		task := fileTask(t, riskpolicy.ModeEnforce, true)
		const writers = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		succeeded := 0
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				record := bypass
				record.ForgeActor = fmt.Sprintf("observer-%d", i)
				if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskMergeRecover, ProjectStages: true, Bypass: &record}); err == nil {
					mu.Lock()
					succeeded++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		current, err := st.GetTask(ctx, task.ID)
		requireOK(t, err)
		if succeeded != 1 || current.State != core.TaskMerged || countEvents(t, task.ID, core.MergeBypassRecordedEvent) != 1 {
			t.Fatalf("succeeded=%d state=%s bypasses=%d", succeeded, current.State, countEvents(t, task.ID, core.MergeBypassRecordedEvent))
		}
	})
}
