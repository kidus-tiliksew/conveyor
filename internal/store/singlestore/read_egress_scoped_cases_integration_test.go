package singlestore

import (
	"context"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Scoped hot-read egress cases. Each case compares the retained pre-change
// read — the whole-workspace listing plus the consumer's existing
// projection — with the scoped public read of the checked-out tree. The
// baseline methods are the original code and are never reimplemented here,
// so a candidate gain cannot come from a hand-tuned baseline.

func init() {
	readEgressCases.Register("artifact-owner-associations", buildScopedArtifactOwnerAssociations)
	readEgressCases.Register("requirement-version-summaries", buildScopedRequirementVersionSummaries)
	readEgressCases.Register("system-design-version-summaries", buildScopedSystemDesignVersionSummaries)
	readEgressCases.Register("review-policy-reconciliation-tasks", buildScopedReconciliationTasks)
}

func scopedArtifactOutcome(rows []core.Artifact) (storetest.ReadEgressOutcome, error) {
	digest, err := storetest.ReadEgressScopedArtifactDigest(rows)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(rows)}, err
}

// buildScopedArtifactOwnerAssociations compares the workspace artifact
// listing filtered by the selected task/requirement/session owners in Go
// with ListArtifactsForOwners. The fixture keeps more than a page of
// unrelated artifacts with long association names, plus orphan, shared,
// multi-role, and repeated associations; sizes come from
// CONVEYOR_READ_EGRESS_SCOPED_ASSOCIATIONS and
// CONVEYOR_READ_EGRESS_SCOPED_ASSOCIATION_NAME_BYTES.
func buildScopedArtifactOwnerAssociations(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_ASSOCIATIONS", 120)
	if err != nil {
		t.Fatal(err)
	}
	nameBytes, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_ASSOCIATION_NAME_BYTES", 512)
	if err != nil {
		t.Fatal(err)
	}
	owners, fixture := storetest.SeedReadEgressScopedArtifacts(t, ctx, st, noise, nameBytes)
	return storetest.ReadEgressCase{Name: "artifact-owner-associations", Fixture: fixture,
		BaselineRevision: "candidate-revision:retained-full-api+consumer-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			listed, err := st.ListArtifacts(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedArtifactOutcome(storetest.ReadEgressScopedArtifactsForOwners(listed, owners))
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			listed, err := st.ListArtifactsForOwners(ctx, owners)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedArtifactOutcome(storetest.ReadEgressScopedArtifactsForOwners(listed, owners))
		}}
}

// buildScopedRequirementVersionSummaries compares the workspace version
// history filtered by the selected requirements in Go with
// ListRequirementVersionSummaries. The fixture keeps a long history of
// large historical versions and unrelated requirements, so the baseline
// transfers their bodies while the candidate transfers only the current and
// pending bodies the list serves. Sizes come from
// CONVEYOR_READ_EGRESS_SCOPED_HISTORY_VERSIONS and
// CONVEYOR_READ_EGRESS_SCOPED_CORPUS_DOCUMENTS.
func buildScopedRequirementVersionSummaries(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	history, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_HISTORY_VERSIONS", 100)
	if err != nil {
		t.Fatal(err)
	}
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_CORPUS_DOCUMENTS", 80)
	if err != nil {
		t.Fatal(err)
	}
	_, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	ids, fixture := storetest.SeedReadEgressScopedRequirementVersions(t, ctx, st, history, noise, pad)
	requirements := make([]core.Requirement, 0, len(ids))
	for _, id := range ids {
		requirement, err := st.GetRequirement(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		requirements = append(requirements, requirement)
	}
	return storetest.ReadEgressCase{Name: "requirement-version-summaries", Fixture: fixture,
		BaselineRevision: "candidate-revision:retained-full-api+consumer-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			versions, err := st.ListRequirementVersionsByRequirement(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedRequirementOutcome(requirements, versions)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			versions, err := st.ListRequirementVersionSummaries(ctx, storetest.ReadEgressRequirementSelections(requirements))
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedRequirementOutcome(requirements, versions)
		}}
}

func scopedRequirementOutcome(requirements []core.Requirement, versions map[string][]core.RequirementVersion) (storetest.ReadEgressOutcome, error) {
	summaries := storetest.SelectReadEgressScopedRequirementSummaries(requirements, versions)
	digest, err := storetest.ReadEgressDigest(summaries)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(summaries)}, err
}

// buildScopedSystemDesignVersionSummaries is the System Design analogue:
// the whole workspace version history filtered by the selected designs in Go
// against ListSystemDesignVersionSummaries, whose selection drops
// historical and dismissed versions.
func buildScopedSystemDesignVersionSummaries(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	history, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_HISTORY_VERSIONS", 100)
	if err != nil {
		t.Fatal(err)
	}
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_CORPUS_DOCUMENTS", 80)
	if err != nil {
		t.Fatal(err)
	}
	_, pad, err := storetest.ReadEgressSizes()
	if err != nil {
		t.Fatal(err)
	}
	ids, fixture := storetest.SeedReadEgressScopedSystemDesignVersions(t, ctx, st, history, noise, pad)
	items := make([]core.SystemDesign, 0, len(ids))
	for _, id := range ids {
		item, err := st.GetSystemDesign(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return storetest.ReadEgressCase{Name: "system-design-version-summaries", Fixture: fixture,
		BaselineRevision: "candidate-revision:retained-full-api+consumer-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			versions, err := st.ListSystemDesignVersionsByDocument(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedSystemDesignOutcome(items, versions)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			versions, err := st.ListSystemDesignVersionSummaries(ctx, storetest.ReadEgressSystemDesignSelections(items))
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedSystemDesignOutcome(items, versions)
		}}
}

func scopedSystemDesignOutcome(items []core.SystemDesign, versions map[string][]core.SystemDesignVersion) (storetest.ReadEgressOutcome, error) {
	summaries := storetest.SelectReadEgressScopedDesignSummaries(items, versions)
	digest, err := storetest.ReadEgressDigest(summaries)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(summaries)}, err
}

// buildScopedReconciliationTasks compares the whole task list plus open
// policy statuses, filtered in Go, with
// ListReviewPolicyReconciliationTasks. The fixture holds active tasks, a
// terminal task with an open status, terminal tasks whose only rows are
// published, and unrelated terminal tasks; sizes come from
// CONVEYOR_READ_EGRESS_SCOPED_TASKS and
// CONVEYOR_READ_EGRESS_SCOPED_TASK_BODY_BYTES.
func buildScopedReconciliationTasks(t testing.TB, ctx context.Context, st *Store) storetest.ReadEgressCase {
	t.Helper()
	noise, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_TASKS", 120)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := storetest.ReadEgressCount("CONVEYOR_READ_EGRESS_SCOPED_TASK_BODY_BYTES", 4<<10)
	if err != nil {
		t.Fatal(err)
	}
	fixture := storetest.SeedReadEgressScopedReconciliationTasks(t, ctx, st, noise, bodyBytes)
	return storetest.ReadEgressCase{Name: "review-policy-reconciliation-tasks", Fixture: fixture,
		BaselineRevision: "candidate-revision:retained-full-api+consumer-fold",
		Baseline: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			tasks, err := st.ListTasks(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			statuses, err := st.ListOpenReviewPolicyStatuses(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			open := map[string]bool{}
			for _, status := range statuses {
				open[status.TaskID] = true
			}
			selected := []core.Task{}
			for _, task := range tasks {
				if !core.TaskTerminal(task.State) || open[task.ID] {
					selected = append(selected, task)
				}
			}
			return scopedReconciliationOutcome(selected)
		},
		Candidate: func(ctx context.Context) (storetest.ReadEgressOutcome, error) {
			tasks, err := st.ListReviewPolicyReconciliationTasks(ctx)
			if err != nil {
				return storetest.ReadEgressOutcome{}, err
			}
			return scopedReconciliationOutcome(tasks)
		}}
}

func scopedReconciliationOutcome(tasks []core.Task) (storetest.ReadEgressOutcome, error) {
	selected := storetest.ReduceReadEgressScopedReconciliationTasks(tasks)
	digest, err := storetest.ReadEgressDigest(selected)
	return storetest.ReadEgressOutcome{Digest: digest, Successes: len(selected)}, err
}
