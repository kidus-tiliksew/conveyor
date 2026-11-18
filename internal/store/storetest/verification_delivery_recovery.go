package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runVerificationDeliveryRecovery proves the reconcile pass's retry, discard,
// supersede, lock and window rules on every backend
// (component-verification-publication invariants 5, 12, 19, 25, 26 and 27;
// req-bounded-automatic-recovery AC-4.1).
func runVerificationDeliveryRecovery(t *testing.T, x Fixture) {
	t.Run("DeliveryRetryAfterDiscard", func(t *testing.T) { runDeliveryRetryAfterDiscard(t, x) })
	t.Run("DeliveryDiscardedRetryingFails", func(t *testing.T) { runDeliveryDiscardedRetryingFails(t, x) })
	t.Run("DeliveryOlderDiscardKeepsNewerRow", func(t *testing.T) { runDeliveryOlderDiscardKeepsNewerRow(t, x) })
	t.Run("DeliverySupersedesOlderGenerations", func(t *testing.T) { runDeliverySupersedesOlderGenerations(t, x) })
	t.Run("DeliveryPassSkipsHeldKey", func(t *testing.T) { runDeliveryPassSkipsHeldKey(t, x) })
	t.Run("DeliveryReadFields", func(t *testing.T) { runDeliveryReadFields(t, x) })
	t.Run("DeliveryWindowPastActiveJobs", func(t *testing.T) { runDeliveryWindowPastActiveJobs(t, x) })
}

// deliveryCase is one task with evidence and one pull request, so planning
// has written one pending delivery and enqueued its job.
type deliveryCase struct {
	x      Fixture
	v      verificationFixture
	args   queue.VerificationPublicationArgs
	stream eventlog.StreamID
}

func newDeliveryCase(t *testing.T, x Fixture, repository string, number int) deliveryCase {
	t.Helper()
	v := newVerificationFixture(t, x)
	name := fmt.Sprintf("recovery-%s-%d", strings.ReplaceAll(repository, "/", "-"), number)
	e := v.envelope(name, name+"-key", "observed state")
	v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
	return addDeliveryCase(t, x, v, repository, number)
}

// addDeliveryCase records one more pull request on v's task, which holds
// evidence, so planning writes that pull request's first delivery.
func addDeliveryCase(t *testing.T, x Fixture, v verificationFixture, repository string, number int) deliveryCase {
	t.Helper()
	pr := core.Event{TaskID: v.access.TaskID, JobID: v.access.WorkOrderID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"repository": repository, "number": number, "head_sha": strings.Repeat("a", 40)})}
	requireOK(t, x.Backend.RecordVerificationPullRequest(v.ctx, pr))
	args := queue.VerificationPublicationArgs{WorkspaceID: x.Workspace, Repository: repository, PullRequestNumber: number}
	c := deliveryCase{x: x, v: v, args: args, stream: logqueue.StreamFor(args.Kind(), args.UniqueKey())}
	if d := c.read(t); d.State != "pending" || d.Generation != 1 {
		t.Fatalf("planning wrote %+v, want a pending first generation", d)
	}
	return c
}

// read returns the pull request's newest delivery as the handler sees it.
func (c deliveryCase) read(t *testing.T) core.VerificationDelivery {
	t.Helper()
	var current core.VerificationDelivery
	requireOK(t, c.x.Backend.RunVerificationDelivery(c.v.ctx, c.args, func(d *core.VerificationDelivery, _ func(string, core.VerificationDelivery) error) error {
		current = *d
		return nil
	}))
	return current
}

func (c deliveryCase) job(t *testing.T) logqueue.Job {
	t.Helper()
	job, err := logqueue.Load(c.v.ctx, c.x.Backend.Log(), c.x.Workspace, c.stream)
	requireOK(t, err)
	return job
}

// reconcile runs one pass at now and returns the drift it reported for this
// case's task.
func (c deliveryCase) reconcile(t *testing.T, now time.Time) (error, []store.DriftRepair) {
	t.Helper()
	var mu sync.Mutex
	var repairs []store.DriftRepair
	ctx := store.WithDriftRecorder(store.WithVerificationDeliveryClock(c.v.ctx, func() time.Time { return now }), func(r store.DriftRepair) {
		if r.TaskID == c.v.access.TaskID {
			mu.Lock()
			repairs = append(repairs, r)
			mu.Unlock()
		}
	})
	err := c.x.Backend.ReconcileVerificationDeliveries(ctx)
	return err, repairs
}

func driftKinds(repairs []store.DriftRepair) []string {
	var out []string
	for _, r := range repairs {
		out = append(out, r.Mismatch)
	}
	return out
}

// runDeliveryQueue runs the queue runtime on the backend's log until done
// reports true. The runtime claims, retries and discards jobs as the daemon's
// does. handle serves the delivery jobs whose keys serve names; every other
// delivery job in the shared workspace is snoozed, so a case never acts on
// another case's rows. A nil discarded records the discard with no handler.
func runDeliveryQueue(t *testing.T, x Fixture, serve map[string]bool, handle func(context.Context, queue.VerificationPublicationArgs) error, discarded func(context.Context, queue.VerificationPublicationArgs) error, done func() bool) {
	t.Helper()
	rt := logqueue.NewRuntime(x.Backend.Log(), logqueue.Options{Workspaces: []string{x.Workspace}, WorkerID: "storetest-delivery", Logf: t.Logf})
	decode := func(ctx context.Context, job queue.Job) (context.Context, queue.VerificationPublicationArgs, bool) {
		var a queue.VerificationPublicationArgs
		if json.Unmarshal(job.Args, &a) != nil || !serve[a.UniqueKey()] {
			return ctx, a, false
		}
		return store.WithWorkspace(store.WithActor(ctx, store.SystemActor("verification-publication")), job.WorkspaceID), a, true
	}
	registration := queue.Registration{
		Kind:       queue.VerificationPublicationArgs{}.Kind(),
		RetryDelay: func(int) time.Duration { return time.Millisecond },
		Handle: func(ctx context.Context, job queue.Job) error {
			ctx, a, ok := decode(ctx, job)
			if !ok {
				return queue.Snooze(time.Hour)
			}
			return handle(ctx, a)
		},
	}
	if discarded != nil {
		registration.Discarded = func(ctx context.Context, job queue.Job, _ string) error {
			ctx, a, ok := decode(ctx, job)
			if !ok {
				return nil
			}
			return discarded(ctx, a)
		}
	}
	rt.Register(registration)
	requireOK(t, rt.Start(context.Background()))
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		requireOK(t, rt.Stop(stop))
	}()
	deadline := time.Now().Add(90 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the queue did not reach the expected job state")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// forgeFailure is the handler's failed attempt (invariant 18): it counts an
// attempt, saves retry_error, and on the fifth attempt of the cycle saves fail
// with a retry 15 minutes ahead; either way the job gets an error.
func forgeFailure(x Fixture) func(context.Context, queue.VerificationPublicationArgs) error {
	return func(ctx context.Context, a queue.VerificationPublicationArgs) error {
		failed := false
		err := x.Backend.RunVerificationDelivery(ctx, a, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			if d.State == "published" || d.State == "failed" || d.State == "superseded" {
				return nil
			}
			if err := save("attempt", *d); err != nil {
				return err
			}
			next := *d
			next.ErrorClass = "forge"
			at := time.Now().UTC().Add(time.Second)
			next.NextAttemptAt = &at
			command := "retry_error"
			if d.CycleAttempts >= 5 {
				command = "fail"
				retryAt := time.Now().UTC().Add(15 * time.Minute)
				next.NextAttemptAt = &retryAt
			}
			failed = true
			return save(command, next)
		})
		if err != nil {
			return err
		}
		if failed {
			return errors.New("verification publication forge")
		}
		return nil
	}
}

// failDiscarded is the handler's discard rule: only a pending or retrying row
// fails, so a row the last attempt saved as failed keeps its retry time.
func failDiscarded(x Fixture) func(context.Context, queue.VerificationPublicationArgs) error {
	return func(ctx context.Context, a queue.VerificationPublicationArgs) error {
		return x.Backend.RunVerificationDelivery(ctx, a, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			if d.State != "pending" && d.State != "retrying" {
				return nil
			}
			if d.State == "pending" {
				if err := save("attempt", *d); err != nil {
					return err
				}
			}
			next := *d
			next.ErrorClass = "forge"
			next.NextAttemptAt = nil
			return save("fail", next)
		})
	}
}

// crashAfterAttempt is a handler that records its attempt and then stops
// without a failure outcome, as a process that died mid-delivery does.
func crashAfterAttempt(x Fixture) func(context.Context, queue.VerificationPublicationArgs) error {
	return func(ctx context.Context, a queue.VerificationPublicationArgs) error {
		err := x.Backend.RunVerificationDelivery(ctx, a, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			if d.State != "pending" && d.State != "retrying" {
				return nil
			}
			return save("attempt", *d)
		})
		if err != nil {
			return err
		}
		// The queue records the outcome after this returns; the pause keeps
		// that time after the attempt's at the microsecond the SQL logs keep.
		time.Sleep(time.Millisecond)
		return errors.New("handler stopped after its attempt")
	}
}

func (c deliveryCase) discardedOnce(t *testing.T) func() bool {
	return func() bool {
		job, err := logqueue.Load(c.v.ctx, c.x.Backend.Log(), c.x.Workspace, c.stream)
		return err == nil && job.State == logqueue.StateDiscarded
	}
}

// runDeliveryRetryAfterDiscard proves a row the fifth failed attempt saved as
// failed gets a fresh job when its retry time passes, although the queue
// discarded the cycle's job: the row turns pending, keeps its error class, and
// no discard drift is reported (invariant 26; req-bounded-automatic-recovery
// AC-4.1).
func runDeliveryRetryAfterDiscard(t *testing.T, x Fixture) {
	c := newDeliveryCase(t, x, "fixture/recovery", 4601)
	runDeliveryQueue(t, x, map[string]bool{c.args.UniqueKey(): true}, forgeFailure(x), failDiscarded(x), c.discardedOnce(t))
	exhausted := c.read(t)
	if exhausted.State != "failed" || exhausted.ErrorClass != "forge" || exhausted.CycleAttempts != 5 || exhausted.NextAttemptAt == nil {
		t.Fatalf("after five failed attempts: %+v", exhausted)
	}
	before := c.job(t)
	if before.State != logqueue.StateDiscarded || before.Attempt < before.MaxAttempts {
		t.Fatalf("queue job after five failed attempts: %+v", before)
	}
	// Before the retry time nothing changes.
	err, repairs := c.reconcile(t, exhausted.NextAttemptAt.Add(-time.Minute))
	requireOK(t, err)
	if d := c.read(t); d.State != "failed" || len(repairs) != 0 || c.job(t).Generation != before.Generation {
		t.Fatalf("a pass before the retry time changed the row: %+v repairs %v", d, driftKinds(repairs))
	}
	err, repairs = c.reconcile(t, exhausted.NextAttemptAt.Add(time.Minute))
	requireOK(t, err)
	retried := c.read(t)
	if retried.State != "pending" || retried.ErrorClass != "forge" || retried.ErrorMessage == "" || retried.CycleAttempts != 0 || retried.Attempts != 5 || retried.NextAttemptAt != nil {
		t.Fatalf("due failed row after the pass: %+v", retried)
	}
	after := c.job(t)
	if !after.Active() || after.Generation != before.Generation+1 {
		t.Fatalf("due failed row got no fresh job: before %+v after %+v", before, after)
	}
	for _, r := range repairs {
		if r.Mismatch == store.DriftVerificationDeliveryDiscarded.Mismatch {
			t.Fatalf("a due retry reported discard drift: %v", driftKinds(repairs))
		}
	}
	// A later pass leaves the served row and its job alone.
	err, _ = c.reconcile(t, exhausted.NextAttemptAt.Add(2*time.Minute))
	requireOK(t, err)
	if again := c.job(t); again.Generation != after.Generation || c.read(t).State != "pending" {
		t.Fatalf("a second pass enqueued again: %+v", again)
	}
}

// runDeliveryDiscardedRetryingFails proves a row left retrying when the queue
// discarded its job after the final attempt becomes failed with class forge
// and no retry time, with discard drift (invariant 26).
func runDeliveryDiscardedRetryingFails(t *testing.T, x Fixture) {
	c := newDeliveryCase(t, x, "fixture/recovery", 4602)
	runDeliveryQueue(t, x, map[string]bool{c.args.UniqueKey(): true}, crashAfterAttempt(x), nil, c.discardedOnce(t))
	stopped := c.read(t)
	if stopped.State != "retrying" || stopped.Attempts != 5 {
		t.Fatalf("after five stopped attempts: %+v", stopped)
	}
	job := c.job(t)
	err, repairs := c.reconcile(t, time.Now().UTC())
	requireOK(t, err)
	failed := c.read(t)
	if failed.State != "failed" || failed.ErrorClass != "forge" || failed.NextAttemptAt != nil {
		t.Fatalf("discarded retrying row: %+v", failed)
	}
	if kinds := driftKinds(repairs); len(kinds) != 1 || kinds[0] != store.DriftVerificationDeliveryDiscarded.Mismatch {
		t.Fatalf("drift for a discarded retrying row: %v", kinds)
	}
	if after := c.job(t); after.Generation != job.Generation || after.State != logqueue.StateDiscarded {
		t.Fatalf("a failed row got a job: %+v", after)
	}
}

// runDeliveryOlderDiscardKeepsNewerRow proves a discard recorded before the
// row's last change fails nothing: a worker the queue had already given up
// on records another attempt after the discard, and the pass gives the
// retrying row a fresh job instead (invariant 26).
func runDeliveryOlderDiscardKeepsNewerRow(t *testing.T, x Fixture) {
	c := newDeliveryCase(t, x, "fixture/recovery", 4603)
	runDeliveryQueue(t, x, map[string]bool{c.args.UniqueKey(): true}, crashAfterAttempt(x), nil, c.discardedOnce(t))
	job := c.job(t)
	time.Sleep(2 * time.Millisecond)
	stale := crashAfterAttempt(x)(store.WithWorkspace(store.WithActor(c.v.ctx, store.SystemActor("verification-publication")), x.Workspace), c.args)
	if stale == nil {
		t.Fatal("the late worker reported success")
	}
	late := c.read(t)
	if late.State != "retrying" || late.Attempts != 6 || !late.UpdatedAt.After(job.DiscardedAt) {
		t.Fatalf("late attempt: %+v discarded at %s", late, job.DiscardedAt)
	}
	err, repairs := c.reconcile(t, time.Now().UTC())
	requireOK(t, err)
	kept := c.read(t)
	if kept.State != "retrying" || kept.ErrorClass != "" || kept.Attempts != 6 {
		t.Fatalf("an older discard changed a newer row: %+v", kept)
	}
	if after := c.job(t); !after.Active() || after.Generation != job.Generation+1 {
		t.Fatalf("retrying row without a job got none: %+v", after)
	}
	if kinds := driftKinds(repairs); len(kinds) != 1 || kinds[0] != store.DriftVerificationDeliveryWithoutJob.Mismatch {
		t.Fatalf("drift for a retrying row without a job: %v", kinds)
	}
}

// runDeliverySupersedesOlderGenerations proves the pass marks superseded every
// older pending, retrying and failed generation, with or without a retry
// time, and clears its retry time; the newest generation keeps its one job,
// and a later pass selects none of the settled rows (invariants 5, 19 and 27).
// No command leaves an older generation unsuperseded, so the fixture restores
// historical rows as releases before the supersede rule left them.
func runDeliverySupersedesOlderGenerations(t *testing.T, x Fixture) {
	if x.RestoreHistoricalDelivery == nil {
		t.Fatal("fixture cannot restore a historical delivery generation")
	}
	c := newDeliveryCase(t, x, "fixture/history", 4611)
	for index := range 4 {
		head := strings.Repeat(string(rune('b'+index)), 40)
		name := fmt.Sprintf("history-%d", index)
		e := c.v.envelope(name, name+"-key", "observed state")
		c.v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}, Publication: &store.VerificationPublication{PullRequestNumber: c.args.PullRequestNumber, HeadSHA: head, BodyDigest: verificationSHA([]byte(name))}})
	}
	newest := c.read(t)
	if newest.State != "pending" || newest.Generation < 5 {
		t.Fatalf("newest generation: %+v", newest)
	}
	past := time.Now().UTC().Add(-time.Hour)
	x.RestoreHistoricalDelivery(t, c.v.ctx, c.v.access.TaskID, 1, "pending", "", nil)
	x.RestoreHistoricalDelivery(t, c.v.ctx, c.v.access.TaskID, 2, "retrying", "forge", nil)
	x.RestoreHistoricalDelivery(t, c.v.ctx, c.v.access.TaskID, 3, "failed", "forge", &past)
	x.RestoreHistoricalDelivery(t, c.v.ctx, c.v.access.TaskID, 4, "failed", "head_mismatch", nil)
	job := c.job(t)
	if !job.Active() {
		t.Fatalf("newest generation has no job: %+v", job)
	}
	err, repairs := c.reconcile(t, time.Now().UTC())
	requireOK(t, err)
	if len(repairs) != 0 {
		t.Fatalf("settling older generations reported drift: %v", driftKinds(repairs))
	}
	for _, p := range c.v.snapshot(t).Publications {
		d := p.Delivery
		if d == nil || d.Repository != c.args.Repository || d.PullRequestNumber != c.args.PullRequestNumber {
			continue
		}
		switch {
		case d.Generation == newest.Generation:
			if d.State != "pending" {
				t.Fatalf("newest generation after the pass: %+v", d)
			}
		case d.State != "superseded" || d.NextAttemptAt != nil:
			t.Fatalf("older generation %d after the pass: %+v", d.Generation, d)
		}
	}
	if after := c.job(t); after.Generation != job.Generation || !after.Active() {
		t.Fatalf("settling older generations enqueued again: before %+v after %+v", job, after)
	}
	probe, stop := probeVerificationReads(t)
	defer stop()
	err, _ = c.reconcile(t, time.Now().UTC())
	requireOK(t, err)
	if due := probe.rows(x.Workspace, store.VerificationReadDue); due != 0 {
		t.Fatalf("a pass after settling selected %d due rows, want none", due)
	}
}

// runDeliveryPassSkipsHeldKey proves a pass never waits on a delivery lock:
// while another transaction holds one pull request's lock, the pass settles
// every other due key, lets an enqueue on an unrelated stream through, and
// reports the busy key, which a later pass settles (invariant 12).
func runDeliveryPassSkipsHeldKey(t *testing.T, x Fixture) {
	if !x.DeliveryPassSkipsHeldKeys {
		t.Skip("the memory store serializes every delivery command on one mutex")
	}
	held := newDeliveryCase(t, x, "fixture/held", 4621)
	free := newDeliveryCase(t, x, "fixture/held", 4622)
	fail := func(c deliveryCase) time.Time {
		t.Helper()
		past := time.Now().UTC().Add(-time.Minute)
		requireOK(t, x.Backend.RunVerificationDelivery(c.v.ctx, c.args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			if err := save("attempt", *d); err != nil {
				return err
			}
			next := *d
			next.ErrorClass = "forge"
			next.NextAttemptAt = &past
			return save("fail", next)
		}))
		return past
	}
	fail(held)
	fail(free)
	locked := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- x.Backend.RunVerificationDelivery(held.v.ctx, held.args, func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error {
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-holder:
		t.Fatalf("holder ended before it locked: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("holder did not take the lock")
	}
	released := false
	defer func() {
		if !released {
			close(release)
			<-holder
		}
	}()
	freeJob := free.job(t)
	pass := make(chan error, 1)
	enqueued := make(chan error, 1)
	go func() {
		err, _ := held.reconcile(t, time.Now().UTC())
		pass <- err
	}()
	go func() {
		_, err := logqueue.Enqueue(held.v.ctx, x.Backend.Log(), x.Workspace, "storetest_unrelated", held.v.access.TaskID, map[string]string{"probe": "unrelated"}, 1, time.Now().UTC())
		enqueued <- err
	}()
	for _, ch := range []chan error{pass, enqueued} {
		select {
		case err := <-ch:
			if ch == pass && !errors.Is(err, store.ErrVerificationDeliveryBusy) {
				t.Fatalf("pass with a held key returned %v, want the busy error", err)
			}
			if ch == enqueued {
				requireOK(t, err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the pass or an unrelated enqueue waited on the held delivery lock")
		}
	}
	// The planning job still serves the free key, so its retry adds no second
	// job.
	if d := free.read(t); d.State != "pending" {
		t.Fatalf("free key after the pass: %+v", d)
	}
	if job := free.job(t); !job.Active() || job.Generation != freeJob.Generation {
		t.Fatalf("free key job after the pass: before %+v after %+v", freeJob, job)
	}
	heldJob := held.job(t)
	close(release)
	released = true
	requireOK(t, <-holder)
	if d := held.read(t); d.State != "failed" {
		t.Fatalf("held key changed while its lock was held: %+v", d)
	}
	err, _ := held.reconcile(t, time.Now().UTC())
	requireOK(t, err)
	if d := held.read(t); d.State != "pending" {
		t.Fatalf("a later pass did not settle the skipped key: %+v", d)
	}
	if job := held.job(t); !job.Active() || job.Generation != heldJob.Generation {
		t.Fatalf("skipped key job after the later pass: before %+v after %+v", heldJob, job)
	}
}

// runDeliveryReadFields proves a read's Delivery equals the stored row without
// its summary, including the error message and both times, and the
// publication page carries the failure's message and time, on every backend
// (invariants 24 and 25).
func runDeliveryReadFields(t *testing.T, x Fixture) {
	c := newDeliveryCase(t, x, "fixture/fields", 4631)
	retryAt := time.Now().UTC().Add(time.Hour)
	requireOK(t, x.Backend.RunVerificationDelivery(c.v.ctx, c.args, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
		if err := save("attempt", *d); err != nil {
			return err
		}
		next := *d
		next.ErrorClass = "forge"
		next.ObservedHead = strings.Repeat("c", 40)
		next.NextAttemptAt = &retryAt
		return save("fail", next)
	}))
	stored := c.read(t)
	if stored.Summary == "" || stored.ErrorMessage == "" || stored.UpdatedAt.IsZero() || stored.CreatedAt.IsZero() {
		t.Fatalf("stored delivery lacks the fields under test: %+v", stored)
	}
	want := stored
	want.Summary = ""
	normal := func(d core.VerificationDelivery) string {
		d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
		if d.LastAttemptAt != nil {
			at := d.LastAttemptAt.UTC()
			d.LastAttemptAt = &at
		}
		if d.NextAttemptAt != nil {
			at := d.NextAttemptAt.UTC()
			d.NextAttemptAt = &at
		}
		return string(verificationBytes(d))
	}
	var source string
	for _, p := range c.v.snapshot(t).Publications {
		if p.Delivery != nil && p.Delivery.ID == stored.ID {
			source = p.ID
			if got := normal(*p.Delivery); got != normal(want) {
				t.Fatalf("read Delivery\n got %s\nwant %s", got, normal(want))
			}
		}
	}
	if source == "" {
		t.Fatal("snapshot has no delivery for the publication")
	}
	ctx, owner := bootstrapOwner(t, x)
	page, err := x.Backend.(store.VerificationReader).ReadVerificationPage(ctx, store.VerificationAccess{TaskID: c.v.access.TaskID, UserID: owner.ID}, store.VerificationPageRequest{Kind: "publications", ContextID: c.v.contextID, Limit: 10})
	requireOK(t, err)
	found := false
	for _, item := range page.Items {
		if item.ID != source {
			continue
		}
		found = true
		var meta map[string]string
		requireOK(t, json.Unmarshal(item.Metadata, &meta))
		if item.State != "failed" || meta["error_class"] != "forge" || meta["error_message"] != stored.ErrorMessage || meta["updated_at"] != stored.UpdatedAt.UTC().Format(time.RFC3339Nano) || meta["next_attempt_at"] != retryAt.Format(time.RFC3339Nano) {
			t.Fatalf("publication page item: state %s metadata %v", item.State, meta)
		}
		if strings.Contains(string(item.Metadata), "Durable verification") {
			t.Fatal("publication page carried the summary")
		}
	}
	if !found {
		t.Fatalf("publication page lacks the source: %+v", page.Items)
	}
	if after := c.read(t); normal(after) != normal(stored) || after.Summary != stored.Summary {
		t.Fatal("a read changed the retained delivery row")
	}
}

// runDeliveryWindowPastActiveJobs proves the 100-row window holds only rows
// with no unfinished job: with 150 pending rows whose first 100 in due order
// have an unfinished job, one pass enqueues the 50 rows past them, acts on at
// most 100 rows, and adds no second job (invariant 19).
func runDeliveryWindowPastActiveJobs(t *testing.T, x Fixture) {
	const total, served = 150, 100
	v := newVerificationFixture(t, x)
	cases := make([]deliveryCase, 0, total)
	for index := range total {
		if index == 0 {
			e := v.envelope("window", "window-key", "observed state")
			v.apply(t, store.VerificationCommand{Kind: store.VerificationWriteEvidence, Key: e.SubmissionKey, Evidence: []json.RawMessage{verificationBytes(e)}})
		}
		cases = append(cases, addDeliveryCase(t, x, v, "fixture/window", 4700+index))
	}
	sort.Slice(cases, func(i, j int) bool {
		return store.VerificationDeliveryKey(cases[i].args.Repository, cases[i].args.PullRequestNumber) < store.VerificationDeliveryKey(cases[j].args.Repository, cases[j].args.PullRequestNumber)
	})
	// Settle every earlier case's leftovers, so the window below holds only
	// this case's rows.
	err, _ := cases[0].reconcile(t, time.Now().UTC())
	requireOK(t, err)
	// The jobs of the last 50 rows in due order end without settling their
	// rows, as a job does whose handler's outcome was lost; the first 100 keep
	// their unfinished jobs.
	stranded := map[string]bool{}
	for _, c := range cases[served:] {
		stranded[c.args.UniqueKey()] = true
	}
	runDeliveryQueue(t, x, stranded, func(context.Context, queue.VerificationPublicationArgs) error { return nil }, nil, func() bool {
		for _, c := range cases[served:] {
			job, err := logqueue.Load(c.v.ctx, x.Backend.Log(), x.Workspace, c.stream)
			if err != nil || job.State != logqueue.StateCompleted {
				return false
			}
		}
		return true
	})
	before := make([]logqueue.Job, len(cases))
	for i, c := range cases {
		before[i] = c.job(t)
		if active := before[i].Active(); active != (i < served) {
			t.Fatalf("row %d before the pass: job %+v", i, before[i])
		}
	}
	probe, stop := probeVerificationReads(t)
	defer stop()
	err, _ = cases[0].reconcile(t, time.Now().UTC())
	requireOK(t, err)
	if due := probe.rows(x.Workspace, store.VerificationReadDue); due != total-served {
		t.Fatalf("the pass selected %d due rows, want the %d rows without a job", due, total-served)
	}
	for i, c := range cases {
		job := c.job(t)
		switch {
		case i < served && job.Generation != before[i].Generation:
			t.Fatalf("row %d with an unfinished job got a second job: %+v", i, job)
		case i >= served && (!job.Active() || job.Generation != before[i].Generation+1):
			t.Fatalf("row %d past the first %d got no job: %+v", i, served, job)
		}
		if d := c.read(t); d.State != "pending" {
			t.Fatalf("row %d after the pass: %+v", i, d)
		}
	}
	probe.reset()
	err, _ = cases[0].reconcile(t, time.Now().UTC())
	requireOK(t, err)
	if due := probe.rows(x.Workspace, store.VerificationReadDue); due != 0 {
		t.Fatalf("a pass after every row has a job selected %d rows", due)
	}
}
