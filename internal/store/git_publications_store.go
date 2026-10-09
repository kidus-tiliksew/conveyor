package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ForgeFailure is the latest unresolved GitHub projection or merge failure
// that belongs in the needs-operator surface.
type ForgeFailure struct {
	Category string    `json:"category"`
	Detail   string    `json:"detail"`
	Surface  string    `json:"surface"`
	At       time.Time `json:"at"`
}

// LatestForgeFailure reduces durable projection events to the current
// operator-actionable GitHub failure.
func LatestForgeFailure(events []core.Event) *ForgeFailure {
	var issue *ForgeFailure
	reviews := make(map[string]*ForgeFailure)
	var merge *ForgeFailure
	var conflict *ForgeFailure
	for _, event := range events {
		var payload struct {
			ReviewWorkOrderID  string `json:"review_work_order_id"`
			ForgeErrorCategory string `json:"forge_error_category"`
			LastError          string `json:"last_error"`
			Error              string `json:"error"`
		}
		switch event.Kind {
		case "github_issue.publication_failed":
			if json.Unmarshal(event.Payload, &payload) == nil {
				issue = &ForgeFailure{Category: payload.ForgeErrorCategory, Detail: payload.LastError, Surface: "GitHub issue publication", At: event.At}
			}
		case "github_issue.publication_published":
			issue = nil
		case "review.publication_failed":
			if json.Unmarshal(event.Payload, &payload) == nil {
				key := payload.ReviewWorkOrderID
				if key == "" {
					key = event.JobID
				}
				reviews[key] = &ForgeFailure{Category: payload.ForgeErrorCategory, Detail: payload.LastError, Surface: "GitHub review publication", At: event.At}
			}
		case "review.publication_published":
			if json.Unmarshal(event.Payload, &payload) == nil {
				key := payload.ReviewWorkOrderID
				if key == "" {
					key = event.JobID
				}
				delete(reviews, key)
			}
		case "merge.failed":
			if json.Unmarshal(event.Payload, &payload) == nil {
				merge = &ForgeFailure{Category: payload.ForgeErrorCategory, Detail: payload.Error, Surface: "GitHub merge", At: event.At}
			}
		case "merge.confirmed", "merge.reconciled":
			merge = nil
		case "merge.blocked", "merge.conflict_cleared", "merge.conflict_fix_dispatched":
			conflict = nil
		case "merge.conflict_recovery_blocked":
			if json.Unmarshal(event.Payload, &payload) == nil {
				conflict = &ForgeFailure{Category: "interrupted_review_recovery", Detail: payload.Error, Surface: "Merge conflict recovery", At: event.At}
			}
		case "merge.conflict_dispatch_exhausted":
			if json.Unmarshal(event.Payload, &payload) == nil {
				conflict = &ForgeFailure{Category: "conflict_dispatch_exhausted", Detail: payload.Error, Surface: "Merge conflict dispatch", At: event.At}
			}
		}
	}
	var latest *ForgeFailure
	consider := func(candidate *ForgeFailure) {
		if candidate != nil && (latest == nil || candidate.At.After(latest.At)) {
			copy := *candidate
			latest = &copy
		}
	}
	consider(issue)
	for _, review := range reviews {
		consider(review)
	}
	consider(merge)
	consider(conflict)
	return latest
}

func (m *memory) QueueReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := NormalizeForgeAuthorProjectionForWrite(&publication.ForgeAuthorClass, &publication.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	if _, ok := m.publications[publication.ReviewWorkOrderID]; ok {
		return nil
	}
	now := time.Now().UTC()
	publication.State, publication.CreatedAt, publication.UpdatedAt = core.ReviewPublicationQueued, now, now
	m.publications[publication.ReviewWorkOrderID] = publication
	m.appendEventLocked(ctx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.publication_queued", Payload: core.JSONPayload(publication)})
	return nil
}

func (m *memory) GetReviewPublication(_ context.Context, id string) (core.ReviewPublication, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	publication, ok := m.publications[id]
	if !ok {
		return core.ReviewPublication{}, fmt.Errorf("review publication %s not found", id)
	}
	return publication, nil
}

func (m *memory) UpdateReviewPublication(ctx context.Context, publication core.ReviewPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := NormalizeForgeAuthorProjectionForWrite(&publication.ForgeAuthorClass, &publication.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	current, ok := m.publications[publication.ReviewWorkOrderID]
	if !ok {
		return fmt.Errorf("review publication %s not found", publication.ReviewWorkOrderID)
	}
	if err := ValidateReviewPublicationUpdate(current, publication); err != nil {
		return err
	}
	publication.UpdatedAt = time.Now().UTC()
	m.publications[publication.ReviewWorkOrderID] = publication
	kind := "review.publication_retry"
	if publication.State == core.ReviewPublicationPublished {
		kind = "review.publication_published"
	} else if publication.State == core.ReviewPublicationFailed {
		kind = "review.publication_failed"
	}
	m.appendEventLocked(ctx, core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: kind, Payload: core.JSONPayload(publication)})
	return nil
}

func (m *memory) ReconcileReviewPublications(ctx context.Context) (int, error) {
	m.mu.Lock()
	var missing []core.ReviewPublication
	seen := map[string]bool{}
	repaired := 0
	for taskID, events := range m.events {
		for _, event := range events {
			if event.Kind != "review.completed" {
				continue
			}
			publication, ok := reviewPublicationFromEvent(taskID, event.JobID, event.Payload)
			if ok && !seen[publication.ReviewWorkOrderID] {
				seen[publication.ReviewWorkOrderID] = true
				if existing, exists := m.publications[publication.ReviewWorkOrderID]; !exists {
					missing = append(missing, publication)
				} else if existing.State == core.ReviewPublicationPublished && existing.CommentID <= 0 {
					existing.State = core.ReviewPublicationRetrying
					if existing.ForgeAuthorClass == "" || existing.ForgeAuthorClass == core.ForgeAuthorClass("host") {
						existing.ForgeAuthorClass = core.ForgeAuthorWorkspace
						existing.ForgeAuthorUserID = ""
					}
					existing.ForgeErrorCategory = ""
					existing.LastError = "reconciling published review projection without required comment"
					existing.UpdatedAt = time.Now().UTC()
					m.publications[existing.ReviewWorkOrderID] = existing
					m.appendEventLocked(ctx, core.Event{
						TaskID: existing.TaskID, JobID: existing.JobID,
						Kind: "review.publication_retry", Payload: core.JSONPayload(existing),
					})
					repaired++
				}
			}
		}
	}
	m.mu.Unlock()
	created := repaired
	for _, publication := range missing {
		if err := m.QueueReviewPublication(ctx, publication); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
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

func reviewPublicationFromEvent(taskID, jobID string, payload []byte) (core.ReviewPublication, bool) {
	var eligibility struct {
		PublicationEligible bool `json:"publication_eligible"`
	}
	if json.Unmarshal(payload, &eligibility) != nil || !eligibility.PublicationEligible {
		return core.ReviewPublication{}, false
	}
	var publication core.ReviewPublication
	if json.Unmarshal(payload, &publication) != nil || publication.ReviewWorkOrderID == "" {
		return core.ReviewPublication{}, false
	}
	publication.TaskID = taskID
	publication.JobID = jobID
	publication.State = core.ReviewPublicationQueued
	if publication.ForgeAuthorClass == "" || publication.ForgeAuthorClass == core.ForgeAuthorClass("host") {
		publication.ForgeAuthorClass = core.ForgeAuthorWorkspace
		publication.ForgeAuthorUserID = ""
	}
	return publication, true
}

func ValidateGitHubPublicationTransition(from, to core.GitHubPublicationState) error {
	if from == to {
		return nil
	}
	command := publicationCommand(string(to), string(core.GitHubPublicationRetrying), string(core.GitHubPublicationPublished), string(core.GitHubPublicationFailed))
	expected, err := core.TransitionGitHubPublication(from, command)
	if err != nil {
		return err
	}
	if expected != to {
		return fmt.Errorf("GitHub publication command %q resolves to %q, not %q", command, expected, to)
	}
	return nil
}

func ValidateReviewPublicationTransition(from, to core.ReviewPublicationState) error {
	if from == to {
		return nil
	}
	command := publicationCommand(string(to), string(core.ReviewPublicationRetrying), string(core.ReviewPublicationPublished), string(core.ReviewPublicationFailed))
	expected, err := core.TransitionReviewPublication(from, command)
	if err != nil {
		return err
	}
	if expected != to {
		return fmt.Errorf("review publication command %q resolves to %q, not %q", command, expected, to)
	}
	return nil
}

// ValidateReviewPublicationUpdate permits one correcting transition for the
// impossible pre-v2.3 state that reported a required comment as published
// without a comment ID. All valid publications retain the canonical terminal
// transition rules.
func ValidateReviewPublicationUpdate(current, next core.ReviewPublication) error {
	repairingMissingComment := current.State == core.ReviewPublicationPublished &&
		current.CommentID <= 0 && next.State == core.ReviewPublicationRetrying
	if !repairingMissingComment {
		if err := ValidateReviewPublicationTransition(current.State, next.State); err != nil {
			return err
		}
	}
	return ValidateReviewPublicationProjection(next)
}

// ValidateReviewPublicationProjection prevents a required Phase 5.3 projection
// from being recorded as complete without its deterministic aggregate comment
func ValidateReviewPublicationProjection(publication core.ReviewPublication) error {
	if err := validateForgeAuthorProjection(publication.ForgeAuthorClass, publication.ForgeAuthorUserID); err != nil {
		return fmt.Errorf("review publication %s: %w", publication.ReviewWorkOrderID, err)
	}
	if publication.State == core.ReviewPublicationPublished && publication.CommentID <= 0 {
		return fmt.Errorf("published review publication %s requires a nonzero comment ID", publication.ReviewWorkOrderID)
	}
	return nil
}

// NormalizeForgeAuthorProjectionForWrite applies the caller's explicit default
// and rejects retired or internally inconsistent author attribution before a
// projection or event is written.
func NormalizeForgeAuthorProjectionForWrite(class *core.ForgeAuthorClass, userID *string, fallback core.ForgeAuthorClass) error {
	if *class == "" {
		*class = fallback
	}
	*userID = strings.TrimSpace(*userID)
	return validateForgeAuthorProjection(*class, *userID)
}

func validateForgeAuthorProjection(class core.ForgeAuthorClass, userID string) error {
	// Historical replay input predates forge attribution. Store write paths
	// normalize the blank pair to workspace before persisting a new event.
	if class == "" && userID == "" {
		return nil
	}
	switch class {
	case core.ForgeAuthorWorkspace:
		if userID != "" {
			return fmt.Errorf("workspace forge author cannot carry a user ID")
		}
	case core.ForgeAuthorExecutingUser, core.ForgeAuthorApprovingOperator:
		if userID == "" {
			return fmt.Errorf("forge author class %s requires a user ID", class)
		}
	default:
		return fmt.Errorf("unsupported forge author class %q", class)
	}
	return nil
}

func publicationCommand(to, retrying, published, failed string) string {
	switch to {
	case retrying:
		return "publication.retry"
	case published:
		return "publication.publish"
	case failed:
		return "publication.fail"
	default:
		return "publication.invalid"
	}
}

func (m *memory) QueueGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := NormalizeForgeAuthorProjectionForWrite(&lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	if _, ok := m.tasks[lifecycle.TaskID]; !ok {
		return fmt.Errorf("task %s not found", lifecycle.TaskID)
	}
	if _, exists := m.github[lifecycle.TaskID]; exists {
		return nil
	}
	now := time.Now().UTC()
	if lifecycle.CreatedAt.IsZero() {
		lifecycle.CreatedAt = now
	}
	lifecycle.UpdatedAt = lifecycle.CreatedAt
	if lifecycle.State == "" {
		lifecycle.State = core.GitHubPublicationQueued
	}
	if lifecycle.CreateState == "" {
		lifecycle.CreateState = core.GitHubCreateNotStarted
	}
	m.github[lifecycle.TaskID] = lifecycle
	m.appendEventLocked(ctx, core.Event{TaskID: lifecycle.TaskID, Kind: "github_issue.publication_queued", Payload: core.JSONPayload(lifecycle)})
	return nil
}

func (m *memory) GetGitHubLifecycle(_ context.Context, taskID string) (core.GitHubLifecycle, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lifecycle, ok := m.github[taskID]
	return lifecycle, ok, nil
}

func (m *memory) UpdateGitHubLifecycle(ctx context.Context, lifecycle core.GitHubLifecycle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := NormalizeForgeAuthorProjectionForWrite(&lifecycle.ForgeAuthorClass, &lifecycle.ForgeAuthorUserID, core.ForgeAuthorWorkspace); err != nil {
		return err
	}
	current, ok := m.github[lifecycle.TaskID]
	if !ok {
		return fmt.Errorf("GitHub lifecycle for task %s not found", lifecycle.TaskID)
	}
	if err := ValidateGitHubPublicationTransition(current.State, lifecycle.State); err != nil {
		return err
	}
	lifecycle.UpdatedAt = time.Now().UTC()
	m.github[lifecycle.TaskID] = lifecycle
	var kind string
	switch {
	case lifecycle.State == core.GitHubPublicationPublished:
		kind = "github_issue.publication_published"
	case lifecycle.State == core.GitHubPublicationFailed:
		kind = "github_issue.publication_failed"
	case lifecycle.State == core.GitHubPublicationRetrying && strings.TrimSpace(lifecycle.LastError) != "":
		kind = "github_issue.publication_retry"
	}
	if kind != "" {
		m.appendEventLocked(ctx, core.Event{TaskID: lifecycle.TaskID, Kind: kind, Payload: core.JSONPayload(lifecycle)})
	}
	return nil
}

func (m *memory) ReconcileGitHubLifecycles(context.Context) (int, error) { return 0, nil }
