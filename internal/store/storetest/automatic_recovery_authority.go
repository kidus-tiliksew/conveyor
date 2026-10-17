package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runAutomaticRecoveryAuthority drives answered authority-conflict recovery
// on every backend (feature-bounded-automatic-recovery AR-3a; DEC-73;
// req-260820-394cac AC-2.6, AC-2.7; req-bounded-automatic-recovery AC-2.2).
func runAutomaticRecoveryAuthority(t *testing.T, x Fixture) {
	f := authorityFixture{t: t, x: x}
	t.Run("AnswersAfterLastConfirmation", f.answersAfterLastConfirmation)
	t.Run("OperatorRace", f.operatorRace)
	t.Run("StaysReleased", f.staysReleased)
}

type authorityFixture struct {
	t *testing.T
	x Fixture
}

type authorityDocument struct {
	kind string
	id   string
}

func (f authorityFixture) document(t *testing.T, kind string) authorityDocument {
	t.Helper()
	st, ctx := f.x.Backend, f.x.Context
	suffix := core.NewTaskID()
	if kind == "requirement" {
		version := authorityRequirementVersion("The initial clause.")
		version.Origin = core.RequirementOriginOperator
		requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-authority-" + suffix, Title: "Authority " + suffix}, version)
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version)
		requireOK(t, err)
		return authorityDocument{kind: kind, id: requirement.ID}
	}
	design, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-authority-" + suffix, Title: "Authority " + suffix, Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial " + suffix), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, initial.Version)
	requireOK(t, err)
	return authorityDocument{kind: "system_design", id: design.ID}
}

func authorityRequirementVersion(statement string) core.RequirementVersion {
	return core.RequirementVersion{Content: "# Authority\n\n```conveyor:requirements\n- id: REQ-1\n  statement: " + statement + "\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}}
}

// propose pushes a pending version from the named origin: a task, a planning
// session, or the operator over REST (req-260820-394cac AC-4.2).
func (f authorityFixture) propose(t *testing.T, document authorityDocument, origin, taskID string) int {
	t.Helper()
	st, ctx := f.x.Backend, f.x.Context
	label := core.NewTaskID()
	if document.kind == "requirement" {
		version := authorityRequirementVersion("A revised clause " + label + ".")
		version.RequirementID = document.id
		switch origin {
		case "task":
			version.Origin, version.OriginTaskID = core.RequirementOriginImplementation, taskID
		case "session":
			version.Origin, version.OriginSessionID = core.RequirementOriginChat, "session-"+label
		default:
			version.Origin = core.RequirementOriginOperator
		}
		proposed, err := st.ProposeRequirementVersion(ctx, version)
		requireOK(t, err)
		return proposed.Version
	}
	version := core.SystemDesignVersion{DocumentID: document.id, Content: designContent("revision " + label)}
	switch origin {
	case "task":
		version.Origin, version.OriginTaskID = core.SystemDesignOriginImplementation, taskID
	case "session":
		version.Origin, version.OriginSessionID = core.SystemDesignOriginPlanning, "session-"+label
	default:
		version.Origin = core.SystemDesignOriginOperator
	}
	proposed, err := st.ProposeSystemDesignVersion(ctx, version)
	requireOK(t, err)
	return proposed.Version
}

func (f authorityFixture) confirm(t *testing.T, document authorityDocument, version int) {
	t.Helper()
	st, ctx := f.x.Backend, f.x.Context
	var err error
	if document.kind == "requirement" {
		_, _, err = st.ConfirmRequirementVersion(ctx, document.id, version)
	} else {
		_, _, err = st.ConfirmSystemDesignVersion(ctx, document.id, version)
	}
	requireOK(t, err)
}

func (f authorityFixture) dismiss(t *testing.T, document authorityDocument, version int) {
	t.Helper()
	st, ctx := f.x.Backend, f.x.Context
	var err error
	if document.kind == "requirement" {
		_, _, err = st.DismissRequirementVersion(ctx, document.id, version)
	} else {
		_, _, err = st.DismissSystemDesignVersion(ctx, document.id, version)
	}
	requireOK(t, err)
}

// task creates a running task with one implement order.
func (f authorityFixture) task(t *testing.T, stage core.Stage) (core.Task, string) {
	t.Helper()
	st, ctx := f.x.Backend, f.x.Context
	now := time.Now().UTC()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: f.x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, CreatedAt: now}
	if stage == core.StageVerify {
		task.ReviewedHeadSHA = "submitted-head"
		task.SetupContract.VerifyStage = true
	}
	requireOK(t, st.CreateTask(ctx, task))
	orderID := id + "-" + string(stage) + "-1"
	job := core.Job{ID: orderID, TaskID: id, Stage: stage, State: core.JobPending}
	if stage == core.StageVerify {
		_, err := CreateStageWorkOrder(ctx, st, job, core.WorkOrder{ID: orderID, JobID: orderID, TaskID: id, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
		requireOK(t, err)
		return task, orderID
	}
	requireOK(t, st.CreateJob(ctx, job))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: id, JobID: orderID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}))
	return task, orderID
}

// release claims the order under a fresh session and releases it at an
// operator checkpoint, returning the released attempt.
func (f authorityFixture) release(t *testing.T, orderID string, checkpoint *core.WorkOrderCheckpoint) core.WorkOrder {
	t.Helper()
	st, ctx := f.x.Backend, f.x.Context
	session := "session-" + core.NewTaskID()
	const worker = "worker-authority"
	_, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: worker, ClaimantID: worker, SessionID: session, ClientToken: session + "-token", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	released, err := ReleaseWorkerClaim(ctx, st, orderID, worker, core.WorkOrderRelease{SessionID: session, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Checkpoint: checkpoint, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
	if released.LastAttemptOutcome != core.WorkOrderOutcomeReleased || !released.RetrySuppressed {
		t.Fatalf("released order=%+v", released)
	}
	return released
}

func authorityCheckpoint(citations ...core.WorkOrderAuthorityConflictCitation) *core.WorkOrderCheckpoint {
	return &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited revisions, then recover.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: citations}
}

func cite(document authorityDocument, version int, clause string) core.WorkOrderAuthorityConflictCitation {
	return core.WorkOrderAuthorityConflictCitation{DocumentID: document.id, CitedVersion: version, StatementOrSectionID: clause}
}

func (f authorityFixture) tick(t *testing.T) {
	t.Helper()
	_, err := taskops.New(f.x.Backend).TickOrderClock(f.x.Context, time.Now().UTC())
	requireOK(t, err)
}

type authorityRecoveredEvent struct {
	event   core.Event
	payload struct {
		Reason           string                            `json:"reason"`
		Automatic        bool                              `json:"automatic"`
		RequestID        string                            `json:"request_id"`
		Direction        string                            `json:"direction"`
		Command          string                            `json:"command"`
		PriorState       string                            `json:"prior_state"`
		NewState         string                            `json:"new_state"`
		ReleaseAttemptID string                            `json:"release_attempt_id"`
		ReleaseEventID   int64                             `json:"release_event_id"`
		RecoveryNumber   *int                              `json:"recovery_number"`
		Citations        []store.AnsweredAuthorityCitation `json:"citations"`
	}
}

// recoveries lists every recovery of the task's orders, operator or system.
func (f authorityFixture) recoveries(t *testing.T, taskID string) (authority []authorityRecoveredEvent, operator []core.Event, releases map[string]int64) {
	t.Helper()
	events, err := f.x.Backend.ListEvents(f.x.Context, taskID)
	requireOK(t, err)
	releases = map[string]int64{}
	for _, event := range events {
		switch event.Kind {
		case "work_order.released":
			releases[store.ReleaseEventAttemptID(event)] = event.ID
		case "work_order.recovered", "work_order.redispatched":
			var recovered authorityRecoveredEvent
			recovered.event = event
			requireOK(t, json.Unmarshal(event.Payload, &recovered.payload))
			if recovered.payload.Reason == store.AuthorityConfirmedRecoveryReason {
				authority = append(authority, recovered)
			} else {
				operator = append(operator, event)
			}
		}
	}
	return authority, operator, releases
}

func (f authorityFixture) requireReleased(t *testing.T, taskID, orderID string) {
	t.Helper()
	order, err := f.x.Backend.GetWorkOrder(f.x.Context, orderID)
	requireOK(t, err)
	if order.LastAttemptOutcome != core.WorkOrderOutcomeReleased || !order.RetrySuppressed || order.Checkpoint == nil {
		t.Fatalf("order left the release: %+v", order)
	}
	if authority, operator, _ := f.recoveries(t, taskID); len(authority) != 0 || len(operator) != 0 {
		t.Fatalf("released order was recovered: authority=%d operator=%d", len(authority), len(operator))
	}
}

func (f authorityFixture) answersAfterLastConfirmation(t *testing.T) {
	st, ctx := f.x.Backend, f.x.Context
	requirement := f.document(t, "requirement")
	design := f.document(t, "system_design")
	task, orderID := f.task(t, core.StageImplement)

	// A mechanical recovery first leaves nonzero ledger values on the order.
	session := "session-ledger-" + core.NewTaskID()
	_, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: "worker-ledger", ClaimantID: "worker-ledger", SessionID: session, ClientToken: session, Lease: time.Minute, ExecutionTimeout: time.Minute})
	requireOK(t, err)
	_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(2*time.Minute))
	requireOK(t, err)
	scheduled, err := st.GetWorkOrder(ctx, orderID)
	requireOK(t, err)
	if scheduled.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("mechanical recovery not scheduled: %+v", scheduled)
	}
	_, err = taskops.New(st).TickOrderClock(ctx, scheduled.NextAutomaticRecoveryAt)
	requireOK(t, err)
	ledger, err := st.GetWorkOrder(ctx, orderID)
	requireOK(t, err)
	if ledger.AutomaticRecoveryCount != 1 || ledger.AutomaticRecoveryWindowStartedAt.IsZero() || ledger.State != core.WorkOrderQueued {
		t.Fatalf("mechanical ledger=%+v", ledger)
	}
	_, mechanical, _ := f.recoveries(t, task.ID)

	released := f.release(t, orderID, authorityCheckpoint(cite(design, 1, "Recovery"), cite(requirement, 1, "REQ-1"), cite(design, 1, "Next action")))
	requirementVersion := f.propose(t, requirement, "task", task.ID)
	designVersion := f.propose(t, design, "task", task.ID)
	f.tick(t)
	f.requireAuthorityCount(t, task.ID, 0)

	// A bound of zero disables only mechanical recovery (AC-2.6).
	restore := setAutomaticRecoveryBound(t, st, ctx, 0)
	defer restore()
	f.confirm(t, requirement, requirementVersion)
	f.tick(t)
	f.requireAuthorityCount(t, task.ID, 0)
	partial, err := st.GetWorkOrder(ctx, orderID)
	requireOK(t, err)
	if partial.LastAttemptOutcome != core.WorkOrderOutcomeReleased || partial.Checkpoint == nil {
		t.Fatalf("partial confirmation recovered: %+v", partial)
	}

	frozen, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	f.confirm(t, design, designVersion)
	f.tick(t)
	recovered, err := st.GetWorkOrder(ctx, orderID)
	requireOK(t, err)
	if recovered.State != core.WorkOrderQueued || recovered.LastAttemptOutcome != "" || recovered.RetrySuppressed || recovered.Checkpoint != nil {
		t.Fatalf("recovered order=%+v", recovered)
	}
	wantDirection := "Continue under the newly confirmed authority: " + design.id + " v2, " + requirement.id + " v2."
	if recovered.OperatorDirection != wantDirection {
		t.Fatalf("direction=%q want %q", recovered.OperatorDirection, wantDirection)
	}
	if recovered.AutomaticRecoveryCount != ledger.AutomaticRecoveryCount || !recovered.AutomaticRecoveryWindowStartedAt.Equal(ledger.AutomaticRecoveryWindowStartedAt) || !recovered.NextAutomaticRecoveryAt.Equal(ledger.NextAutomaticRecoveryAt) {
		t.Fatalf("ledger changed: before=%+v after=%+v", ledger, recovered)
	}
	if recovered.QueueEnteredAt.Before(released.QueueEnteredAt) || recovered.RedispatchCount != released.RedispatchCount+1 {
		t.Fatalf("queue re-entry=%+v released=%+v", recovered, released)
	}
	stored, err := st.GetTask(ctx, task.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(stored.SetupContract, frozen.SetupContract) || stored.PolicyVersion != frozen.PolicyVersion {
		t.Fatalf("frozen policy changed: before=%+v after=%+v", frozen.SetupContract, stored.SetupContract)
	}
	authority, operator, releases := f.recoveries(t, task.ID)
	if len(authority) != 1 || len(operator) != len(mechanical) {
		t.Fatalf("authority=%d operator=%d mechanical=%d", len(authority), len(operator), len(mechanical))
	}
	event := authority[0]
	if event.event.Kind != "work_order.recovered" || event.event.ActorRole != core.ActorSystem || event.event.ActorID != "queue:"+orderID {
		t.Fatalf("event=%+v", event.event)
	}
	payload := event.payload
	if !payload.Automatic || payload.Command != "" || payload.PriorState != "queued" || payload.NewState != "queued" || payload.Direction != wantDirection || payload.RecoveryNumber != nil {
		t.Fatalf("payload=%+v", payload)
	}
	if payload.RequestID != store.AuthorityConfirmedRequestID(orderID, released.LastAttemptID) || payload.ReleaseAttemptID != released.LastAttemptID || payload.ReleaseEventID == 0 || payload.ReleaseEventID != releases[released.LastAttemptID] {
		t.Fatalf("release identity payload=%+v releases=%v", payload, releases)
	}
	if len(payload.Citations) != 3 || payload.Citations[0] != (store.AnsweredAuthorityCitation{DocumentID: design.id, CitedVersion: 1, ConfirmedVersion: 2}) || payload.Citations[1].ConfirmedVersion != 2 {
		t.Fatalf("citations=%+v", payload.Citations)
	}

	// Repeated ticks and confirmations find nothing to do.
	f.tick(t)
	f.tick(t)
	f.requireAuthorityCount(t, task.ID, 1)
	if _, err = RecoverWorkOrderWithDirection(ctx, st, orderID, "operator-after", "late operator recovery", time.Hour); err == nil {
		t.Fatal("operator recovery accepted an order the factory already recovered")
	}
	f.requireAuthorityCount(t, task.ID, 1)

	// The next action is ordinary queued work with no recover-route choice,
	// the route refuses a further recovery with the text the projection
	// records, and the acts log lists the act.
	action, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{Now: time.Now().UTC()})
	requireOK(t, err)
	if action.Kind == nextaction.KindAgentQuestion || action.Details.Checkpoint != "" {
		t.Fatalf("next action still states the checkpoint: %+v", action)
	}
	requireResumedQueuedAction(t, st, ctx, task.ID, orderID)
	f.requireAuthorityCount(t, task.ID, 1)
	// GET /v1/inbox/acts lists system recoveries through FilterFactoryActs,
	// as it lists mechanical ones (AR-6).
	history, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	acts := store.FilterFactoryActs(history, time.Time{}, 50)
	listed := false
	for _, act := range acts {
		if act.ID == event.event.ID && act.TaskID == task.ID && act.Kind == "recovered" && act.Automatic {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("acts log misses event %d: %+v", event.event.ID, acts)
	}

	// A later checkpoint release of the same order is answered once more.
	second := f.release(t, orderID, authorityCheckpoint(cite(design, 2, "Recovery")))
	f.tick(t)
	f.requireAuthorityCount(t, task.ID, 1)
	f.confirm(t, design, f.propose(t, design, "operator", ""))
	f.tick(t)
	authority, _, _ = f.recoveries(t, task.ID)
	if len(authority) != 2 || authority[1].payload.RequestID != store.AuthorityConfirmedRequestID(orderID, second.LastAttemptID) || second.LastAttemptID == released.LastAttemptID {
		t.Fatalf("second release recoveries=%+v", authority)
	}
}

func (f authorityFixture) requireAuthorityCount(t *testing.T, taskID string, want int) {
	t.Helper()
	if authority, _, _ := f.recoveries(t, taskID); len(authority) != want {
		t.Fatalf("authority recoveries=%d want %d", len(authority), want)
	}
}

// operatorRace starts a clock tick and an operator recovery of one answered
// release together; exactly one recovers it.
func (f authorityFixture) operatorRace(t *testing.T) {
	st, ctx := f.x.Backend, f.x.Context
	for _, operatorFirst := range []bool{false, true} {
		design := f.document(t, "system_design")
		task, orderID := f.task(t, core.StageImplement)
		f.release(t, orderID, authorityCheckpoint(cite(design, 1, "Recovery")))
		f.confirm(t, design, f.propose(t, design, "task", task.ID))
		if operatorFirst {
			_, err := RecoverWorkOrderWithDirection(ctx, st, orderID, "operator-first", "operator direction", time.Hour)
			requireOK(t, err)
			f.tick(t)
			if authority, operator, _ := f.recoveries(t, task.ID); len(authority) != 0 || len(operator) != 1 {
				t.Fatalf("operator first: authority=%d operator=%d", len(authority), len(operator))
			}
			// The 261016-cdb507 shape: cited revisions confirmed, then an
			// operator recovery that keeps the checkpoint. The answered
			// question is not asked again and no recovery is offered.
			resumed, err := st.GetWorkOrder(ctx, orderID)
			requireOK(t, err)
			if resumed.Checkpoint == nil || resumed.Checkpoint.Class != core.WorkOrderCheckpointClassAuthorityConflict || resumed.LastAttemptOutcome != "" {
				t.Fatalf("operator first: resumed order=%+v, want the checkpoint kept and the outcome cleared", resumed)
			}
			requireResumedQueuedAction(t, st, ctx, task.ID, orderID)
			continue
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var tickErr, recoverErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, tickErr = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		}()
		go func() {
			defer wg.Done()
			<-start
			_, recoverErr = RecoverWorkOrderWithDirection(ctx, st, orderID, "operator-race", "operator direction", time.Hour)
		}()
		close(start)
		wg.Wait()
		if tickErr != nil && !isRetryable(tickErr) {
			t.Fatalf("tick: %v", tickErr)
		}
		authority, operator, _ := f.recoveries(t, task.ID)
		if len(authority)+len(operator) != 1 {
			t.Fatalf("race recovered %d times (authority=%d operator=%d recoverErr=%v)", len(authority)+len(operator), len(authority), len(operator), recoverErr)
		}
		if (recoverErr == nil) != (len(operator) == 1) {
			t.Fatalf("operator result err=%v operator events=%d", recoverErr, len(operator))
		}
		f.tick(t)
		if again, operatorAgain, _ := f.recoveries(t, task.ID); len(again)+len(operatorAgain) != 1 {
			t.Fatalf("tick after race recovered again: authority=%d operator=%d", len(again), len(operatorAgain))
		}
	}
}

func isRetryable(err error) bool {
	return errors.Is(err, store.ErrRetryable)
}

// staysReleased covers every AC-2.7 case and the excluded order shapes.
func (f authorityFixture) staysReleased(t *testing.T) {
	st, ctx := f.x.Backend, f.x.Context
	answered := func(t *testing.T, stage core.Stage, checkpoint func(authorityDocument) *core.WorkOrderCheckpoint) (core.Task, string, authorityDocument) {
		t.Helper()
		design := f.document(t, "system_design")
		task, orderID := f.task(t, stage)
		f.release(t, orderID, checkpoint(design))
		f.confirm(t, design, f.propose(t, design, "task", task.ID))
		return task, orderID, design
	}
	full := func(design authorityDocument) *core.WorkOrderCheckpoint {
		return authorityCheckpoint(cite(design, 1, "Recovery"))
	}

	t.Run("Dismissal", func(t *testing.T) {
		design := f.document(t, "system_design")
		task, orderID := f.task(t, core.StageImplement)
		f.release(t, orderID, full(design))
		f.dismiss(t, design, f.propose(t, design, "task", task.ID))
		f.tick(t)
		f.requireReleased(t, task.ID, orderID)
	})
	t.Run("PartialConfirmationOfTwoDocuments", func(t *testing.T) {
		design, requirement := f.document(t, "system_design"), f.document(t, "requirement")
		task, orderID := f.task(t, core.StageImplement)
		f.release(t, orderID, authorityCheckpoint(cite(design, 1, "Recovery"), cite(requirement, 1, "REQ-1")))
		f.confirm(t, design, f.propose(t, design, "task", task.ID))
		f.dismiss(t, requirement, f.propose(t, requirement, "task", task.ID))
		f.tick(t)
		f.requireReleased(t, task.ID, orderID)
	})
	for _, origin := range []string{"task", "session", "operator"} {
		t.Run("PendingVersionFrom_"+origin, func(t *testing.T) {
			for _, kind := range []string{"requirement", "system_design"} {
				document := f.document(t, kind)
				task, orderID := f.task(t, core.StageImplement)
				f.release(t, orderID, authorityCheckpoint(cite(document, 1, "REQ-1")))
				f.confirm(t, document, f.propose(t, document, "task", task.ID))
				other, _ := f.task(t, core.StageImplement)
				f.propose(t, document, origin, other.ID)
				f.tick(t)
				f.requireReleased(t, task.ID, orderID)
			}
		})
	}
	t.Run("AdvancedBeforeReleaseWithNewPendingVersion", func(t *testing.T) {
		design := f.document(t, "system_design")
		f.confirm(t, design, f.propose(t, design, "operator", ""))
		task, orderID := f.task(t, core.StageImplement)
		f.release(t, orderID, full(design))
		f.propose(t, design, "task", task.ID)
		f.tick(t)
		f.requireReleased(t, task.ID, orderID)
	})
	t.Run("NoClass", func(t *testing.T) {
		task, orderID, _ := answered(t, core.StageImplement, func(design authorityDocument) *core.WorkOrderCheckpoint {
			return &core.WorkOrderCheckpoint{DecisionRequest: "Confirm " + design.id + ", then recover."}
		})
		f.tick(t)
		f.requireReleased(t, task.ID, orderID)
	})
	t.Run("NoDecisionRequest", func(t *testing.T) {
		task, orderID, _ := answered(t, core.StageImplement, func(design authorityDocument) *core.WorkOrderCheckpoint {
			checkpoint := full(design)
			checkpoint.DecisionRequest = ""
			return checkpoint
		})
		f.tick(t)
		f.requireReleased(t, task.ID, orderID)
	})
	t.Run("VerifyStage", func(t *testing.T) {
		task, orderID, _ := answered(t, core.StageVerify, full)
		f.tick(t)
		f.requireReleased(t, task.ID, orderID)
	})
	t.Run("LiveClaim", func(t *testing.T) {
		task, orderID, _ := answered(t, core.StageImplement, full)
		// Recovered by the operator and claimed again before the tick.
		_, err := RecoverWorkOrderWithDirection(ctx, st, orderID, "operator-live", "operator direction", time.Hour)
		requireOK(t, err)
		session := "session-live-" + core.NewTaskID()
		claimed, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: "worker-live", ClaimantID: "worker-live", SessionID: session, ClientToken: session, Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		f.tick(t)
		after, err := st.GetWorkOrder(ctx, orderID)
		requireOK(t, err)
		if after.State != core.WorkOrderClaimed || after.SessionID != claimed.SessionID {
			t.Fatalf("live claim touched: %+v", after)
		}
		f.requireAuthorityCount(t, task.ID, 0)
	})
	t.Run("RecoveryRefusal", func(t *testing.T) {
		task, orderID, _ := answered(t, core.StageImplement, full)
		later := time.Now().UTC().Add(time.Second)
		successor := task.ID + "-implement-2"
		requireOK(t, st.CreateJob(ctx, core.Job{ID: successor, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: successor, TaskID: task.ID, JobID: successor, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: later, QueueDeadline: later.Add(time.Hour), CreatedAt: later}))
		f.tick(t)
		order, err := st.GetWorkOrder(ctx, orderID)
		requireOK(t, err)
		if order.OperatorDirection != "" || order.Checkpoint == nil {
			t.Fatalf("superseded order recovered: %+v", order)
		}
		f.requireAuthorityCount(t, task.ID, 0)
	})
	for _, tc := range []struct {
		name    string
		command core.TaskCommand
		state   core.TaskState
	}{
		{"PendingPlanRevision", core.TaskGatePlanRevision, core.TaskAwaiting},
		{"Parked", core.TaskDispatchFailFinal, core.TaskParked},
		{"Merged", core.TaskMergeRecover, core.TaskMerged},
		{"Closed", core.TaskCancel, core.TaskClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, orderID, _ := answered(t, core.StageImplement, full)
			moved, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: tc.command})
			requireOK(t, err)
			if moved.Task.State != tc.state {
				t.Fatalf("task state=%s want %s", moved.Task.State, tc.state)
			}
			f.tick(t)
			f.requireAuthorityCount(t, task.ID, 0)
			if order, err := st.GetWorkOrder(ctx, orderID); err != nil || order.OperatorDirection != "" {
				t.Fatalf("order=%+v err=%v", order, err)
			}
		})
	}
}
