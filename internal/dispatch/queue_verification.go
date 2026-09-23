package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type verificationPublicationWorker struct{ dispatcher *Dispatcher }

func (w *verificationPublicationWorker) Reconcile(ctx context.Context, workspace string) error {
	if workspace == "" {
		return store.VerificationDeliveryIdentityError()
	}
	b, ok := w.dispatcher.Store.(store.VerificationDeliveryStore)
	if !ok {
		return nil
	}
	ctx = store.WithWorkspace(store.WithActor(ctx, store.Actor{ID: "verification-publication", Role: core.ActorSystem}), workspace)
	return b.ReconcileVerificationDeliveries(ctx)
}

// Legacy performs only translation. Source and stream IDs never resolve a
// workspace; older payloads are bound exclusively to the trusted partition.
func (w *verificationPublicationWorker) Legacy(ctx context.Context, job queue.Job) error {
	var a struct {
		store.VerificationPublication
		WorkspaceID string `json:"workspace_id"`
	}
	if err := core.DecodeVerificationRequest(job.Args, &a); err != nil {
		return store.ErrVerificationInvalid
	}
	if job.WorkspaceID == "" || a.WorkspaceID != "" && a.WorkspaceID != job.WorkspaceID {
		return store.VerificationDeliveryIdentityError()
	}
	// Reject alternate spellings too; legacy publications had no workspace field.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(job.Args, &fields); err != nil {
		return store.ErrVerificationInvalid
	}
	if raw, exists := fields["workspace_id"]; exists {
		var ws string
		if json.Unmarshal(raw, &ws) != nil || ws != job.WorkspaceID {
			return store.VerificationDeliveryIdentityError()
		}
	}
	if a.ID == "" || a.TaskID == "" {
		return store.ErrVerificationInvalid
	}
	b, ok := w.dispatcher.Store.(store.VerificationDeliveryStore)
	if !ok {
		return store.ErrVerificationInvalid
	}
	ctx = store.WithWorkspace(store.WithActor(ctx, store.Actor{ID: "verification-publication", Role: core.ActorSystem}), job.WorkspaceID)
	return b.TranslateVerificationPublication(ctx, a.VerificationPublication)
}

func (w *verificationPublicationWorker) Work(ctx context.Context, job queue.Job) error {
	var a queue.VerificationPublicationArgs
	err := core.DecodeVerificationRequest(job.Args, &a)
	if err != nil {
		return store.ErrVerificationInvalid
	}
	if !a.ValidWorkspace(job.WorkspaceID) {
		return store.VerificationDeliveryIdentityError()
	}
	b, ok := w.dispatcher.Store.(store.VerificationDeliveryStore)
	if !ok {
		return store.ErrVerificationInvalid
	}
	ctx = store.WithWorkspace(store.WithActor(ctx, store.Actor{ID: "verification-publication", Role: core.ActorSystem}), job.WorkspaceID)
	// Resolve credentials before the transaction lock: the App client itself reads
	// store state. Only this typed worker ever writes a verification region.
	forgeCtx, authErr := w.dispatcher.workspaceForgeContext(ctx, a.Repository)
	read := w.dispatcher.ReadVerificationPR
	if read == nil {
		read = github.ReadVerificationPullRequest
	}
	write := w.dispatcher.WriteVerificationPR
	if write == nil {
		write = github.WriteVerificationPullRequest
	}
	var deliveryErr error
	err = b.RunVerificationDelivery(ctx, a, func(d *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
		if d.State == "published" || d.State == "failed" || d.State == "superseded" {
			return nil
		}
		if err := save("attempt", *d); err != nil {
			return err
		}
		observedHead, observedDigest := "", ""
		fail := func(class string) error {
			next := *d
			next.ErrorClass = class
			next.ObservedHead = observedHead
			next.ObservedDigest = observedDigest
			at := time.Now().UTC().Add(time.Second * time.Duration(1<<min(d.CycleAttempts-1, 12)))
			next.NextAttemptAt = &at
			command := "retry_error"
			if d.CycleAttempts >= 5 {
				command = "fail"
				retryAt := time.Now().UTC().Add(15 * time.Minute)
				next.NextAttemptAt = &retryAt
			}
			if err := save(command, next); err != nil {
				return err
			}
			deliveryErr = errors.New("verification publication " + class)
			return nil
		}
		if authErr != nil {
			return fail("auth")
		}
		// The backend holds the intent lock across these operations. New generations
		// cannot commit between the latest-generation read and any forge operation.
		if err := save("check", *d); err != nil {
			return err
		}
		pr, err := read(forgeCtx, a.Repository, a.PullRequestNumber)
		if err != nil || !github.ValidSnapshotSHA(pr.Head.SHA) {
			return fail("forge")
		}
		next := *d
		next.ObservedHead = pr.Head.SHA
		next.ObservedDigest = github.VerificationBodyDigest(pr.Body, d.WorkspaceID, d.TaskID)
		observedHead, observedDigest = next.ObservedHead, next.ObservedDigest
		if pr.Head.SHA != d.TargetHead {
			return fail("head_mismatch")
		}
		if next.ObservedDigest == d.TargetDigest {
			next.ErrorClass = ""
			next.NextAttemptAt = nil
			return save("publish", next)
		}
		body := github.ComposeVerificationBody(pr.Body, d.WorkspaceID, d.TaskID, d.Summary)
		if err := save("check", *d); err != nil {
			return err
		}
		writeErr := write(forgeCtx, a.Repository, a.PullRequestNumber, body)
		// Even an error may mean GitHub accepted the write. Always reconcile by read.
		if err := save("check", *d); err != nil {
			return err
		}
		observed, readErr := read(forgeCtx, a.Repository, a.PullRequestNumber)
		if readErr != nil || !github.ValidSnapshotSHA(observed.Head.SHA) {
			return fail("forge")
		}
		next = *d
		next.ObservedHead = observed.Head.SHA
		next.ObservedDigest = github.VerificationBodyDigest(observed.Body, d.WorkspaceID, d.TaskID)
		observedHead, observedDigest = next.ObservedHead, next.ObservedDigest
		if next.ObservedHead != d.TargetHead {
			return fail("head_mismatch")
		}
		if next.ObservedDigest != d.TargetDigest {
			if writeErr != nil {
				return fail("forge")
			}
			return fail("readback_mismatch")
		}
		next.ErrorClass = ""
		next.NextAttemptAt = nil
		return save("publish", next)
	})
	if errors.Is(err, context.Canceled) {
		return queue.Snooze(time.Second)
	}
	if err != nil {
		return err
	}
	return deliveryErr
}
