package singlestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Store) CreateReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, id string, jobs []core.Job, orders []core.WorkOrder) error {
	if !lease.ValidForCommand(id, string(core.WorkOrderCmdCreate)) {
		return fmt.Errorf("review-round create requires a valid taskops lease")
	}
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return fmt.Errorf("review round requires one job per work order")
	}
	return s.taskTx(ctx, id, func(tx *sql.Tx) error {
		if _, err := getTaskRow(ctx, tx, id); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='review' AND review_round=?`, documentWorkspace(ctx), id, orders[0].ReviewRound).Scan(&count); err != nil {
			return err
		}
		if count == len(orders) {
			return nil
		}
		if count != 0 {
			return fmt.Errorf("review round %d is only partially persisted", orders[0].ReviewRound)
		}
		for i, j := range jobs {
			if j.TaskID != id || j.Stage != core.StageReview || orders[i].TaskID != id || orders[i].JobID != j.ID || orders[i].ReviewRound != orders[0].ReviewRound {
				return fmt.Errorf("invalid review round member %d", i)
			}
		}
		for i, j := range jobs {
			if err := s.insertReviewMemberTx(ctx, tx, j, orders[i]); err != nil {
				return err
			}
		}
		return taskEvent(ctx, tx, core.Event{TaskID: id, Kind: "review.round_created", Payload: core.JSONPayload(map[string]any{"review_round": orders[0].ReviewRound, "seat_count": len(orders)})})
	})
}
func (s *Store) insertReviewMemberTx(ctx context.Context, tx *sql.Tx, j core.Job, o core.WorkOrder) error {
	if err := insertJobRow(ctx, tx, j); err != nil {
		return err
	}
	if err := taskEvent(ctx, tx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.created", Payload: core.JSONPayload(j)}); err != nil {
		return err
	}
	clean := core.WorkOrder{ID: o.ID, TaskID: o.TaskID, JobID: o.JobID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: o.ReviewRound, ReviewSeat: o.ReviewSeat, RequiredModel: o.RequiredModel, RequiredHarness: o.RequiredHarness, RequiredHarnessConfig: o.RequiredHarnessConfig, ExecutionTimeoutText: o.ExecutionTimeoutText, ReasonCode: o.ReasonCode, ReviewKind: o.ReviewKind, ReviewScope: o.ReviewScope, BaselineSHA: o.BaselineSHA, HeadSHA: o.HeadSHA, CreatedAt: o.CreatedAt, QueueEnteredAt: o.QueueEnteredAt, QueueDeadline: o.QueueDeadline, ServedRequirementSnapshot: o.ServedRequirementSnapshot, GovernanceSnapshot: o.GovernanceSnapshot}
	return s.insertOrderTx(ctx, tx, orderDefaults(clean), false)
}
func supersededReviewWorkOrdersTx(ctx context.Context, tx *sql.Tx, id string) (map[string]bool, error) {
	events, err := documentTaskEvents(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return store.SupersededReviewWorkOrders(events), nil
}
func completedReviewRoundTx(ctx context.Context, tx *sql.Tx, id string, round int, orderID string) ([]completedReviewRecord, int, error) {
	superseded, err := supersededReviewWorkOrdersTx(ctx, tx, id)
	if err != nil {
		return nil, 0, err
	}
	required := 1
	if round > 0 {
		required = 0
		rows, err := tx.QueryContext(ctx, `SELECT id FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='review' AND review_round=?`, documentWorkspace(ctx), id, round)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var oid string
			if err = rows.Scan(&oid); err != nil {
				rows.Close()
				return nil, 0, err
			}
			if !superseded[oid] {
				required++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
		if required == 0 {
			required = 1
		}
	}
	events, err := documentTaskEvents(ctx, tx, id)
	if err != nil {
		return nil, 0, err
	}
	var reviews []completedReviewRecord
	for _, e := range events {
		if e.Kind != "review.completed" {
			continue
		}
		var r completedReviewRecord
		if err = json.Unmarshal(e.Payload, &r); err != nil {
			return nil, 0, err
		}
		if r.ReviewRound == round && (round != 0 || r.ReviewWorkOrderID == orderID) && !superseded[r.ReviewWorkOrderID] {
			reviews = append(reviews, r)
		}
	}
	sort.SliceStable(reviews, func(i, j int) bool { return reviews[i].ReviewSeat < reviews[j].ReviewSeat })
	return reviews, required, nil
}
func (s *Store) settleAcceptedReviewTx(ctx context.Context, tx *sql.Tx, o core.WorkOrder, j core.Job, now time.Time) error {
	if o.State != core.WorkOrderCompleted {
		next, err := core.TransitionWorkOrder(o.State, core.WorkOrderCmdSubmitReviewVerdict)
		if err != nil {
			return err
		}
		o.State = next
		o.Claimable = false
		o.OperatorDirection = ""
		o.UpdatedAt = now
		o.ContinuationSessionID = ""
		o.ContinuationAttemptID = ""
		o.ContinuationHarness = ""
		o.ContinuationLaunchEnvironment = ""
		if err = orderWrite(ctx, tx, o); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: o.TaskID, JobID: o.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(o), At: now}); err != nil {
			return err
		}
	}
	if j.State != core.JobDone {
		if err := store.ValidateJobTransition(j.State, core.JobDone); err != nil {
			return err
		}
		j.State = core.JobDone
		j.EndedAt = now
		j.CostUSD = &o.CostUSD
		j.TokensIn = o.TokensIn
		j.TokensOut = o.TokensOut
		values := jobValues(j)
		values["updated_at"] = now
		if _, err := writeRow(ctx, tx, rowWrite{table: "jobs", operation: "UPDATE", values: values, where: map[string]any{"workspace_id": documentWorkspace(ctx), "id": j.ID}}); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: j.TaskID, JobID: j.ID, Kind: "job.updated", Payload: core.JSONPayload(j), At: now}); err != nil {
			return err
		}
	}
	orders, err := s.listOrders(ctx, tx, []string{o.TaskID})
	if err != nil {
		return err
	}
	for _, i := range orders {
		if i.Stage != core.StageImplement || i.State != core.WorkOrderSubmitted || (i.ContinuationSessionID == "" && i.ContinuationAttemptID == "" && i.ContinuationHarness == "" && i.ContinuationLaunchEnvironment == "") {
			continue
		}
		i.ContinuationSessionID = ""
		i.ContinuationAttemptID = ""
		i.ContinuationHarness = ""
		i.ContinuationLaunchEnvironment = ""
		i.UpdatedAt = now
		if err = orderWrite(ctx, tx, i); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: i.TaskID, JobID: i.JobID, Kind: "work_order.updated", Payload: core.JSONPayload(i), At: now}); err != nil {
			return err
		}
	}
	return nil
}

type completedReviewRecord struct {
	ReviewWorkOrderID string `json:"review_work_order_id"`
	Verdict           string `json:"verdict"`
	ReasonCode        string `json:"reason_code"`
	Summary           string `json:"summary"`
	Feedback          string `json:"feedback"`
	ReviewerModel     string `json:"reviewer_model"`
	ReviewRound       int    `json:"review_round"`
	ReviewSeat        int    `json:"review_seat"`
	RequiredModel     string `json:"required_model"`
	RequiredHarness   string `json:"required_harness"`
	RequiredEffort    string `json:"required_effort"`
	ModelEnforcement  string `json:"model_enforcement"`
	ReviewedCommitSHA string `json:"reviewed_commit_sha"`
}

type reviewRoundResultRecord struct {
	ReviewRound     int                     `json:"review_round"`
	Verdict         string                  `json:"verdict"`
	ReasonCode      string                  `json:"reason_code"`
	Summary         string                  `json:"summary"`
	Feedback        string                  `json:"feedback,omitempty"`
	Reviews         []completedReviewRecord `json:"reviews"`
	ApprovedHeadSHA string                  `json:"approved_head_sha,omitempty"`
}

func aggregateReviewRoundResult(round int, reviews []completedReviewRecord) reviewRoundResultRecord {
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].ReviewSeat < reviews[j].ReviewSeat })
	if round == 0 && len(reviews) == 1 {
		review := reviews[0]
		result := reviewRoundResultRecord{ReviewRound: round, Verdict: review.Verdict, ReasonCode: review.ReasonCode, Summary: review.Summary, Feedback: review.Feedback, Reviews: reviews}
		if review.Verdict == "approve" {
			result.ApprovedHeadSHA = review.ReviewedCommitSHA
		}
		return result
	}
	result := reviewRoundResultRecord{ReviewRound: round, Verdict: "approve", ReasonCode: "approved", Summary: "All review panel seats approved.", Reviews: reviews}
	var feedback []string
	for _, review := range reviews {
		if review.Verdict == "changes_requested" {
			result.Verdict, result.ReasonCode, result.Summary = "changes_requested", "panel_changes_requested", "The review panel requested changes."
		}
		if strings.TrimSpace(review.Feedback) != "" {
			feedback = append(feedback, fmt.Sprintf("Seat %d (%s, %s): %s", review.ReviewSeat, review.RequiredModel, review.ModelEnforcement, strings.TrimSpace(review.Feedback)))
		}
	}
	if result.Verdict == "approve" && len(reviews) > 0 {
		result.ApprovedHeadSHA = reviews[0].ReviewedCommitSHA
		for _, review := range reviews[1:] {
			if review.ReviewedCommitSHA != result.ApprovedHeadSHA {
				result.Verdict, result.ReasonCode, result.Summary, result.ApprovedHeadSHA = "changes_requested", "review_head_mismatch", "Review seats evaluated different pull-request heads.", ""
				break
			}
		}
	}
	result.Feedback = strings.Join(feedback, "\n")
	return result
}

func reviewDecisionPayload(decision core.ReviewDecision) []byte {
	return core.JSONPayload(map[string]any{
		"review_work_order_id": decision.ReviewWorkOrderID, "verdict": decision.Verdict,
		"reason_code": decision.ReasonCode, "summary": decision.Summary, "feedback": decision.Feedback,
		"reviewed_commit_sha": decision.ReviewedCommitSHA, "reviewer": decision.Reviewer,
		"evidence_ids":           decision.EvidenceIDs,
		"requirement_citations":  decision.RequirementCitations,
		"done_criteria_coverage": decision.DoneCriteriaAssessment,
		"governance_assessment":  decision.GovernanceAssessment,
		"reviewer_model":         decision.ReviewerModel, "reviewer_session": decision.ReviewerSession,
		"same_model_as_implementer": decision.SameModelAsImplementer,
		"review_round":              decision.ReviewRound, "review_seat": decision.ReviewSeat,
		"review_kind": decision.ReviewKind, "review_scope": decision.ReviewScope,
		"baseline_sha": decision.BaselineSHA, "head_sha": decision.HeadSHA,
		"required_model": decision.RequiredModel, "required_harness": decision.RequiredHarness,
		"required_effort":      decision.RequiredEffort,
		"model_enforcement":    decision.ModelEnforcement,
		"publication_eligible": decision.PublicationEligible,
	})
}

func reviewPublicationFromDecision(decision core.ReviewDecision) core.ReviewPublication {
	return core.ReviewPublication{
		ReviewWorkOrderID: decision.ReviewWorkOrderID, TaskID: decision.TaskID, JobID: decision.JobID,
		Verdict: decision.Verdict, ReasonCode: decision.ReasonCode, Summary: decision.Summary,
		Feedback: decision.Feedback, ReviewedCommitSHA: decision.ReviewedCommitSHA,
		ReviewerModel: decision.ReviewerModel, ReviewerSession: decision.ReviewerSession,
		SameModelAsImplementer: decision.SameModelAsImplementer,
		ReviewRound:            decision.ReviewRound, ReviewSeat: decision.ReviewSeat,
		RequiredModel: decision.RequiredModel, RequiredHarness: decision.RequiredHarness, RequiredEffort: decision.RequiredEffort,
		ModelEnforcement: decision.ModelEnforcement, ForgeAuthorClass: core.ForgeAuthorWorkspace,
	}
}

func (s *Store) AcceptReviewDecisionCommand(ctx context.Context, lease taskops.TaskLease, decision core.ReviewDecision) error {
	if !lease.ValidFor(decision.TaskID) {
		return fmt.Errorf("review lifecycle mutation requires a valid taskops lease")
	}
	return s.taskTx(ctx, decision.TaskID, func(tx *sql.Tx) error {
		var completed, accepted bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=? AND task_id=? AND kind='review.completed' AND JSON_EXTRACT_STRING(payload_json,'review_work_order_id')=?),EXISTS(SELECT 1 FROM events WHERE workspace_id=? AND task_id=? AND kind='review.accepted' AND JSON_EXTRACT_STRING(payload_json,'review_work_order_id')=?)`, documentWorkspace(ctx), decision.TaskID, decision.ReviewWorkOrderID, documentWorkspace(ctx), decision.TaskID, decision.ReviewWorkOrderID).Scan(&completed, &accepted); err != nil {
			return err
		}
		before, err := getTaskRow(ctx, tx, decision.TaskID)
		if err != nil {
			return err
		}
		job, err := scanJob(documentRow(ctx, tx, `SELECT `+jobColumns+` FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), decision.JobID))
		if err != nil || job.TaskID != decision.TaskID {
			return fmt.Errorf("job %s does not belong to task %s in workspace %s", decision.JobID, decision.TaskID, documentWorkspace(ctx))
		}

		var order core.WorkOrder
		if decision.ClaimSession != "" {
			order, err = getOrderRow(ctx, tx, decision.ReviewWorkOrderID)
			if err != nil || order.TaskID != decision.TaskID || order.JobID != decision.JobID || order.Stage != core.StageReview {
				return fmt.Errorf("review work order %s does not belong to task %s in workspace %s", decision.ReviewWorkOrderID, decision.TaskID, documentWorkspace(ctx))
			}
		}
		if accepted {
			if decision.ClaimSession != "" {
				return s.settleAcceptedReviewTx(ctx, tx, order, job, time.Now().UTC())
			}
			return nil
		}
		if decision.ClaimSession != "" {
			if order.State != core.WorkOrderClaimed || order.SessionID != decision.ClaimSession || !order.LeaseExpiresAt.After(time.Now()) {
				return store.ErrWorkOrderClaimLost
			}
			if core.TaskState(before.State) == core.TaskQueued {
				if err = taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{
					"from": core.TaskQueued, "to": core.TaskRunning, "command": core.TaskOrderClaim, "claim_authority_work_order_id": order.ID,
				})}); err != nil {
					return err
				}
				before.State = core.TaskRunning
			}
		}
		if !completed {
			if err := taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.completed", Payload: reviewDecisionPayload(decision)}); err != nil {
				return err
			}
		}
		if decision.PublicationEligible {
			if err := s.queueReviewPublicationTx(ctx, tx, reviewPublicationFromDecision(decision)); err != nil {
				return err
			}
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.accepted", Payload: core.JSONPayload(map[string]any{"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound, "review_seat": decision.ReviewSeat})}); err != nil {
			return err
		}
		if decision.ClaimSession != "" {
			if err := s.settleAcceptedReviewTx(ctx, tx, order, job, time.Now().UTC()); err != nil {
				return err
			}
		}
		reviews, required, err := completedReviewRoundTx(ctx, tx, decision.TaskID, decision.ReviewRound, decision.ReviewWorkOrderID)
		if err != nil {
			return err
		}
		if len(reviews) < required {
			return taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, Kind: "review.round_pending", Payload: core.JSONPayload(map[string]any{"review_round": decision.ReviewRound, "completed": len(reviews), "required": required})})
		}
		aggregate := aggregateReviewRoundResult(decision.ReviewRound, reviews)
		if err = taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.round_completed", Payload: core.JSONPayload(aggregate)}); err != nil {
			return err
		}

		command, next, recovery := core.TaskGateMerge, core.Stage(""), core.StageImplement
		autoApprove := false
		if aggregate.Verdict == "changes_requested" {
			count, window, countErr := bounceCountsTx(ctx, tx, decision.TaskID)
			if countErr != nil {
				return countErr
			}

			count++
			window++
			actorID := fmt.Sprintf("review:round:%d", decision.ReviewRound)
			plan := store.PlanChangesRequested(store.ChangesRequestedInput{
				TaskID: decision.TaskID, JobID: decision.JobID, ActorID: actorID, ActorRole: core.ActorAgent,
				ReasonCode: aggregate.ReasonCode, Feedback: aggregate.Feedback, Source: "mcp-review-panel",
				Count: int(count), Window: int(window), MaxBounces: decision.MaxBounces, ReviewRound: decision.ReviewRound,
				Reviews: aggregate.Reviews, Requeue: core.TaskStageAdvance, EnforceLimit: true,
			})
			if err = insertInterventionTx(ctx, tx, plan.Intervention); err != nil {
				return err
			}
			for _, event := range plan.Events {
				if err = taskEvent(ctx, tx, event); err != nil {
					return err
				}
			}
			command, next, recovery = plan.Command, plan.NextStage, plan.Recovery
		} else if decision.ReviewKind == "refresh" || (decision.PolicyVersion > 0 && !decision.MergeApproval) || (decision.PolicyVersion == 0 && decision.Level == core.L0) {
			autoApprove, recovery = true, ""
		}
		fromState := core.TaskState(before.State)
		state, transitionErr := core.TransitionTask(fromState, command)
		if transitionErr != nil {
			return transitionErr
		}
		if autoApprove {
			approved, approveErr := core.TransitionTask(state, core.TaskInterventionApproveReview)
			if approveErr != nil {
				return approveErr
			}
			// Auto-approval currently projects running -> awaiting_human with
			// gate.merge even though the merge gate is off. The table has no direct
			// running -> approved edge; keep this explicit gap workaround visible
			// until a table amendment supplies the intended command.
			if err = taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command})}); err != nil {
				return err
			}
			fromState, state = state, approved
			command = core.TaskInterventionApproveReview
		}
		nonAdvancingRefresh := store.NonAdvancingRefreshBinding(decision, aggregate.ApprovedHeadSHA)
		if nonAdvancingRefresh {
			if err = taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, JobID: decision.JobID, Kind: "review.refresh_binding_not_advanced", Payload: core.JSONPayload(map[string]any{
				"review_work_order_id": decision.ReviewWorkOrderID, "review_round": decision.ReviewRound,
				"baseline_sha": decision.BaselineSHA, "head_sha": decision.HeadSHA, "bound_sha": aggregate.ApprovedHeadSHA,
			})}); err != nil {
				return err
			}
		}
		values := map[string]any{"state": state, "next_stage": next, "recovery_stage": recovery}
		if aggregate.Verdict == "approve" && aggregate.ApprovedHeadSHA != "" {
			values["reviewed_head_sha"] = aggregate.ApprovedHeadSHA
			if state == core.TaskApproved && !nonAdvancingRefresh {
				for k, v := range approvalValues(aggregate.ApprovedHeadSHA) {
					values[k] = v
				}
			}
		}
		if err = taskWrite(ctx, tx, decision.TaskID, values); err != nil {
			return err
		}

		// When autoApprove is true, this second projection records an intervention
		// command without a human intervention. It is the paired gap workaround for
		// the absent running -> approved table edge.
		if err := taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, Kind: "task.state_changed", Payload: core.JSONPayload(map[string]any{"from": fromState, "to": state, "command": command})}); err != nil {
			return err
		}
		if err := taskEvent(ctx, tx, core.Event{TaskID: decision.TaskID, Kind: "pipeline.transition_decided", Payload: core.JSONPayload(map[string]any{
			"from_stage": before.NextStage, "next_stage": next, "recovery_stage": recovery, "state": state,
			"review_round": decision.ReviewRound,
		})}); err != nil {
			return err
		}
		if state == core.TaskQueued {
			if _, err := s.enqueueTaskTx(ctx, tx, decision.TaskID, before.Workspace); err != nil {
				return err
			}
		}
		return nil
	})
}

func bounceCountsTx(ctx context.Context, tx *sql.Tx, id string) (int, int, error) {
	var last sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT MAX(at) FROM interventions WHERE workspace_id=? AND task_id=? AND actor_role='human'`, documentWorkspace(ctx), id).Scan(&last); err != nil {
		return 0, 0, err
	}
	cutoff := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
	if last.Valid {
		cutoff = last.Time
	}
	var count, window int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(at>?),0) FROM events WHERE workspace_id=? AND task_id=? AND kind='pipeline.bounced'`, cutoff, documentWorkspace(ctx), id).Scan(&count, &window)
	return count, window, err
}

// Acceptance publishes only a durable queue intent in the verdict transaction.
func (s *Store) queueReviewPublicationTx(ctx context.Context, tx *sql.Tx, p core.ReviewPublication) error {
	if err := store.NormalizeForgeAuthorProjectionForWrite(&p.ForgeAuthorClass, &p.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	if p.State == "" {
		p.State = core.ReviewPublicationQueued
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = p.CreatedAt
	if err := lockKey(ctx, tx, "review-publication:"+p.ReviewWorkOrderID); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_publications WHERE review_work_order_id=?)`, p.ReviewWorkOrderID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := documentParent(ctx, tx, "tasks", p.TaskID); err != nil {
		return err
	}
	j, err := scanJob(documentRow(ctx, tx, `SELECT `+jobColumns+` FROM jobs WHERE workspace_id=? AND id=?`, documentWorkspace(ctx), p.JobID))
	if err != nil || j.TaskID != p.TaskID {
		return fmt.Errorf("publication job does not belong to task")
	}
	_, err = writeRow(ctx, tx, rowWrite{table: "review_publications", operation: "INSERT", values: map[string]any{"review_work_order_id": p.ReviewWorkOrderID, "workspace_id": documentWorkspace(ctx), "task_id": p.TaskID, "job_id": p.JobID, "verdict": p.Verdict, "reason_code": p.ReasonCode, "summary": p.Summary, "feedback": p.Feedback, "reviewed_commit_sha": p.ReviewedCommitSHA, "reviewer_model": p.ReviewerModel, "reviewer_session": p.ReviewerSession, "same_model_as_implementer": p.SameModelAsImplementer, "review_round": p.ReviewRound, "review_seat": p.ReviewSeat, "required_model": p.RequiredModel, "required_harness": p.RequiredHarness, "required_effort": p.RequiredEffort, "model_enforcement": p.ModelEnforcement, "state": p.State, "forge_author_class": p.ForgeAuthorClass, "forge_author_user_id": p.ForgeAuthorUserID}})
	if err != nil {
		return err
	}
	if err = taskEvent(ctx, tx, core.Event{TaskID: p.TaskID, JobID: p.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(p)}); err != nil {
		return err
	}
	_, err = logqueue.Enqueue(s2log.WithTx(ctx, tx), s.log, documentWorkspace(ctx), queue.ReviewPublicationArgs{}.Kind(), p.ReviewWorkOrderID, queue.ReviewPublicationArgs{WorkspaceID: documentWorkspace(ctx), ReviewWorkOrderID: p.ReviewWorkOrderID}, 5, time.Now().UTC())
	return err
}

func reviewRoundOrdersTx(ctx context.Context, tx *sql.Tx, id string, round int) ([]core.WorkOrder, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+workOrderColumns+` FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='review' AND review_round=? ORDER BY review_seat`, documentWorkspace(ctx), id, round)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []core.WorkOrder{}
	for rows.Next() {
		o, err := scanWorkOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Store) RetryReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, r store.ReviewRoundRetryRequest, jobs []core.Job, orders []core.WorkOrder) (store.ReviewRoundRetryResult, error) {
	var result store.ReviewRoundRetryResult
	if !lease.ValidForCommand(r.TaskID, string(core.WorkOrderCmdCreate)) {
		return result, fmt.Errorf("review retry requires a valid taskops lease")
	}
	r.RequestID = strings.TrimSpace(r.RequestID)
	r.Reason = strings.TrimSpace(r.Reason)
	r.PRHead = strings.TrimSpace(r.PRHead)
	if r.RequestID == "" || r.Reason == "" || r.PRHead == "" {
		return result, fmt.Errorf("review retry request_id, reason, and verified PR head are required")
	}
	if len(jobs) == 0 || len(jobs) != len(orders) {
		return result, fmt.Errorf("review retry requires one job per work order")
	}
	err := s.taskTx(ctx, r.TaskID, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		if err := lockKey(ctx, tx, "review-retry-request:"+r.RequestID); err != nil {
			return err
		}
		var sw, st, sr, sh string
		var prior, next int
		err := tx.QueryRowContext(ctx, `SELECT workspace_id,task_id,reason,prior_round,new_round,pr_head FROM review_round_retries WHERE request_id=?`, r.RequestID).Scan(&sw, &st, &sr, &prior, &next, &sh)
		if err == nil {
			if sw != ws || st != r.TaskID || sr != r.Reason || sh != r.PRHead || prior != r.PriorRound {
				return fmt.Errorf("%w: request_id was already used for different inputs", store.ErrReviewRetryConflict)
			}
			created, err := reviewRoundOrdersTx(ctx, tx, r.TaskID, next)
			result = store.ReviewRoundRetryResult{RequestID: r.RequestID, TaskID: r.TaskID, PriorRound: prior, NewRound: next, PRHead: sh, WorkOrders: created}
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		t, err := getTaskRow(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		var latest int
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(review_round),0) FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='review'`, ws, r.TaskID).Scan(&latest); err != nil {
			return err
		}
		old, err := reviewRoundOrdersTx(ctx, tx, r.TaskID, latest)
		if err != nil {
			return err
		}
		active, recoverable := 0, 0
		var timedOutIDs, inconsistentIDs []string
		for _, o := range old {
			if o.State == core.WorkOrderQueued || o.State == core.WorkOrderClaimed || o.State == core.WorkOrderSubmitted {
				active++
			}
			if o.State == core.WorkOrderTimedOut {
				recoverable++
				timedOutIDs = append(timedOutIDs, o.ID)
			}
			if o.State == core.WorkOrderCompleted && (o.LastAttemptOutcome != "" || o.RetrySuppressed || o.LastFailureMessage != "") && (o.AttemptID == "" || o.LastAttemptID == o.AttemptID) {
				recoverable++
				inconsistentIDs = append(inconsistentIDs, o.ID)
			}
		}
		var resolved bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE workspace_id=? AND task_id=? AND kind='review.round_completed' AND JSON_EXTRACT_STRING(payload_json,'review_round')=?)`, ws, r.TaskID, fmt.Sprint(latest)).Scan(&resolved); err != nil {
			return err
		}
		if latest == 0 || latest != r.PriorRound || recoverable == 0 || active != 0 || resolved {
			return fmt.Errorf("%w: task has no matching recoverable non-progressing review round", store.ErrReviewRetryConflict)
		}
		next = r.PriorRound + 1
		for i, j := range jobs {
			o := orders[i]
			if j.TaskID != r.TaskID || j.Stage != core.StageReview || o.TaskID != r.TaskID || o.JobID != j.ID || o.Stage != core.StageReview || o.ReviewRound != next || o.ReviewSeat != i+1 {
				return fmt.Errorf("invalid review retry member %d", i)
			}
		}
		for i, j := range jobs {
			if err = s.insertReviewMemberTx(ctx, tx, j, orders[i]); err != nil {
				return err
			}
		}
		created, err := reviewRoundOrdersTx(ctx, tx, r.TaskID, next)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		actor := store.ActorFromContext(ctx)
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_retries(workspace_id,request_id,task_id,reason,prior_round,new_round,pr_head,actor_id,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, ws, r.RequestID, r.TaskID, r.Reason, r.PriorRound, next, r.PRHead, actor.ID, now); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: r.TaskID, Kind: "review.round_retried", At: now, Payload: core.JSONPayload(map[string]any{"request_id": r.RequestID, "workspace_id": ws, "task_id": r.TaskID, "actor": actor.ID, "reason": r.Reason, "prior_round": r.PriorRound, "new_round": next, "pr_head": r.PRHead, "timed_out_work_order_ids": timedOutIDs, "inconsistent_work_order_ids": inconsistentIDs, "setup_name": t.SetupName, "setup_contract": t.SetupContract})}); err != nil {
			return err
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: r.TaskID, Kind: "review.round_created", At: now, Payload: core.JSONPayload(map[string]any{"review_round": next, "seat_count": len(created), "retry_request_id": r.RequestID})}); err != nil {
			return err
		}
		result = store.ReviewRoundRetryResult{RequestID: r.RequestID, TaskID: r.TaskID, PriorRound: r.PriorRound, NewRound: next, PRHead: r.PRHead, WorkOrders: created}
		return nil
	})
	return result, err
}
func (s *Store) RecoverInterruptedReviewRoundCommand(ctx context.Context, lease taskops.TaskLease, r store.InterruptedReviewRecoveryRequest, timeout time.Duration) (store.InterruptedReviewRecoveryResult, error) {
	var result store.InterruptedReviewRecoveryResult
	if !lease.ValidForCommand(r.TaskID, string(core.WorkOrderCmdRecover)) {
		return result, fmt.Errorf("interrupted review recovery requires a valid taskops lease")
	}
	r.RequestID = strings.TrimSpace(r.RequestID)
	if r.RequestID == "" || r.TaskID == "" || r.Round <= 0 || timeout <= 0 {
		return result, fmt.Errorf("interrupted review recovery requires task, request_id, round, and queue timeout")
	}
	err := s.taskTx(ctx, r.TaskID, func(tx *sql.Tx) error {
		ws := documentWorkspace(ctx)
		if err := lockKey(ctx, tx, "interrupted-review-request:"+r.RequestID); err != nil {
			return err
		}
		var sw, st string
		var sr int
		var data []byte
		err := tx.QueryRowContext(ctx, `SELECT workspace_id,task_id,review_round,result_json FROM interrupted_review_recoveries WHERE request_id=?`, r.RequestID).Scan(&sw, &st, &sr, &data)
		if err == nil {
			if sw != ws || st != r.TaskID || sr != r.Round {
				return fmt.Errorf("%w: request_id was already used for different inputs", store.ErrReviewRetryConflict)
			}
			if err = json.Unmarshal(data, &result); err != nil {
				return err
			}
			if result.RecoveredOrders == nil {
				result.RecoveredOrders = []core.WorkOrder{}
			}
			if result.RetainedOrders == nil {
				result.RetainedOrders = []core.WorkOrder{}
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		t, err := getTaskRow(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		if core.TaskTerminal(t.State) {
			return fmt.Errorf("%w: terminal task cannot recover review work", store.ErrReviewRetryConflict)
		}
		events, err := documentTaskEvents(ctx, tx, r.TaskID)
		if err != nil {
			return err
		}
		var latest int
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(review_round),0) FROM work_orders WHERE workspace_id=? AND task_id=? AND stage='review'`, ws, r.TaskID).Scan(&latest); err != nil {
			return err
		}
		if latest == 0 || latest != r.Round {
			return fmt.Errorf("%w: task has no matching latest review round", store.ErrReviewRetryConflict)
		}
		orders, err := reviewRoundOrdersTx(ctx, tx, r.TaskID, r.Round)
		if err != nil {
			return err
		}
		superseded := store.SupersededReviewWorkOrders(events)
		current := orders[:0]
		for _, o := range orders {
			if !superseded[o.ID] {
				current = append(current, o)
			}
		}
		recovery := store.InterruptedReviewRecoveryNeeded(t, current, events)
		if recovery == nil || recovery.ReviewRound != r.Round {
			return fmt.Errorf("%w: task has no recoverable interrupted review seats or has a conflicting active attempt", store.ErrReviewRetryConflict)
		}
		result = store.InterruptedReviewRecoveryResult{RequestID: r.RequestID, TaskID: r.TaskID, ReviewRound: r.Round, RecoveredOrders: []core.WorkOrder{}, RetainedOrders: append([]core.WorkOrder{}, recovery.RetainedOrders...)}
		now := time.Now().UTC()
		actor := store.ActorFromContext(ctx)
		for _, o := range recovery.EligibleOrders {
			if c := r.Refreezes[o.ID]; c != nil {
				prior := t.SetupContract
				if err = taskWrite(ctx, tx, t.ID, map[string]any{"setup_contract": core.JSONPayload(c.Setup)}); err != nil {
					return err
				}
				t.SetupContract = c.Setup
				if !reflect.DeepEqual(prior, c.Setup) {
					if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: o.JobID, Kind: "task.setup.refrozen", At: now, Payload: core.JSONPayload(map[string]any{"prior": prior, "new": c.Setup, "request_id": r.RequestID, "work_order_id": o.ID, "actor": actor.ID})}); err != nil {
						return err
					}
				}
				break
			}
		}
		for _, o := range recovery.EligibleOrders {
			prior := o.LastAttemptOutcome
			o.LastAttemptOutcome = ""
			o.RetrySuppressed = false
			o.RetrySuppressionReason = ""
			o.AutomaticRetryCount = 0
			o.NextRetryAt = time.Time{}
			o.QueueEnteredAt = now
			o.QueueDeadline = now.Add(timeout)
			o.RedispatchCount++
			o.UpdatedAt = now
			o.Claimable = true
			if c := r.Refreezes[o.ID]; c != nil {
				o.RequiredModel = c.RequiredModel
				o.RequiredHarness = c.RequiredHarness
				o.RequiredEffort = c.RequiredEffort
				o.RequiredHarnessConfig = c.RequiredHarnessConfig
				o.ExecutionTimeoutText = c.ExecutionTimeoutText
			}
			if err = orderWrite(ctx, tx, o); err != nil {
				return err
			}
			result.RecoveredOrders = append(result.RecoveredOrders, o)
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: o.JobID, Kind: "review.seat_recovered", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "review_round": r.Round, "review_seat": o.ReviewSeat, "work_order_id": o.ID, "request_id": r.RequestID, "prior_state": core.WorkOrderQueued, "prior_outcome": prior, "resulting_state": o.State, "outcome": "recovered", "setup_name": t.SetupName, "setup_contract": t.SetupContract})}); err != nil {
				return err
			}
		}
		for _, o := range recovery.RetainedOrders {
			if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, JobID: o.JobID, Kind: "review.seat_recovery_skipped", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "review_round": r.Round, "review_seat": o.ReviewSeat, "work_order_id": o.ID, "request_id": r.RequestID, "prior_state": o.State, "resulting_state": o.State, "outcome": "retained_completed", "setup_name": t.SetupName, "setup_contract": t.SetupContract})}); err != nil {
				return err
			}
		}
		if err = taskEvent(ctx, tx, core.Event{TaskID: t.ID, Kind: "review.round_recovered", At: now, Payload: core.JSONPayload(map[string]any{"workspace_id": ws, "review_round": r.Round, "request_id": r.RequestID, "actor": actor.ID, "recovered_seats": len(result.RecoveredOrders), "retained_completed_seats": len(result.RetainedOrders)})}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO interrupted_review_recoveries(workspace_id,request_id,task_id,review_round,actor_id,result_json,created_at) VALUES(?,?,?,?,?,?,?)`, ws, r.RequestID, t.ID, r.Round, actor.ID, core.JSONPayload(result), now)
		return err
	})
	return result, err
}
