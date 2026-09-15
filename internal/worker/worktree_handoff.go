package worker

import (
	"context"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Service) WorktreeHandoff(ctx context.Context, id string, claim core.WorkOrderClaimIdentity, request core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorktreeHandoff{}, err
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return core.WorktreeHandoff{}, err
	}
	if s.ConfigProvider == nil {
		return core.WorktreeHandoff{}, fmt.Errorf("repository configuration unavailable")
	}
	cfg, err := s.ConfigProvider(ctx)
	if err != nil {
		return core.WorktreeHandoff{}, err
	}
	if cfg == nil {
		return core.WorktreeHandoff{}, fmt.Errorf("repository configuration unavailable")
	}
	repository := ""
	for _, repo := range cfg.Repos {
		if repo.Name == task.Repo {
			repository, err = gitx.NormalizeRepositoryIdentity(repo.URL)
			if err != nil {
				return core.WorktreeHandoff{}, err
			}
			break
		}
	}
	if repository == "" {
		return core.WorktreeHandoff{}, fmt.Errorf("assigned repository identity unavailable")
	}
	result, err := taskops.ExecuteWorkOrder(ctx, s.Store, task.ID, store.WorktreeHandoffCommand, func(lease taskops.TaskLease) (core.WorktreeHandoff, error) {
		return s.Store.WorktreeHandoffCommand(ctx, lease, id, claim, repository, request)
	})
	if err == nil && request.Action == "audit" && request.Producer != nil && request.Producer.AttemptID == result.Writer.AttemptID {
		checkpoint := core.WorkOrderAttemptCheckpoint{SessionID: request.SessionID, AttemptID: request.Producer.AttemptID, TerminationReason: request.OriginalReason, CommitSHA: request.CommitSHA, PushResult: "pushed"}
		if request.Transcript != nil {
			content, truncated, redactErr := s.boundedObservabilityContent(ctx, request.Transcript.Content, AttemptTranscriptLimit, request.Transcript.Truncated)
			if redactErr == nil {
				checkpoint.Transcript = &core.WorkOrderAttemptTranscript{Content: content, Truncated: truncated}
			}
		}
		_ = s.Store.FinalizeWorkOrderAttemptObservability(ctx, id, claim.WorkerID, checkpoint)
	}
	return result, err
}
