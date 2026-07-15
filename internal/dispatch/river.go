package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type dispatchTaskWorker struct {
	river.WorkerDefaults[queueargs.DispatchTaskArgs]
	dispatcher *Dispatcher
}

type reviewPublicationWorker struct {
	river.WorkerDefaults[queueargs.ReviewPublicationArgs]
	dispatcher *Dispatcher
}

func (w *reviewPublicationWorker) Work(ctx context.Context, job *river.Job[queueargs.ReviewPublicationArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:review-publication:%d", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, job.Args.WorkspaceID)
	publication, err := w.dispatcher.Store.GetReviewPublication(ctx, job.Args.ReviewWorkOrderID)
	if err != nil || publication.State == core.ReviewPublicationPublished {
		return err
	}
	publication.Attempts++
	publication.State = core.ReviewPublicationRetrying
	publication.LastError = ""
	if err = w.dispatcher.Store.UpdateReviewPublication(ctx, publication); err != nil {
		return err
	}
	task, err := w.dispatcher.Store.GetTask(ctx, publication.TaskID)
	if err != nil {
		return err
	}
	cfg, err := w.dispatcher.currentConfig(ctx)
	if err != nil {
		return err
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok || repo.GitHub == "" {
		return fmt.Errorf("GitHub repository is not configured for %s", task.Repo)
	}
	var bounceHistory []string
	skipComment := false
	events, _ := w.dispatcher.Store.ListEvents(ctx, task.ID)
	for _, event := range events {
		if event.Kind == "review.completed" && event.At.After(publication.CreatedAt) {
			skipComment = true
		}
		if event.Kind != "pipeline.bounced" {
			continue
		}
		var item struct {
			Count      int    `json:"count"`
			ReasonCode string `json:"reason_code"`
		}
		if json.Unmarshal(event.Payload, &item) == nil {
			bounceHistory = append(bounceHistory, fmt.Sprintf("bounce %d: %s", item.Count, item.ReasonCode))
		}
	}
	taskLink := ""
	if strings.HasPrefix(task.Source, "http://") || strings.HasPrefix(task.Source, "https://") {
		taskLink = task.Source
	} else if source, ok := strings.CutPrefix(task.Source, "github:"); ok {
		if slug, number, found := strings.Cut(source, "#"); found && slug != "" && number != "" {
			taskLink = "https://github.com/" + slug + "/issues/" + number
		}
	}
	result, publishErr := github.PublishReview(ctx, github.ReviewPublication{
		Repo: repo.GitHub, Branch: task.Branch, TaskID: task.ID, TaskLink: taskLink,
		ReviewWorkOrderID: publication.ReviewWorkOrderID, Verdict: publication.Verdict,
		ReasonCode: publication.ReasonCode, Summary: publication.Summary, Feedback: publication.Feedback,
		ReviewedCommitSHA: publication.ReviewedCommitSHA, ReviewerModel: publication.ReviewerModel,
		ReviewerSession: publication.ReviewerSession, SameModelAsImplementer: publication.SameModelAsImplementer,
		BounceHistory: bounceHistory,
		SkipComment:   skipComment,
	})
	if publishErr != nil {
		publication.LastError = publishErr.Error()
		if job.Attempt >= job.MaxAttempts {
			publication.State = core.ReviewPublicationFailed
		}
		if updateErr := w.dispatcher.Store.UpdateReviewPublication(ctx, publication); updateErr != nil {
			return fmt.Errorf("publish review: %v; record retry: %w", publishErr, updateErr)
		}
		return publishErr
	}
	publication.State = core.ReviewPublicationPublished
	publication.CheckRunID = result.CheckRunID
	publication.CommentID = result.CommentID
	publication.ReviewedCommitSHA = result.ReviewedCommitSHA
	publication.LastError = ""
	return w.dispatcher.Store.UpdateReviewPublication(ctx, publication)
}

func (w *dispatchTaskWorker) Work(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs]) error {
	ctx = store.WithActor(ctx, store.Actor{ID: fmt.Sprintf("river:%d", job.ID), Role: core.ActorSystem})
	ctx = store.WithWorkspace(ctx, job.Args.WorkspaceID)
	err := w.dispatcher.runTask(ctx, job.Args.TaskID)
	if err == nil {
		task, getErr := w.dispatcher.Store.GetTask(ctx, job.Args.TaskID)
		if getErr != nil {
			return getErr
		}
		if task.State == core.TaskQueued {
			// The currently running River row owns this task's pipeline. A
			// queued stage transition cannot insert a duplicate while this row
			// is active, so snooze the same row into the next stage.
			return river.JobSnooze(time.Second)
		}
		return nil
	}
	return w.handleFailure(ctx, job, err)
}

func (w *dispatchTaskWorker) handleFailure(ctx context.Context, job *river.Job[queueargs.DispatchTaskArgs], err error) error {
	payload := core.JSONPayload(map[string]any{
		"attempt":      job.Attempt,
		"max_attempts": job.MaxAttempts,
		"error":        err.Error(),
	})
	if eventErr := w.dispatcher.Store.AppendEvent(ctx, core.Event{
		TaskID: job.Args.TaskID, Kind: "dispatch.failed", Payload: payload,
	}); eventErr != nil {
		log.Printf("[task %s] record River failure: %v", job.Args.TaskID, eventErr)
	}
	task, taskErr := w.dispatcher.Store.GetTask(ctx, job.Args.TaskID)
	if taskErr != nil {
		return fmt.Errorf("dispatch failed: %v; load recovery transition: %w", err, taskErr)
	}
	recoveryStage := task.NextStage
	if recoveryStage == "" {
		if latest, ok, latestErr := w.dispatcher.Store.GetLatestJob(ctx, job.Args.TaskID); latestErr == nil && ok {
			recoveryStage = latest.Stage
		}
	}
	if job.Attempt >= job.MaxAttempts {
		if stateErr := w.dispatcher.Store.SetTaskTransition(ctx, job.Args.TaskID, core.TaskParked, "", recoveryStage); stateErr != nil {
			return fmt.Errorf("dispatch failed: %v; park after final River attempt: %w", err, stateErr)
		}
	} else if stateErr := w.dispatcher.Store.SetTaskTransition(ctx, job.Args.TaskID, core.TaskQueued, recoveryStage, ""); stateErr != nil {
		return fmt.Errorf("dispatch failed: %v; persist retry transition: %w", err, stateErr)
	}
	return err
}

// NewRiverClient binds the durable queue to the dispatcher. The Store owns
// transactional insertion; this client owns only worker execution (spec
// §3.1, §17.0).
func NewRiverClient(pool *pgxpool.Pool, dispatcher *Dispatcher, workspaces []string) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &dispatchTaskWorker{dispatcher: dispatcher})
	river.AddWorker(workers, &reviewPublicationWorker{dispatcher: dispatcher})
	queues := map[string]river.QueueConfig{queueargs.ControlQueue: {MaxWorkers: 1}}
	for _, workspace := range workspaces {
		queues[queueargs.DispatchQueue(workspace)] = river.QueueConfig{MaxWorkers: 1}
		queues[queueargs.ReviewPublicationQueue(workspace)] = river.QueueConfig{MaxWorkers: 1}
	}
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		// Dispatcher stage contexts enforce the configured per-stage wall-clock
		// limits. River's one-minute default would cancel long harness runs and
		// handoff artifact collection before those limits (spec §14).
		JobTimeout: -1,
		Queues:     queues,
		Workers:    workers,
	})
}

func AddWorkspaceQueues(client *river.Client[pgx.Tx], workspace string) error {
	if err := client.Queues().Add(queueargs.DispatchQueue(workspace), river.QueueConfig{MaxWorkers: 1}); err != nil {
		if !errors.Is(err, &river.QueueAlreadyAddedError{}) {
			return err
		}
	}
	err := client.Queues().Add(queueargs.ReviewPublicationQueue(workspace), river.QueueConfig{MaxWorkers: 1})
	if errors.Is(err, &river.QueueAlreadyAddedError{}) {
		return nil
	}
	return err
}
