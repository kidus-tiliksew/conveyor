package store

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestEventDeadlineNotice covers each event row of component-persistence,
// Deadlines: the notice an inserted event raises and its time.
func TestEventDeadlineNotice(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	lease, queue, execution, recovery := at.Add(5*time.Minute), at.Add(30*time.Minute), at.Add(4*time.Hour), at.Add(time.Minute)
	order := func(mutate func(*core.WorkOrder)) []byte {
		o := core.WorkOrder{ID: "o", TaskID: "t", Stage: core.StageImplement, QueueDeadline: queue}
		mutate(&o)
		return WorkOrderEventPayload(o)
	}
	cases := []struct {
		name    string
		kind    string
		payload []byte
		want    time.Time
		none    bool
	}{
		{name: "queued order raises its queue deadline", kind: "work_order.created", payload: order(func(o *core.WorkOrder) { o.State = core.WorkOrderQueued }), want: queue},
		{name: "blocked queued order raises nothing", kind: "work_order.updated", payload: order(func(o *core.WorkOrder) { o.State, o.QueueBlockedAt = core.WorkOrderQueued, at }), none: true},
		{name: "claimed order raises the earlier of lease and execution deadline", kind: "work_order.claimed", payload: order(func(o *core.WorkOrder) {
			o.State, o.LeaseExpiresAt, o.ExecutionStartedAt, o.ExecutionDeadline = core.WorkOrderClaimed, lease, at, execution
		}), want: lease},
		{name: "stale order without a recovery time is due at once", kind: "work_order.stale", payload: order(func(o *core.WorkOrder) { o.State = core.WorkOrderStale }), want: at},
		{name: "timed-out order with a recovery time raises it", kind: "work_order.recovery_scheduled", payload: order(func(o *core.WorkOrder) {
			o.State, o.NextAutomaticRecoveryAt = core.WorkOrderTimedOut, recovery
		}), want: recovery},
		{name: "expired queued order without a recovery time is due at once", kind: "work_order.expired", payload: order(func(o *core.WorkOrder) {
			o.State, o.LastAttemptOutcome, o.ExecutionStartedAt = core.WorkOrderQueued, core.WorkOrderOutcomeExpired, at
		}), want: at},
		{name: "completed order raises nothing", kind: "work_order.updated", payload: order(func(o *core.WorkOrder) { o.State = core.WorkOrderCompleted }), none: true},
		{name: "cancelled order raises nothing", kind: "work_order.cancelled", payload: order(func(o *core.WorkOrder) { o.State = core.WorkOrderCancelled }), none: true},
		{name: "renewal raises its lease", kind: "work_order.lease_renewed", payload: core.JSONPayload(map[string]any{"attempt_id": "a", "lease_expires_at": lease}), want: lease},
		{name: "progress raises nothing", kind: "work_order.progress_reported", payload: core.JSONPayload(map[string]any{"message": "working"}), none: true},
		{name: "review publication failure is due at once", kind: "review.publication_failed", payload: []byte(`{}`), want: at},
		{name: "GitHub issue publication failure is due at once", kind: "github_issue.publication_failed", payload: []byte(`{}`), want: at},
		{name: "configuration update is due at once", kind: "config.updated", payload: []byte(`{}`), want: at},
		{name: "requirement confirmation is due at once", kind: "requirement.version_confirmed", payload: []byte(`{}`), want: at},
		{name: "System Design confirmation is due at once", kind: "system_design.version_confirmed", payload: []byte(`{}`), want: at},
		{name: "an unrelated event raises nothing", kind: "task.state_changed", payload: []byte(`{}`), none: true},
		{name: "a review publication retry raises nothing", kind: "review.publication_retry", payload: []byte(`{}`), none: true},
		// A verify order that loses its claim is due at once, so the next fire
		// reconciles its verification attempt's claim loss.
		{name: "released verify order is due at once", kind: "work_order.released", payload: order(func(o *core.WorkOrder) {
			o.Stage, o.State, o.LastAttemptID = core.StageVerify, core.WorkOrderQueued, "attempt-1"
		}), want: at},
		{name: "cancelled verify order is due at once", kind: "work_order.cancelled", payload: order(func(o *core.WorkOrder) {
			o.Stage, o.State, o.LastAttemptID = core.StageVerify, core.WorkOrderCancelled, "attempt-1"
		}), want: at},
		{name: "preempted verify order is due at once", kind: "work_order.preempted", payload: order(func(o *core.WorkOrder) {
			o.Stage, o.State, o.LastAttemptID = core.StageVerify, core.WorkOrderQueued, "attempt-1"
		}), want: at},
		{name: "superseded verification cancel without a stage is due at once", kind: "work_order.cancelled", payload: core.JSONPayload(map[string]any{"work_order_id": "o", "reason": "superseded verification head"}), want: at},
		{name: "a new verify order without an attempt keeps its queue deadline", kind: "work_order.created", payload: order(func(o *core.WorkOrder) { o.Stage, o.State = core.StageVerify, core.WorkOrderQueued }), want: queue},
		{name: "released implement order keeps its queue deadline", kind: "work_order.released", payload: order(func(o *core.WorkOrder) { o.State = core.WorkOrderQueued }), want: queue},
		{name: "a claimed verify order keeps its lease", kind: "work_order.claimed", payload: order(func(o *core.WorkOrder) {
			o.Stage, o.State, o.LeaseExpiresAt, o.ExecutionStartedAt, o.ExecutionDeadline = core.StageVerify, core.WorkOrderClaimed, lease, at, execution
		}), want: lease},
		{name: "a staged chunk raises its upload expiry", kind: "verification.chunk.stage", payload: []byte(`{}`), want: at.Add(VerificationUploadLifetime)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := EventDeadlineNotice("ws", tc.kind, tc.payload, at)
			if tc.none {
				if ok {
					t.Fatalf("notice=%+v, want none", n)
				}
				return
			}
			if !ok || n.Kind != changes.Deadline || n.Workspace != "ws" || !n.At.Equal(tc.want) {
				t.Fatalf("notice=%+v ok=%t, want deadline at %s", n, ok, tc.want)
			}
		})
	}
}

// TestNextDeadlinePublications covers the volatile backend's publication and
// verification-delivery rows: a failed row's next automatic re-enqueue one
// window after the last, none at bound 0, and a failed delivery's
// next_attempt_at.
func TestNextDeadlinePublications(t *testing.T) {
	st := NewMemory().(*memory)
	ctx := WithWorkspace(WithActor(context.Background(), SystemActor()), "ws")
	now := time.Now().UTC().Truncate(time.Second)
	window := config.DefaultRecoveryPolicy().Window
	if st.verificationDeliveries == nil {
		st.verificationDeliveries = map[string]core.VerificationDelivery{}
	}
	st.tasks["issue"] = core.Task{ID: "issue", Workspace: "ws", State: core.TaskRunning}
	st.tasks["review"] = core.Task{ID: "review", Workspace: "ws", State: core.TaskRunning}
	st.tasks["other"] = core.Task{ID: "other", Workspace: "other", State: core.TaskRunning}
	st.github["issue"] = core.GitHubLifecycle{TaskID: "issue", State: core.GitHubPublicationFailed, AutomaticReenqueueAt: now.Add(-window + 3*time.Hour)}
	st.publications["review-1"] = core.ReviewPublication{ReviewWorkOrderID: "review-1", TaskID: "review", State: core.ReviewPublicationFailed, AutomaticReenqueueAt: now.Add(-window + 2*time.Hour)}
	// Never re-enqueued: due at once, so no stored time.
	st.publications["review-2"] = core.ReviewPublication{ReviewWorkOrderID: "review-2", TaskID: "review", State: core.ReviewPublicationFailed}
	// Published rows and other workspaces are never read.
	st.publications["review-3"] = core.ReviewPublication{ReviewWorkOrderID: "review-3", TaskID: "review", State: core.ReviewPublicationPublished, AutomaticReenqueueAt: now.Add(-window + time.Minute)}
	st.publications["other-1"] = core.ReviewPublication{ReviewWorkOrderID: "other-1", TaskID: "other", State: core.ReviewPublicationFailed, AutomaticReenqueueAt: now.Add(-window + time.Minute)}
	retry := now.Add(time.Hour)
	st.verificationDeliveries["delivery"] = core.VerificationDelivery{ID: "delivery", WorkspaceID: "ws", State: "failed", NextAttemptAt: &retry}
	otherRetry := now.Add(time.Minute)
	st.verificationDeliveries["other-delivery"] = core.VerificationDelivery{ID: "other-delivery", WorkspaceID: "other", State: "failed", NextAttemptAt: &otherRetry}

	expect := func(after, want time.Time) {
		t.Helper()
		next, ok, err := st.NextDeadline(ctx, after)
		if err != nil || !ok || !next.Equal(want) {
			t.Fatalf("NextDeadline(%s)=%s ok=%t err=%v, want %s", after, next, ok, err, want)
		}
	}
	expect(now, retry)
	expect(retry, now.Add(2*time.Hour))
	expect(now.Add(2*time.Hour), now.Add(3*time.Hour))
	if next, ok, err := st.NextDeadline(ctx, now.Add(3*time.Hour)); err != nil || ok {
		t.Fatalf("NextDeadline after every row=%s ok=%t err=%v", next, ok, err)
	}

	// At bound 0 a failed publication is never re-enqueued.
	st.readClockPolicy = func(string) (config.RecoveryPolicy, time.Duration, error) {
		policy := config.DefaultRecoveryPolicy()
		policy.AutomaticRecoveries = 0
		return policy, config.DefaultWorkOrderQueueTimeout, nil
	}
	if next, ok, err := st.NextDeadline(ctx, retry); err != nil || ok {
		t.Fatalf("NextDeadline at bound 0=%s ok=%t err=%v", next, ok, err)
	}
}
