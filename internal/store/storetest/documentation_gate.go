package storetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// REQ-9/AC-9.3–9.6; DEC-38, DEC-71. Same behavior on every backend.
func runDocumentationGate(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	policy := json.RawMessage(`{"schema_version":1,"docs":[{"path":"docs/api.md","governs":["internal/**"],"description":"API"}],"rule":{"none_statement":"docs: none","reason_required":true,"text":"Maintain API docs"}}`)
	evidence := core.DocumentationGateEvidence{TaskID: task.ID, HeadSHA: head, BaseSHA: base, PolicyJSON: policy, ChangedPaths: []string{"internal/server.go"}}
	before, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	if err := st.RecordDocumentationGateEvidence(ctx, evidence); !errors.Is(err, store.ErrDocumentationGateUnresolved) {
		t.Fatalf("affected untouched docs admitted: %v", err)
	}
	if _, err := st.GetDocumentationGateEvidence(ctx, task.ID, head); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refusal wrote evidence: %v", err)
	}
	after, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refusal wrote events")
	}
	evidence.AgentAuthoredBody = "docs: none internal refactor preserves documented behavior"
	requireOK(t, st.RecordDocumentationGateEvidence(ctx, evidence))
	got, err := st.GetDocumentationGateEvidence(ctx, task.ID, head)
	requireOK(t, err)
	if got.HeadSHA != head || got.BaseSHA != base || got.NoneReason != "internal refactor preserves documented behavior" || !reflect.DeepEqual(got.AffectedDocuments, []string{"docs/api.md"}) || string(got.PolicyJSON) == "" {
		t.Fatalf("evidence lost admission facts: %+v", got)
	}
	if !reflect.DeepEqual(got.MatchedPaths, []string{}) {
		t.Fatalf("empty matched paths must remain a JSON array: %+v", got)
	}
	var canonicalPolicy map[string]any
	requireOK(t, json.Unmarshal(policy, &canonicalPolicy))
	expected := got
	expected.PolicyJSON = core.JSONPayload(canonicalPolicy)
	if pack.DocumentationReviewContract(&got) != pack.DocumentationReviewContract(&expected) {
		t.Fatal("backend JSON storage changed the exact-head documentation prompt")
	}
	if _, err := st.GetDocumentationGateEvidence(ctx, task.ID, strings.Repeat("c", 40)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("evidence leaked to another head: %v", err)
	}
	other := newAggregateTask(t, x)
	if _, err := st.GetDocumentationGateEvidence(ctx, other.ID, head); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("evidence leaked to another task: %v", err)
	}
	evidence.HeadSHA = strings.Repeat("d", 40)
	evidence.AgentAuthoredBody = ""
	evidence.ChangedPaths = []string{"internal/server.go", "docs/api.md"}
	requireOK(t, st.RecordDocumentationGateEvidence(ctx, evidence))
	updated, err := st.GetDocumentationGateEvidence(ctx, task.ID, evidence.HeadSHA)
	requireOK(t, err)
	if !reflect.DeepEqual(updated.MatchedPaths, []string{"docs/api.md"}) || !reflect.DeepEqual(updated.AffectedDocuments, []string{}) || updated.NoneStatement != "" {
		t.Fatalf("updated doc classification=%+v", updated)
	}
	got.PolicyJSON[0] = '!'
	got.ChangedPaths[0] = "mutated"
	got, err = st.GetDocumentationGateEvidence(ctx, task.ID, head)
	requireOK(t, err)
	if got.PolicyJSON[0] == '!' || got.ChangedPaths[0] == "mutated" {
		t.Fatal("caller mutated durable evidence")
	}
	assessment := &core.DocumentationAssessment{Applicable: true, Summary: "API document updated", UpdatedPaths: []string{"docs/api.md"}, Unresolved: []string{}, Conflicts: []string{}}
	requireOK(t, store.ValidateDocumentationAssessment(&updated, "approve", assessment))
	assessment.Unresolved = []string{"behavior missing"}
	if !errors.Is(store.ValidateDocumentationAssessment(&updated, "approve", assessment), store.ErrDocumentationUnresolved) {
		t.Fatal("approve accepted unresolved finding")
	}
	requireOK(t, store.ValidateDocumentationAssessment(&updated, "changes_requested", assessment))
	assessment.Unresolved = []string{}
	jobs, orders := reviewRound(task.ID, 1)
	requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
	for i, order := range orders {
		decision := core.ReviewDecision{TaskID: task.ID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: i + 1, Verdict: "approve", ReasonCode: "verified", Summary: "documentation closure", ReviewedCommitSHA: evidence.HeadSHA, MaxBounces: 3, DocumentationAssessment: assessment}
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
	}
	events, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	completed, aggregated := 0, false
	for _, event := range events {
		if event.Kind == "review.completed" {
			var payload struct {
				Assessment *core.DocumentationAssessment `json:"documentation_assessment"`
			}
			requireOK(t, json.Unmarshal(event.Payload, &payload))
			if !reflect.DeepEqual(payload.Assessment, assessment) {
				t.Fatalf("review payload lost assessment: %s", event.Payload)
			}
			completed++
		}
		if event.Kind == "review.round_completed" {
			var payload struct {
				Reviews []struct {
					Assessment *core.DocumentationAssessment `json:"documentation_assessment"`
				} `json:"reviews"`
			}
			requireOK(t, json.Unmarshal(event.Payload, &payload))
			if len(payload.Reviews) != 2 {
				t.Fatalf("aggregate seats=%s", event.Payload)
			}
			for _, seat := range payload.Reviews {
				if !reflect.DeepEqual(seat.Assessment, assessment) {
					t.Fatalf("aggregation dropped assessment: %s", event.Payload)
				}
			}
			aggregated = true
		}
	}
	if completed != 2 || !aggregated {
		t.Fatalf("missing review payloads completed=%d aggregated=%t", completed, aggregated)
	}
	noConfig := newAggregateTask(t, x)
	jobs, orders = reviewRound(noConfig.ID, 1)
	requireOK(t, CreateReviewRound(ctx, st, noConfig.ID, jobs, orders))
	for i, order := range orders {
		requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: noConfig.ID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: i + 1, Verdict: "approve", ReasonCode: "verified", Summary: "legacy no-config review", ReviewedCommitSHA: head, MaxBounces: 3}))
	}
	events, err = st.ListEvents(ctx, noConfig.ID)
	requireOK(t, err)
	completed = 0
	for _, event := range events {
		if event.Kind != "review.completed" {
			continue
		}
		var payload map[string]json.RawMessage
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if value, exists := payload["documentation_assessment"]; exists {
			t.Fatalf("no-config payload must omit documentation_assessment, including null: %s (%s)", event.Payload, value)
		}
		completed++
	}
	if completed != 2 {
		t.Fatalf("no-config completed seats=%d", completed)
	}
	t.Run("atomic-submission-evidence", func(t *testing.T) {
		for _, scenario := range []string{"write", "replace", "rollback", "remove"} {
			t.Run(scenario, func(t *testing.T) {
				order := newAggregateOrder(t, x)
				claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("usr-submit"), OwnerUserID: "usr-submit", SessionID: "submitter", ClientToken: "fixture", Lease: time.Hour, ExecutionTimeout: time.Hour}
				order, err := ClaimWorkOrder(ctx, st, order.ID, claim)
				requireOK(t, err)
				prior := evidence
				prior.TaskID, prior.HeadSHA, prior.AgentAuthoredBody = order.TaskID, head, "docs: none PRIOR-REASON"
				prior.ChangedPaths = []string{"internal/server.go"}
				if scenario != "write" {
					requireOK(t, st.RecordDocumentationGateEvidence(ctx, prior))
				}
				next := prior
				next.AgentAuthoredBody = "docs: none CURRENT-REASON"
				var admitted *core.DocumentationGateEvidence = &next
				if scenario == "remove" {
					admitted = nil
				}
				if scenario == "rollback" {
					requireOK(t, st.UpdateJob(ctx, core.Job{ID: order.JobID, TaskID: order.TaskID, Stage: order.Stage, State: core.JobFailed, EndedAt: time.Now().UTC()}))
				}
				beforeOrder, beforeJobs, beforeEvents, beforeTask := submissionSnapshot(t, x, order)
				order.State, order.HeadSHA = core.WorkOrderSubmitted, head
				_, submitErr := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdSubmitForReview, func(lease taskops.TaskLease) (core.Job, error) {
					return st.SubmitImplementationCommand(ctx, lease, store.ImplementationSubmission{Order: order, EndedAt: time.Now().UTC(), DocumentationChecked: true, DocumentationEvidence: admitted})
				})
				got, gateErr := st.GetDocumentationGateEvidence(ctx, order.TaskID, head)
				if scenario == "rollback" {
					if submitErr == nil {
						t.Fatal("non-running job admitted documentation submission")
					}
					afterOrder, afterJobs, afterEvents, afterTask := submissionSnapshot(t, x, order)
					if !reflect.DeepEqual(beforeOrder, afterOrder) || !reflect.DeepEqual(beforeJobs, afterJobs) || beforeEvents != afterEvents || !reflect.DeepEqual(beforeTask, afterTask) {
						t.Fatal("failed documentation submission changed order, jobs, task, or events")
					}
					if gateErr != nil || got.NoneReason != "PRIOR-REASON" {
						t.Fatalf("rollback changed prior evidence: %+v %v", got, gateErr)
					}
					return
				}
				requireOK(t, submitErr)
				if scenario == "remove" {
					if !errors.Is(gateErr, store.ErrNotFound) {
						t.Fatalf("absent/unknown submission retained same-head evidence: %+v %v", got, gateErr)
					}
					return
				}
				requireOK(t, gateErr)
				if got.NoneReason != "CURRENT-REASON" || got.Status != "" {
					t.Fatalf("submission did not atomically replace admitted evidence: %+v", got)
				}
				completedOrder, jobs, _, completedTask := submissionSnapshot(t, x, order)
				if completedOrder.State != core.WorkOrderSubmitted || jobs[order.JobID] != core.JobDone || completedTask.ReviewedHeadSHA != head {
					t.Fatalf("evidence admission did not complete exact order/head: %+v %v %+v", completedOrder, jobs, completedTask)
				}
			})
		}
	})
}
