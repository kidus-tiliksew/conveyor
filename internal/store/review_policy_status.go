package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// ReviewPolicyStatusStore persists the conveyor/review-policy publication of
// an enforcing task's heads (req-risk-based-review AC-10.1, AC-10.2;
// component-git-delivery invariant 37). Every backend writes under the task's
// operation lock and keeps at most one row per workspace, task, and head. A
// queue that changes a row appends its event and enqueues the head's
// review_policy_status job in the same transaction, so a crash leaves either
// no intent or an intent with its job.
type ReviewPolicyStatusStore interface {
	// QueueReviewPolicyStatus records that head's requirements call for
	// desired. It inserts a queued row, reopens a published or failed row
	// whose state differs, and reopens a failed row for a retry. It writes
	// nothing and reports false when the row already holds desired and is
	// open or published at it. It refuses a task whose frozen risk mode is
	// not enforce.
	QueueReviewPolicyStatus(ctx context.Context, taskID, head, desired string) (core.ReviewPolicyStatus, bool, error)
	// GetReviewPolicyStatus reads one head's row; ok is false when none
	// exists.
	GetReviewPolicyStatus(ctx context.Context, taskID, head string) (core.ReviewPolicyStatus, bool, error)
	// ListReviewPolicyStatuses reads every row of a task in creation order.
	ListReviewPolicyStatuses(ctx context.Context, taskID string) ([]core.ReviewPolicyStatus, error)
	// UpdateReviewPolicyStatus records one attempt's outcome. The row's
	// revision must equal the stored revision, so a stale writer changes
	// nothing and gets ErrReviewPolicyStatusStale.
	UpdateReviewPolicyStatus(ctx context.Context, status core.ReviewPolicyStatus) (core.ReviewPolicyStatus, error)
	// ListOpenReviewPolicyStatuses lists the workspace's queued, retrying,
	// and failed rows for startup recovery.
	ListOpenReviewPolicyStatuses(ctx context.Context) ([]core.ReviewPolicyStatus, error)
}

// ErrReviewPolicyStatusStale refuses an update prepared from a row another
// writer has since changed.
var ErrReviewPolicyStatusStale = fmt.Errorf("policy status row changed since it was read")

// PlanReviewPolicyStatusQueue decides a queue request against the stored row,
// which is nil when the head has none. It returns the row to write and true,
// or the stored row and false when nothing changes.
func PlanReviewPolicyStatusQueue(task core.Task, existing *core.ReviewPolicyStatus, head, desired string, now time.Time) (core.ReviewPolicyStatus, bool, error) {
	head = strings.TrimSpace(head)
	if !task.SetupContract.Risk.Enforcing() {
		return core.ReviewPolicyStatus{}, false, Refusef("the task's frozen risk mode does not enforce, so it publishes no policy status")
	}
	if head == "" || !core.ValidPolicyStatus(desired) {
		return core.ReviewPolicyStatus{}, false, Refusef("a policy status needs a head and the state pending or success")
	}
	now = now.UTC()
	if existing == nil {
		row := core.ReviewPolicyStatus{TaskID: task.ID, HeadSHA: head, Desired: desired, State: core.ReviewPolicyStatusQueued, Revision: 1, CreatedAt: now, UpdatedAt: now}
		return row, true, row.Validate()
	}
	row := *existing
	if row.Desired == desired {
		switch row.State {
		case core.ReviewPolicyStatusQueued, core.ReviewPolicyStatusRetrying:
			return row, false, nil
		case core.ReviewPolicyStatusPublished:
			if row.Published == desired {
				return row, false, nil
			}
		}
	}
	row.Desired = desired
	row.State = core.ReviewPolicyStatusQueued
	row.ForgeErrorCategory, row.LastError = "", ""
	row.Revision++
	row.UpdatedAt = now
	if err := core.TransitionReviewPolicyStatus(existing.State, row.State); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return row, true, row.Validate()
}

// PlanReviewPolicyStatusReopen reopens a row for re-evaluation in the
// transaction of an act that can leave its head short of what it published,
// a tier raise. A queued or retrying row already has a job whose handler
// re-reads the requirements, so it is left alone and false is returned.
func PlanReviewPolicyStatusReopen(row core.ReviewPolicyStatus, now time.Time) (core.ReviewPolicyStatus, bool) {
	switch row.State {
	case core.ReviewPolicyStatusQueued, core.ReviewPolicyStatusRetrying:
		return row, false
	}
	row.State = core.ReviewPolicyStatusQueued
	row.ForgeErrorCategory, row.LastError = "", ""
	row.Revision++
	row.UpdatedAt = now.UTC()
	return row, true
}

// PrepareReviewPolicyStatusUpdate validates an update against the stored row
// and stamps its revision and time.
func PrepareReviewPolicyStatusUpdate(current, next core.ReviewPolicyStatus, now time.Time) (core.ReviewPolicyStatus, error) {
	if next.TaskID != current.TaskID || next.HeadSHA != current.HeadSHA {
		return core.ReviewPolicyStatus{}, fmt.Errorf("policy status update names another row")
	}
	if next.Revision != current.Revision {
		return core.ReviewPolicyStatus{}, ErrReviewPolicyStatusStale
	}
	if err := core.TransitionReviewPolicyStatus(current.State, next.State); err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	if err := next.Validate(); err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	next.CreatedAt = current.CreatedAt
	next.Revision = current.Revision + 1
	next.UpdatedAt = now.UTC()
	return next, nil
}

// ReviewPolicyStatusEvent is the activity event of one row write.
func ReviewPolicyStatusEvent(row core.ReviewPolicyStatus) core.Event {
	return core.Event{TaskID: row.TaskID, Kind: core.ReviewPolicyStatusEventKind(row.State), Payload: core.JSONPayload(row)}
}

// PrepareMergeBypass validates the bypass a merge-completing command records
// in its own transaction (req-risk-based-review AC-10.3;
// component-submission-merge invariant 66). Only a command that completes an
// observed merge carries one, only an enforcing task records one, and the
// command credits no approval with it.
func PrepareMergeBypass(task core.Task, command core.TaskCommand, bypass core.MergeBypass, final *core.TaskApproval) (core.Event, error) {
	switch command {
	case core.TaskMergeConfirm, core.TaskMergeRecover:
	default:
		return core.Event{}, Refusef("command %s does not complete an observed merge and records no bypass", command)
	}
	if final != nil {
		return core.Event{}, Refusef("a merge that bypassed the risk requirements credits no approval")
	}
	if !task.SetupContract.Risk.Enforcing() {
		return core.Event{}, Refusef("the task's frozen risk mode does not enforce, so it records no bypass")
	}
	if err := bypass.Validate(); err != nil {
		return core.Event{}, Refuse(err)
	}
	return core.Event{TaskID: task.ID, Kind: core.MergeBypassRecordedEvent, Payload: core.JSONPayload(bypass)}, nil
}

func (m *memory) QueueReviewPolicyStatus(ctx context.Context, taskID, head, desired string) (core.ReviewPolicyStatus, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	task, key, err := m.riskTaskLocked(ctx, taskID)
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	var existing *core.ReviewPolicyStatus
	if row, ok := m.reviewPolicyStatuses[key][strings.TrimSpace(head)]; ok {
		existing = &row
	}
	row, write, err := PlanReviewPolicyStatusQueue(task, existing, head, desired, time.Now())
	if err != nil || !write {
		return row, false, err
	}
	if m.reviewPolicyStatuses == nil {
		m.reviewPolicyStatuses = map[memoryScopedKey]map[string]core.ReviewPolicyStatus{}
	}
	if m.reviewPolicyStatuses[key] == nil {
		m.reviewPolicyStatuses[key] = map[string]core.ReviewPolicyStatus{}
	}
	m.reviewPolicyStatuses[key][row.HeadSHA] = row
	m.appendEventLocked(ctx, ReviewPolicyStatusEvent(row))
	return row, true, nil
}

func (m *memory) GetReviewPolicyStatus(ctx context.Context, taskID, head string) (core.ReviewPolicyStatus, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, key, err := m.riskTaskLocked(ctx, taskID)
	if err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	row, ok := m.reviewPolicyStatuses[key][head]
	return row, ok, nil
}

func (m *memory) ListReviewPolicyStatuses(ctx context.Context, taskID string) ([]core.ReviewPolicyStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, key, err := m.riskTaskLocked(ctx, taskID)
	if err != nil {
		return nil, err
	}
	rows := []core.ReviewPolicyStatus{}
	for _, row := range m.reviewPolicyStatuses[key] {
		rows = append(rows, row)
	}
	SortReviewPolicyStatuses(rows)
	return rows, nil
}

func (m *memory) UpdateReviewPolicyStatus(ctx context.Context, status core.ReviewPolicyStatus) (core.ReviewPolicyStatus, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, key, err := m.riskTaskLocked(ctx, status.TaskID)
	if err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	current, ok := m.reviewPolicyStatuses[key][status.HeadSHA]
	if !ok {
		return core.ReviewPolicyStatus{}, fmt.Errorf("%w: policy status of task %s head %s", ErrNotFound, status.TaskID, status.HeadSHA)
	}
	next, err := PrepareReviewPolicyStatusUpdate(current, status, time.Now())
	if err != nil {
		return core.ReviewPolicyStatus{}, err
	}
	m.reviewPolicyStatuses[key][next.HeadSHA] = next
	m.appendEventLocked(ctx, ReviewPolicyStatusEvent(next))
	return next, nil
}

func (m *memory) ListOpenReviewPolicyStatuses(ctx context.Context) ([]core.ReviewPolicyStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	workspace, _ := WorkspaceFromContext(ctx)
	rows := []core.ReviewPolicyStatus{}
	for key, heads := range m.reviewPolicyStatuses {
		if key.workspace != workspace {
			continue
		}
		for _, row := range heads {
			if row.State != core.ReviewPolicyStatusPublished {
				rows = append(rows, row)
			}
		}
	}
	SortReviewPolicyStatuses(rows)
	return rows, nil
}

// SortReviewPolicyStatuses orders rows by creation time, then task and head.
func SortReviewPolicyStatuses(rows []core.ReviewPolicyStatus) {
	slices.SortStableFunc(rows, func(a, b core.ReviewPolicyStatus) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		if c := strings.Compare(a.TaskID, b.TaskID); c != 0 {
			return c
		}
		return strings.Compare(a.HeadSHA, b.HeadSHA)
	})
}

// QueueReviewPolicyStatus on the test backend also enqueues the head's job on
// its log, as the relational backends do in their transaction, so a fixture
// can run the real queue runtime without a database.
func (m *volatileMemory) QueueReviewPolicyStatus(ctx context.Context, taskID, head, desired string) (core.ReviewPolicyStatus, bool, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	row, wrote, err := m.memory.QueueReviewPolicyStatus(ctx, taskID, head, desired)
	if err != nil || !wrote {
		return row, wrote, err
	}
	workspace, _ := WorkspaceFromContext(ctx)
	args := queue.ReviewPolicyStatusArgs{WorkspaceID: workspace, TaskID: row.TaskID, HeadSHA: row.HeadSHA}
	if _, err := logqueue.Enqueue(ctx, m.log, workspace, args.Kind(), args.UniqueKey(), args, 5, time.Now().UTC()); err != nil {
		return core.ReviewPolicyStatus{}, false, err
	}
	return row, true, nil
}

// reopenReviewPolicyStatusesLocked reopens every published or failed row of
// an enforcing task in the raise that m.mu guards, appending each row's
// queued event. It returns the reopened rows.
func (m *memory) reopenReviewPolicyStatusesLocked(ctx context.Context, key memoryScopedKey, task core.Task) []core.ReviewPolicyStatus {
	if !task.SetupContract.Risk.Enforcing() {
		return nil
	}
	var reopened []core.ReviewPolicyStatus
	now := time.Now()
	for head, row := range m.reviewPolicyStatuses[key] {
		next, ok := PlanReviewPolicyStatusReopen(row, now)
		if !ok {
			continue
		}
		m.reviewPolicyStatuses[key][head] = next
		m.appendEventLocked(ctx, ReviewPolicyStatusEvent(next))
		reopened = append(reopened, next)
	}
	SortReviewPolicyStatuses(reopened)
	return reopened
}

// RaiseTaskRiskTier on the test backend also enqueues the job of every policy
// status row the raise reopened, as the relational backends do in their
// transaction.
func (m *volatileMemory) RaiseTaskRiskTier(ctx context.Context, raise core.RiskRating, seats RiskSeatSettings) (core.RiskRating, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.RiskRating{}, err
	}
	raised, err := m.memory.RaiseTaskRiskTier(ctx, raise, seats)
	if err != nil {
		return raised, err
	}
	rows, err := m.memory.ListReviewPolicyStatuses(ctx, raise.TaskID)
	if err != nil {
		return core.RiskRating{}, err
	}
	workspace, _ := WorkspaceFromContext(ctx)
	for _, row := range rows {
		if row.State != core.ReviewPolicyStatusQueued {
			continue
		}
		args := queue.ReviewPolicyStatusArgs{WorkspaceID: workspace, TaskID: row.TaskID, HeadSHA: row.HeadSHA}
		if _, err := logqueue.Enqueue(ctx, m.log, workspace, args.Kind(), args.UniqueKey(), args, 5, time.Now().UTC()); err != nil {
			return core.RiskRating{}, err
		}
	}
	return raised, nil
}
