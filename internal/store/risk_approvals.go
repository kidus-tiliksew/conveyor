package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
)

// TaskApprovalStore records and reads the human approvals and author waivers
// of an enforcing task's head (req-risk-based-review AC-8.3 to AC-8.8;
// component-risk-review invariants 9 and 10; feature-risk-based-review RR-7).
// Every backend writes under the task's operation lock and keeps one row per
// workspace, task, head, review round, kind, and person, so a replayed or
// concurrent request by the same person records one row and one event.
// Records are append-only: a new head reaches the merge gate through a new
// review round, so an approval of an earlier round never counts again, and its
// row stays as audit history.
type TaskApprovalStore interface {
	// RecordTaskApproval records one approval or waiver. The task's frozen
	// risk mode must be enforce, and the task must wait at the merge gate or
	// be approved with the record's head as its reviewed head; otherwise it
	// is refused with nothing recorded. A record whose key already exists
	// returns the recorded row and false and writes nothing.
	RecordTaskApproval(ctx context.Context, approval core.TaskApproval) (core.TaskApproval, bool, error)
	// ListTaskApprovals returns every recorded approval and waiver of a task
	// in recorded order.
	ListTaskApprovals(ctx context.Context, taskID string) ([]core.TaskApproval, error)
}

// PrepareTaskApproval validates a record against the task under the task
// lock and stamps its time. It refuses a task that does not enforce, one that
// is not at the merge gate or approved, and a head that is not the task's
// reviewed head (req-risk-based-review AC-8.6; req-review-gates-evidence
// REQ-4).
func PrepareTaskApproval(task core.Task, approval core.TaskApproval, now time.Time) (core.TaskApproval, error) {
	approval.TaskID = task.ID
	if err := approval.Validate(); err != nil {
		return core.TaskApproval{}, Refuse(err)
	}
	if !task.SetupContract.Risk.Enforcing() {
		return core.TaskApproval{}, Refusef("the task's frozen risk policy does not enforce approvals")
	}
	atGate := task.State == core.TaskAwaiting && task.GateKind == core.GateKindMergeApproval
	if !atGate && task.State != core.TaskApproved {
		return core.TaskApproval{}, Refusef("the task is not waiting at the merge gate")
	}
	head := strings.TrimSpace(task.ReviewedHeadSHA)
	if head == "" || head != approval.HeadSHA {
		return core.TaskApproval{}, Refuse(&ApprovalHeadError{Head: approval.HeadSHA, Current: head})
	}
	approval.CreatedAt = now.UTC()
	return approval, nil
}

// ApprovalHeadError refuses an approval of a head that is not the task's
// reviewed head.
type ApprovalHeadError struct{ Head, Current string }

func (e *ApprovalHeadError) Error() string {
	if e.Current == "" {
		return "the task has no reviewed head to approve"
	}
	return fmt.Sprintf("head %s is not the task's reviewed head %s; approve the current head", e.Head, e.Current)
}

// TaskApprovalKeyOf is the uniqueness key of one record.
func TaskApprovalKeyOf(a core.TaskApproval) string {
	return strings.Join([]string{a.TaskID, a.HeadSHA, fmt.Sprint(a.ReviewRound), string(a.Kind), a.UserID}, "\x00")
}

// TaskApprovalRecordedEvent is the audited activity event of one record. The
// event actor is the request's authenticated person.
func TaskApprovalRecordedEvent(a core.TaskApproval) core.Event {
	kind := core.TaskApprovalEvent
	if a.Kind == core.TaskApprovalAuthorWaiver {
		kind = core.TaskAuthorWaiverEvent
	}
	return core.Event{TaskID: a.TaskID, Kind: kind, Payload: core.JSONPayload(core.TaskApprovalPayload(a))}
}

func (m *memory) RecordTaskApproval(ctx context.Context, approval core.TaskApproval) (core.TaskApproval, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.TaskApproval{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, key, err := m.riskTaskLocked(ctx, approval.TaskID)
	if err != nil {
		return core.TaskApproval{}, false, err
	}
	prepared, err := PrepareTaskApproval(task, approval, time.Now())
	if err != nil {
		return core.TaskApproval{}, false, err
	}
	for _, recorded := range m.taskApprovals[key] {
		if TaskApprovalKeyOf(recorded) == TaskApprovalKeyOf(prepared) {
			return recorded, false, nil
		}
	}
	if m.taskApprovals == nil {
		m.taskApprovals = map[memoryScopedKey][]core.TaskApproval{}
	}
	m.taskApprovals[key] = append(m.taskApprovals[key], prepared)
	m.appendEventLocked(ctx, TaskApprovalRecordedEvent(prepared))
	return prepared, true, nil
}

func (m *memory) ListTaskApprovals(ctx context.Context, taskID string) ([]core.TaskApproval, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, key, err := m.riskTaskLocked(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return slices.Clone(m.taskApprovals[key]), nil
}

// TaskAuthors reads a task's authors: the people whose sessions claimed its
// implementation orders, from every recorded claim and every current order,
// so earlier bounces and conflict fixes count (req-risk-based-review AC-8.4).
// A claim by an identity that resolves to no person sets unknown, and an
// unknown author is never trusted. Authors are deduplicated by user ID, never
// by session.
func TaskAuthors(events []core.Event, orders []core.WorkOrder) (authors []string, unknown bool) {
	seen := map[string]bool{}
	add := func(order core.WorkOrder) {
		if order.Stage != core.StageImplement || (order.ClaimantID == "" && order.WorkerID == "") {
			return
		}
		owner := strings.TrimPrefix(order.ClaimantID, core.TaskRunClaimantPrefix)
		if order.WorkerID != "" || !core.IsTaskRunClaimantID(order.ClaimantID) || strings.TrimSpace(owner) != owner {
			unknown = true
			return
		}
		if !seen[owner] {
			seen[owner] = true
			authors = append(authors, owner)
		}
	}
	for _, event := range events {
		if event.Kind != "work_order.claimed" {
			continue
		}
		var claimed core.WorkOrder
		if err := json.Unmarshal(event.Payload, &claimed); err != nil {
			unknown = true
			continue
		}
		add(claimed)
	}
	for _, order := range orders {
		add(order)
	}
	slices.Sort(authors)
	return authors, unknown
}

// LatestReviewRound is the highest review round among a task's orders, or 0.
func LatestReviewRound(orders []core.WorkOrder) int {
	round := 0
	for _, order := range orders {
		if order.Stage == core.StageReview && order.ReviewRound > round {
			round = order.ReviewRound
		}
	}
	return round
}

// ApprovalEvaluation is an enforcing task's approval state at its reviewed
// head: what its tier requires, the evaluation of the recorded approvals, and
// the head and round they bind to.
type ApprovalEvaluation struct {
	// Enforced is false for a task whose frozen mode is not enforce; every
	// other field is then zero and nothing was read.
	Enforced     bool
	Head         string
	ReviewRound  int
	Tier         riskpolicy.Tier
	Requirements riskpolicy.Requirements
	Status       riskpolicy.ApprovalStatus
	Authors      []string
	// Approvals are the counted and uncounted approval records of the head
	// and round; Waivers are its author waivers.
	Approvals []core.TaskApproval
	Waivers   []core.TaskApproval
}

// IsAuthor reports whether userID is one of the task's authors.
func (e ApprovalEvaluation) IsAuthor(userID string) bool {
	return slices.Contains(e.Authors, userID)
}

// EvaluateTaskApprovals reads an enforcing task's ratings, authors, review
// round, approvals, and each named person's operate_gates capability, and
// evaluates them with riskpolicy.EvaluateApprovals. trusted is the
// repository's live trusted_authors list, and prospective is a person whose
// approval is being considered, or empty. Nothing is cached: every call reads
// the capability and trust state current at that call (req-risk-based-review
// AC-9.3). A store without a membership boundary counts no approval, so the
// evaluation fails closed.
func EvaluateTaskApprovals(ctx context.Context, st Store, task core.Task, trusted []string, prospective string) (ApprovalEvaluation, error) {
	if !task.SetupContract.Risk.Enforcing() {
		return ApprovalEvaluation{}, nil
	}
	if task.Workspace != "" {
		ctx = WithWorkspace(ctx, task.Workspace)
	}
	ratings, err := st.ListTaskRiskRatings(ctx, task.ID)
	if err != nil {
		return ApprovalEvaluation{}, err
	}
	requirements, tier, _ := EnforcedRiskRequirements(task, ratings)
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		return ApprovalEvaluation{}, err
	}
	orders, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil {
		return ApprovalEvaluation{}, err
	}
	records, err := st.ListTaskApprovals(ctx, task.ID)
	if err != nil {
		return ApprovalEvaluation{}, err
	}
	authors, unknown := TaskAuthors(events, orders)
	evaluation := ApprovalEvaluation{
		Enforced: true, Head: strings.TrimSpace(task.ReviewedHeadSHA), ReviewRound: LatestReviewRound(orders),
		Tier: tier, Requirements: requirements, Authors: authors,
	}
	var approvers []string
	waived := false
	for _, record := range records {
		if evaluation.Head == "" || record.HeadSHA != evaluation.Head || record.ReviewRound != evaluation.ReviewRound {
			continue
		}
		switch record.Kind {
		case core.TaskApprovalPerson:
			evaluation.Approvals = append(evaluation.Approvals, record)
			approvers = append(approvers, record.UserID)
		case core.TaskApprovalAuthorWaiver:
			evaluation.Waivers = append(evaluation.Waivers, record)
		}
	}
	people := slices.Concat(authors, approvers)
	for _, waiver := range evaluation.Waivers {
		people = append(people, waiver.UserID)
	}
	if prospective != "" {
		people = append(people, prospective)
	}
	gatekeepers := map[string]bool{}
	membership, _ := st.(CapabilityAuthorizer)
	for _, userID := range people {
		if _, done := gatekeepers[userID]; done || membership == nil || userID == "" {
			continue
		}
		allowed, err := membership.AuthorizeWorkspace(ctx, userID, task.Workspace, core.CapabilityOperateGates)
		if err != nil {
			return ApprovalEvaluation{}, err
		}
		gatekeepers[userID] = allowed
	}
	// A waiver counts only from a person who holds operate_gates now and is
	// not an author (AC-8.5).
	for _, waiver := range evaluation.Waivers {
		if !slices.Contains(authors, waiver.UserID) && gatekeepers[waiver.UserID] {
			waived = true
			break
		}
	}
	evaluation.Status = riskpolicy.EvaluateApprovals(riskpolicy.ApprovalInput{
		Required: requirements.HumanApprovals, Authors: authors, UnknownAuthor: unknown, TrustedAuthors: trusted,
		Gatekeepers: gatekeepers, Approvals: approvers, Waived: waived, Prospective: prospective,
	})
	return evaluation, nil
}

// CapabilityAuthorizer answers whether one person holds a capability in a
// workspace now. Every production backend implements it through its
// MembershipStore.
type CapabilityAuthorizer interface {
	AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error)
}

// ProjectTaskApprovals fills the task read field risk's approval fields from
// an evaluation, so REST, MCP, the CLI, and the task page show one count
// (req-risk-based-review AC-11.2).
func ProjectTaskApprovals(projection *core.TaskRisk, evaluation ApprovalEvaluation) {
	if projection == nil || !evaluation.Enforced {
		return
	}
	status := evaluation.Status
	projection.RequiredHumanApprovals = max(projection.RequiredHumanApprovals, status.Required)
	projection.RecordedApprovals = status.Recorded
	projection.RemainingApprovals = status.Remaining
	projection.AuthorApprovalNeeded = status.TrustedAuthorApprovalNeeded
	projection.NonAuthorApprovalNeeded = status.NonAuthorApprovalNeeded
	projection.UntrustedAuthor = status.UntrustedAuthor
	projection.ApprovalHead = evaluation.Head
	projection.ApprovalsSatisfied = status.Satisfied
}

// AuthorHoldsOperateGates reports whether userID holds operate_gates in the
// workspace now. A store without a membership boundary answers false, so the
// caller fails closed; a read error is returned.
func AuthorHoldsOperateGates(ctx context.Context, st Store, workspace, userID string) (bool, error) {
	membership, ok := st.(CapabilityAuthorizer)
	if !ok || userID == "" {
		return false, nil
	}
	return membership.AuthorizeWorkspace(ctx, userID, workspace, core.CapabilityOperateGates)
}

// capabilityStore pairs a store with the membership boundary that answers
// capability questions for it.
type capabilityStore struct {
	Store
	authorizer CapabilityAuthorizer
}

func (s capabilityStore) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	return s.authorizer.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
}

// WithCapabilityAuthorizer returns st answering capability questions through
// authorizer, the same membership boundary the HTTP routes authorize with.
func WithCapabilityAuthorizer(st Store, authorizer CapabilityAuthorizer) Store {
	if authorizer == nil {
		return st
	}
	return capabilityStore{Store: st, authorizer: authorizer}
}
