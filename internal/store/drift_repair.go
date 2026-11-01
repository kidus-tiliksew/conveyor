package store

import (
	"context"
	"encoding/json"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Drift repair reports carry each mismatch a start-up check repairs from the
// pass that repaired it to the check that records it (component-persistence,
// Drift repair reports; req-quiet-idle-runtime AC-4.1, AC-4.2).

// DriftRepairedEvent is the task event the start-up check appends once per
// reported repair.
const DriftRepairedEvent = "drift.repaired"

// Start-up check pass names (component-runtime, Start-up checks and
// background loops).
const (
	DriftPassGitHubLifecycleIntents  = "github_lifecycle_intents"
	DriftPassGitHubIssuePublications = "github_issue_publications"
	DriftPassQueuedTasks             = "queued_tasks"
	DriftPassBlueprintClosures       = "blueprint_closures"
	DriftPassReviewPublications      = "review_publications"
	DriftPassVerificationDeliveries  = "verification_deliveries"
)

// DriftRepair is one mismatch between a task's state and its background work
// or publications, and the repair a pass applied to it.
type DriftRepair struct {
	TaskID   string
	Pass     string
	Mismatch string
	Repair   string
}

type driftRecorderKey struct{}

// WithDriftRecorder binds record to ctx. Each reporting pass run under the
// returned context calls it once per repair, after the repair commits.
func WithDriftRecorder(ctx context.Context, record func(DriftRepair)) context.Context {
	return context.WithValue(ctx, driftRecorderKey{}, record)
}

// ReportDriftRepair calls the recorder bound to ctx. Without one it does
// nothing, so a pass run outside the start-up checks reports nothing.
func ReportDriftRepair(ctx context.Context, repair DriftRepair) {
	if record, ok := ctx.Value(driftRecorderKey{}).(func(DriftRepair)); ok && record != nil {
		record(repair)
	}
}

// DriftRepairedPayload is the drift.repaired event payload: the pass, the
// mismatch, and the repair.
func DriftRepairedPayload(repair DriftRepair) json.RawMessage {
	return core.JSONPayload(map[string]string{"pass": repair.Pass, "mismatch": repair.Mismatch, "repair": repair.Repair})
}

// The reports each pass makes. A store reports these strings so every backend
// names a repair the same way.
var (
	DriftQueuedTaskWithoutJob = DriftRepair{Pass: DriftPassQueuedTasks,
		Mismatch: "queued task had no active dispatch job", Repair: "enqueued a dispatch job"}
	DriftRunningTaskJobDiscarded = DriftRepair{Pass: DriftPassQueuedTasks,
		Mismatch: "running task's dispatch job was discarded after its final attempt", Repair: "failed the dispatch and parked the task"}
	DriftBlueprintParentOpen = DriftRepair{Pass: DriftPassBlueprintClosures,
		Mismatch: "blueprint parent stayed queued after every child finished", Repair: "closed the blueprint parent"}
	DriftReviewPublicationMissing = DriftRepair{Pass: DriftPassReviewPublications,
		Mismatch: "completed review had no publication", Repair: "queued the review publication"}
	DriftReviewPublicationWithoutComment = DriftRepair{Pass: DriftPassReviewPublications,
		Mismatch: "published review publication had no comment", Repair: "moved the publication to retrying"}
	DriftIssuePublicationWithoutJob = DriftRepair{Pass: DriftPassGitHubIssuePublications,
		Mismatch: "queued GitHub issue publication had no active job", Repair: "enqueued the publication job"}
	DriftApprovedPlanWithoutIssuePublication = DriftRepair{Pass: DriftPassGitHubLifecycleIntents,
		Mismatch: "approved plan had no GitHub issue publication", Repair: "queued the issue publication"}
	DriftStartedOverWithoutPullRequestClose = DriftRepair{Pass: DriftPassGitHubLifecycleIntents,
		Mismatch: "started-over task had no pull-request close intent", Repair: "queued the pull-request close"}
	DriftVerificationDeliveryWithoutJob = DriftRepair{Pass: DriftPassVerificationDeliveries,
		Mismatch: "pending verification delivery had no active job", Repair: "enqueued the delivery job"}
)

// For returns r with the task set.
func (r DriftRepair) For(taskID string) DriftRepair {
	r.TaskID = taskID
	return r
}
