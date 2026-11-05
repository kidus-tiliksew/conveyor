package workorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// summaryAuthorityStore is the memory store plus a scripted membership
// authority, and an optional per-order override so every order state reaches
// the real admission path.
type summaryAuthorityStore struct {
	store.Store
	claimWork map[string]bool
	overrides map[string]core.WorkOrder
}

func (s *summaryAuthorityStore) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	if order, ok := s.overrides[id]; ok {
		return order, nil
	}
	return s.Store.GetWorkOrder(ctx, id)
}

func (s *summaryAuthorityStore) AuthorizeWorkspace(_ context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	return workspaceID == summaryWorkspace && capability == core.CapabilityClaimWork && s.claimWork[userID], nil
}
func (s *summaryAuthorityStore) AuthorizeWorkspaceCapabilities(ctx context.Context, userID, workspaceID string, capabilities []core.Capability) (map[core.Capability]bool, error) {
	out := make(map[core.Capability]bool, len(capabilities))
	for _, capability := range capabilities {
		out[capability], _ = s.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
	}
	return out, nil
}
func (s *summaryAuthorityStore) AuthorizeDeployment(context.Context, string, core.Capability) (bool, error) {
	return false, nil
}
func (s *summaryAuthorityStore) AuthorizeInstanceAdministration(context.Context, string) (bool, error) {
	return false, nil
}
func (s *summaryAuthorityStore) ListWorkspacesForUser(context.Context, string) ([]core.Workspace, error) {
	return nil, nil
}
func (s *summaryAuthorityStore) ListWorkspaceMembers(context.Context, string, string) ([]core.WorkspaceMembership, error) {
	return nil, nil
}
func (s *summaryAuthorityStore) ListWorkspaceInvitations(context.Context, string) ([]core.WorkspaceInvitation, error) {
	return nil, nil
}
func (s *summaryAuthorityStore) GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error) {
	return core.MembershipGrant{}, errors.New("unsupported")
}
func (s *summaryAuthorityStore) RevokeWorkspaceInvitation(context.Context, string, string) error {
	return errors.New("unsupported")
}
func (s *summaryAuthorityStore) RevokeWorkspaceRole(context.Context, string, string) error {
	return errors.New("unsupported")
}

const summaryWorkspace = "demo"

type summaryFixture struct {
	t       *testing.T
	base    context.Context
	st      *summaryAuthorityStore
	service *Service
	taskID  string
}

// newSummaryFixture stores a task whose implementation was submitted and
// whose round 1 settled with seat 1 requesting changes and seat 2 approving,
// through the real claim and acceptance paths.
func newSummaryFixture(t *testing.T) *summaryFixture {
	t.Helper()
	base := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), summaryWorkspace)
	st := &summaryAuthorityStore{Store: store.NewMemory(), claimWork: map[string]bool{}, overrides: map[string]core.WorkOrder{}}
	f := &summaryFixture{t: t, base: base, st: st, service: &Service{Store: st}, taskID: "summary-task"}
	f.task(f.taskID)
	implement := f.order(f.taskID, f.taskID+"-implement-1", core.StageImplement, 0, 0)
	f.claim(implement, "implement-session")
	submitted, err := st.GetWorkOrder(base, implement)
	f.ok(err)
	submitted.State = core.WorkOrderSubmitted
	f.ok(storetest.UpdateWorkOrder(base, st, submitted, core.WorkOrderCmdSubmitForReview))
	var jobs []core.Job
	var orders []core.WorkOrder
	for seat := 1; seat <= 2; seat++ {
		id := fmt.Sprintf("%s-review-1-seat-%d", f.taskID, seat)
		jobs = append(jobs, core.Job{ID: id, TaskID: f.taskID, Stage: core.StageReview, State: core.JobPending})
		orders = append(orders, core.WorkOrder{ID: id, JobID: id, TaskID: f.taskID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: seat, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour)})
	}
	f.ok(storetest.CreateReviewRound(base, st, f.taskID, jobs, orders))
	for i, verdict := range []string{"changes_requested", "approve"} {
		session := fmt.Sprintf("seat-%d-session", i+1)
		f.claim(orders[i].ID, session)
		decision := core.ReviewDecision{TaskID: f.taskID, JobID: orders[i].JobID, ReviewWorkOrderID: orders[i].ID, ReviewRound: 1, ReviewSeat: i + 1, ClaimSession: session, Verdict: verdict, ReasonCode: "verified", Summary: "seat verdict", Feedback: "seat feedback", PolicyVersion: 1, MaxBounces: 5}
		f.ok(taskops.New(st).AcceptReviewDecision(base, decision))
	}
	return f
}

func (f *summaryFixture) ok(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *summaryFixture) task(id string) {
	f.t.Helper()
	f.ok(f.st.CreateTask(f.base, core.Task{ID: id, Workspace: summaryWorkspace, Repo: "conveyor", Branch: "conveyor/task-" + id, BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, PolicyVersion: 1, CreatedAt: time.Now().UTC()}))
}

func (f *summaryFixture) order(taskID, id string, stage core.Stage, round, seat int) string {
	f.t.Helper()
	f.ok(f.st.CreateJob(f.base, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
	f.ok(storetest.CreateWorkOrder(f.base, f.st, core.WorkOrder{ID: id, JobID: id, TaskID: taskID, Stage: stage, ReviewRound: round, ReviewSeat: seat}))
	return id
}

func (f *summaryFixture) claim(id, session string) {
	f.t.Helper()
	_, err := storetest.ClaimWorkOrder(f.base, f.st, id, core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", ClaimantID: core.TaskRunClaimantID("usr_owner"), OwnerUserID: "usr_owner", Agent: "codex", Model: "model", Lease: time.Hour, ExecutionTimeout: time.Hour})
	f.ok(err)
}

func userSummaryContext(base context.Context, owner string) context.Context {
	ctx := store.WithActor(base, store.Actor{ID: store.UserActorID(owner), Role: core.ActorUser})
	return store.WithCredential(ctx, core.AuthenticatedCredential{ID: "pat_" + owner, OwnerUserID: owner, Kind: core.CredentialUser, Scope: core.CredentialScopeUser})
}

// retiredAgentSummaryContext presents the retired agent class directly: no
// store verification returns it now, and admission still refuses it (DEC-116(1)).
func retiredAgentSummaryContext(base context.Context, owner string) context.Context {
	credential := core.AuthenticatedCredential{ID: "agt_" + owner, OwnerUserID: owner, Kind: core.CredentialKind("agent"), Scope: core.CredentialScopeUser}
	ctx := store.WithActor(base, store.Actor{ID: store.AgentActorID(credential.ID), Role: core.ActorAgent})
	return store.WithCredential(ctx, credential)
}

func (f *summaryFixture) input(requestID string) ReviewSummaryInput {
	return ReviewSummaryInput{TaskID: f.taskID, ReviewRound: 1, RequestID: requestID, Summary: "Seat 1 found a defect.", AuthorModel: "claude-opus-5-5", Findings: []store.ReviewSummaryFinding{
		{Severity: store.ReviewSummarySeverityBlocking, Problem: "Defect.", Seats: []int{1}},
	}}
}

func (f *summaryFixture) events(taskID string) []core.Event {
	f.t.Helper()
	events, err := f.st.ListEvents(f.base, taskID)
	f.ok(err)
	return events
}

func (f *summaryFixture) summaryPayload(eventID int64) store.ReviewSummaryPayload {
	f.t.Helper()
	for _, event := range f.events(f.taskID) {
		if event.ID == eventID && event.Kind == store.ReviewSummaryRecordedEvent {
			var payload store.ReviewSummaryPayload
			f.ok(json.Unmarshal(event.Payload, &payload))
			return payload
		}
	}
	f.t.Fatalf("summary event %d not found", eventID)
	return store.ReviewSummaryPayload{}
}

// TestRecordReviewSummaryAuthorization pins SU-3 admission: who may author a
// summary, the credential-derived author record, and one uniform refusal that
// writes nothing and discloses nothing (req-review-gates-evidence AC-10.3;
// DEC-19; DEC-79(4)).
func TestRecordReviewSummaryAuthorization(t *testing.T) {
	f := newSummaryFixture(t)
	for _, user := range []string{"usr_executor", "usr_seat", "usr_owner"} {
		f.st.claimWork[user] = true
	}
	seat1, seat2 := f.taskID+"-review-1-seat-1", f.taskID+"-review-1-seat-2"
	for _, id := range []string{seat1, seat2} {
		order, err := f.st.GetWorkOrder(f.base, id)
		f.ok(err)
		if order.State != core.WorkOrderCompleted {
			t.Fatalf("seat %s state=%s, want completed after settlement", id, order.State)
		}
	}
	other := "summary-other"
	f.task(other)
	f.claim(f.order(other, other+"-implement-1", core.StageImplement, 0, 0), "other-session")

	admitted := []struct {
		name string
		ctx  context.Context
	}{
		{"executor with claim_work", userSummaryContext(f.base, "usr_executor")},
		{"seat session's person with claim_work", userSummaryContext(f.base, "usr_seat")},
		{"implementation session's person with claim_work", userSummaryContext(f.base, "usr_owner")},
	}
	for i, tc := range admitted {
		input := f.input(fmt.Sprintf("admitted-%d", i))
		result, err := f.service.RecordReviewSummary(tc.ctx, input)
		if err != nil {
			t.Fatalf("%s refused: %v", tc.name, err)
		}
		if result.Version != i+1 || result.Replayed {
			t.Fatalf("%s result=%+v", tc.name, result)
		}
		payload := f.summaryPayload(result.EventID)
		actor := store.ActorFromContext(tc.ctx)
		want := store.ReviewSummaryAuthor{ActorID: actor.ID, ActorRole: core.ActorUser, Model: "claude-opus-5-5", ModelEnforcement: "self-reported"}
		if payload.Author != want {
			t.Fatalf("%s author=%+v, want %+v", tc.name, payload.Author, want)
		}
	}

	refusals := []struct {
		name  string
		ctx   context.Context
		task  string
		round int
	}{
		{"user without claim_work", userSummaryContext(f.base, "usr_viewer"), "", 1},
		{"user without claim_work on an unsettled round", userSummaryContext(f.base, "usr_viewer"), "", 99},
		{"user with claim_work naming a missing task", userSummaryContext(f.base, "usr_executor"), "summary-absent", 1},
		{"user with claim_work in another workspace", store.WithWorkspace(userSummaryContext(f.base, "usr_executor"), "beta"), "", 1},
		{"retired agent credential with claim_work", retiredAgentSummaryContext(f.base, "usr_seat"), "", 1},
		{"retired agent credential naming a missing task", retiredAgentSummaryContext(f.base, "usr_seat"), "summary-absent", 1},
		{"retired agent credential in another workspace", store.WithWorkspace(retiredAgentSummaryContext(f.base, "usr_seat"), "beta"), "", 1},
		{"person's credential under the agent actor", store.WithActor(userSummaryContext(f.base, "usr_seat"), store.Actor{ID: store.AgentActorID("agt_usr_seat"), Role: core.ActorAgent}), "", 1},
		{"missing credential", store.WithActor(f.base, store.Actor{ID: store.UserActorID("usr_executor"), Role: core.ActorUser}), "", 1},
	}
	before := len(f.events(f.taskID))
	for i, tc := range refusals {
		input := f.input(fmt.Sprintf("refused-%d", i))
		if tc.task != "" {
			input.TaskID = tc.task
		}
		input.ReviewRound = tc.round
		_, err := f.service.RecordReviewSummary(tc.ctx, input)
		if err != store.ErrWorkOrderClaimUnauthorized || err.Error() != store.ErrWorkOrderClaimUnauthorized.Error() {
			t.Fatalf("%s err=%v, want exactly ErrWorkOrderClaimUnauthorized", tc.name, err)
		}
	}
	if after := len(f.events(f.taskID)); after != before {
		t.Fatalf("refusals appended events %d -> %d", before, after)
	}
	if events := f.events(other); len(events) == 0 {
		t.Fatal("other task lost its events")
	}

	// Without a membership authority a user is refused, fail closed.
	bare := &Service{Store: f.st.Store}
	if _, err := bare.RecordReviewSummary(userSummaryContext(f.base, "usr_executor"), f.input("no-authority")); err != store.ErrWorkOrderClaimUnauthorized {
		t.Fatalf("missing authority err=%v", err)
	}
	// Admitted callers still meet SU-2 validation after admission.
	invalid := f.input("invalid")
	invalid.Findings = nil
	if _, err := f.service.RecordReviewSummary(userSummaryContext(f.base, "usr_executor"), invalid); !errors.Is(err, store.ErrReviewSummaryInvalid) {
		t.Fatalf("uncited seat err=%v", err)
	}
	unsettled := f.input("unsettled")
	unsettled.ReviewRound = 2
	if _, err := f.service.RecordReviewSummary(userSummaryContext(f.base, "usr_executor"), unsettled); !errors.Is(err, store.ErrReviewSummaryRoundNotSettled) {
		t.Fatalf("unsettled round err=%v", err)
	}
}

// TestRecordReviewSummaryLeavesLifecycleUnchanged proves a recorded summary
// changes no task, order, job, intervention, or bounce (AC-10.7) through the
// service, and that admission performs no renewal or other write.
func TestRecordReviewSummaryLeavesLifecycleUnchanged(t *testing.T) {
	f := newSummaryFixture(t)
	type snapshot struct {
		task          core.Task
		orders        []core.WorkOrder
		jobs          []core.Job
		interventions []core.Intervention
		events        int
	}
	read := func() snapshot {
		var s snapshot
		var err error
		s.task, err = f.st.GetTask(f.base, f.taskID)
		f.ok(err)
		s.orders, err = f.st.ListTaskWorkOrdersStored(f.base, f.taskID)
		f.ok(err)
		s.jobs, err = f.st.ListJobs(f.base, f.taskID)
		f.ok(err)
		s.interventions, err = f.st.ListInterventions(f.base, f.taskID)
		f.ok(err)
		s.events = len(f.events(f.taskID))
		return s
	}
	f.st.claimWork["usr_seat"] = true
	before := read()
	input := f.input("unchanged")
	if _, err := f.service.RecordReviewSummary(userSummaryContext(f.base, "usr_seat"), input); err != nil {
		t.Fatal(err)
	}
	after := read()
	if after.events != before.events+1 {
		t.Fatalf("events %d -> %d, want exactly one summary event", before.events, after.events)
	}
	after.events = before.events
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("summary changed lifecycle state:\nbefore=%+v\nafter=%+v", before, after)
	}
}
