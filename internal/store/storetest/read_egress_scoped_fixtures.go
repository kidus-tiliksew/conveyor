package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The scoped-read measurement fixtures are shared by the PostgreSQL and
// SingleStore cases so both backends compare the same shapes and sizes. Every
// row is written through public store methods; no case writes SQL fixture
// rows. Sizes stay medium by default and are overridable from the
// environment, so a measurement run stays bounded.

func scopedFixtureOK(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func scopedFixtureBody(label string, padBytes int) string {
	return "# " + label + "\n\n" + strings.Repeat("requirement prose. ", padBytes/19+1)
}

func scopedFixtureStatements() []core.RequirementStatement {
	return []core.RequirementStatement{{ID: "REQ-1", Statement: "Preserve history."}}
}

// SeedReadEgressScopedArtifacts creates selected artifact owners of all three
// kinds — two tasks, one requirement, and one planning session — and a set of
// unrelated associations with long names. It includes an orphan artifact, a
// content address shared by two selected owners, two roles on one selected
// owner, and an exactly repeated association, so the read must reproduce
// every selected row (including duplicates) and drop the rest. noise is the
// number of unrelated artifacts; nameBytes pads their names so association
// rows are large.
func SeedReadEgressScopedArtifacts(t testing.TB, ctx context.Context, st store.Store, noise, nameBytes int) (store.ArtifactOwners, map[string]int) {
	t.Helper()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ws, _ := store.WorkspaceFromContext(ctx)
	n := 0
	fileTask := func(title string) string {
		id := "egress-scoped-artifact-task-" + core.NewTaskID()
		scopedFixtureOK(t, st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id,
			Title: title, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}))
		return id
	}
	tasks := []string{fileTask("Selected artifact task A"), fileTask("Selected artifact task B")}
	noiseTask := fileTask("Unrelated artifact task")
	reqTitle := "Selected artifact requirement " + core.NewTaskID()
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "egress-scoped-artifact-req-" + core.NewTaskID(), Title: reqTitle},
		core.RequirementVersion{Content: "# " + reqTitle, Origin: core.RequirementOriginOperator, Statements: scopedFixtureStatements()})
	scopedFixtureOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 1)
	scopedFixtureOK(t, err)
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "egress-scoped-artifact-session-" + core.NewTaskID(), Title: "Selected artifact session " + core.NewTaskID()})
	scopedFixtureOK(t, err)

	attach := func(artifact core.Artifact, content []byte) core.Artifact {
		n++
		artifact.ContentType = "text/plain"
		artifact.CreatedAt = now.Add(time.Duration(n) * time.Second)
		stored, err := st.CreateArtifact(ctx, artifact, content)
		scopedFixtureOK(t, err)
		return stored
	}
	body := func(label string) []byte {
		return []byte(label + "\n" + strings.Repeat("artifact body prose. ", 16))
	}

	attach(core.Artifact{Name: "selected-task.txt", Role: core.ArtifactRoleTaskContext, TaskID: tasks[0]}, body("task"))
	attach(core.Artifact{Name: "selected-requirement.txt", Role: core.ArtifactRoleTaskContext, RequirementID: requirement.ID}, body("requirement"))
	attach(core.Artifact{Name: "selected-session.txt", Role: core.ArtifactRoleTaskContext, PlanningSessionID: session.ID}, body("session"))
	// One content address with two roles on the same selected owner, and one
	// shared by two selected owners: both widens the association rows the
	// selected read must reproduce.
	shared := body("shared")
	attach(core.Artifact{Name: "selected-task-context.txt", Role: core.ArtifactRoleTaskContext, TaskID: tasks[1]}, shared)
	attach(core.Artifact{Name: "selected-task-audit.txt", Role: core.ArtifactRoleGeneratedAudit, TaskID: tasks[1]}, shared)
	attach(core.Artifact{Name: "selected-shared-task.txt", Role: core.ArtifactRoleTaskContext, TaskID: tasks[0]}, body("shared-owner"))
	attach(core.Artifact{Name: "selected-shared-requirement.txt", Role: core.ArtifactRoleTaskContext, RequirementID: requirement.ID}, body("shared-owner"))
	// The exact same association twice stores one link; the read must not
	// invent a second row for it.
	repeated := body("repeated")
	attach(core.Artifact{Name: "selected-repeated.txt", Role: core.ArtifactRoleTaskContext, TaskID: tasks[0]}, repeated)
	attach(core.Artifact{Name: "selected-repeated.txt", Role: core.ArtifactRoleTaskContext, TaskID: tasks[0]}, repeated)
	// An artifact no owner lists is absent from both variants.
	attach(core.Artifact{Name: "orphan.txt"}, body("orphan"))

	padding := strings.Repeat("-association", max(1, nameBytes/12))
	for i := range noise {
		attach(core.Artifact{Name: fmt.Sprintf("noise-%03d.txt%s", i, padding), Role: core.ArtifactRoleTaskContext, TaskID: noiseTask}, body(fmt.Sprintf("noise-%03d", i)))
	}
	owners := store.ArtifactOwners{TaskIDs: tasks, RequirementIDs: []string{requirement.ID}, SessionIDs: []string{session.ID}}
	return owners, map[string]int{
		"selected_tasks": 2, "selected_requirements": 1, "selected_sessions": 1,
		"selected_artifacts": 6, "shared_associations": 3, "repeated_association_calls": 2, "orphan_artifacts": 1,
		"noise_artifacts": noise, "noise_name_bytes": nameBytes,
	}
}

// ReadEgressScopedArtifactsForOwners keeps the rows the selected owners list
// and stamps the download URL the API serves. It is the consumer projection
// both variants apply, so a row-set or metadata difference still changes the
// digest.
func ReadEgressScopedArtifactsForOwners(listed []core.Artifact, owners store.ArtifactOwners) []core.Artifact {
	tasks := map[string]bool{}
	for _, id := range owners.TaskIDs {
		tasks[id] = true
	}
	requirements := map[string]bool{}
	for _, id := range owners.RequirementIDs {
		requirements[id] = true
	}
	sessions := map[string]bool{}
	for _, id := range owners.SessionIDs {
		sessions[id] = true
	}
	out := []core.Artifact{}
	for _, artifact := range listed {
		if !tasks[artifact.TaskID] && !requirements[artifact.RequirementID] && !sessions[artifact.PlanningSessionID] {
			continue
		}
		artifact.DownloadURL = "/v1/artifacts/" + artifact.ID
		out = append(out, artifact)
	}
	return out
}

// ReadEgressScopedArtifactDigest preserves the SQL order while resolving only
// ties: both queries order by created_at, artifact ID, and role, leaving
// associations with identical keys unordered.
func ReadEgressScopedArtifactDigest(rows []core.Artifact) (string, error) {
	encoded := make([]json.RawMessage, len(rows))
	for i := range rows {
		value, err := json.Marshal(rows[i])
		if err != nil {
			return "", err
		}
		encoded[i] = value
	}
	for start := 0; start < len(rows); {
		end := start + 1
		for end < len(rows) && rows[start].CreatedAt.Equal(rows[end].CreatedAt) && rows[start].ID == rows[end].ID && rows[start].Role == rows[end].Role {
			end++
		}
		sort.Slice(encoded[start:end], func(i, j int) bool {
			return string(encoded[start+i]) < string(encoded[start+j])
		})
		start = end
	}
	return ReadEgressDigest(encoded)
}

// ReadEgressScopedRequirementSummary is the requirement list's served
// version selection: the confirmed current version and the unconfirmed,
// unretired pending versions. Historical content is not served.
type ReadEgressScopedRequirementSummary struct {
	ID      string                    `json:"id"`
	Current *core.RequirementVersion  `json:"current_version,omitempty"`
	Pending []core.RequirementVersion `json:"pending_versions"`
}

// SelectReadEgressScopedRequirementSummaries applies the requirement list's
// existing selection to either variant's version map.
func SelectReadEgressScopedRequirementSummaries(requirements []core.Requirement, versions map[string][]core.RequirementVersion) []ReadEgressScopedRequirementSummary {
	out := make([]ReadEgressScopedRequirementSummary, 0, len(requirements))
	for _, requirement := range requirements {
		summary := ReadEgressScopedRequirementSummary{ID: requirement.ID, Pending: []core.RequirementVersion{}}
		for _, version := range versions[requirement.ID] {
			if version.Version == requirement.CurrentVersion && version.Confirmed {
				current := version
				summary.Current = &current
			}
			if !version.Confirmed && !version.Retired {
				summary.Pending = append(summary.Pending, version)
			}
		}
		out = append(out, summary)
	}
	return out
}

// SeedReadEgressScopedRequirementVersions creates the selected requirements
// whose version history differs from the pending selection. The first
// selected requirement owns history confirmed versions with large bodies, one
// pending version with origin and statement metadata, and its current
// version; the second owns two confirmed versions and one pending version.
// An archived requirement and noise unrelated requirements with large
// current bodies are never requested. It returns the requested IDs and
// fixture counts.
func SeedReadEgressScopedRequirementVersions(t testing.TB, ctx context.Context, st store.Store, history, noise, padBytes int) ([]string, map[string]int) {
	t.Helper()
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "egress-scoped-version-session-" + core.NewTaskID(), Title: "Scoped version session " + core.NewTaskID()})
	scopedFixtureOK(t, err)
	file := func(title string, body string) core.Requirement {
		actual := title + " " + core.NewTaskID()
		requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "egress-scoped-req-" + core.NewTaskID(), Title: actual},
			core.RequirementVersion{Content: readEgressTitleH1(actual, body), Origin: core.RequirementOriginOperator, Statements: scopedFixtureStatements()})
		scopedFixtureOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 1)
		scopedFixtureOK(t, err)
		return requirement
	}
	confirmed := func(requirementID string, version int, body string, origin core.RequirementOrigin) {
		versionStruct := core.RequirementVersion{RequirementID: requirementID, Content: body, Origin: origin, Statements: scopedFixtureStatements()}
		if origin == core.RequirementOriginChat {
			versionStruct.OriginSessionID = session.ID
		}
		_, err := st.ProposeRequirementVersion(ctx, versionStruct)
		scopedFixtureOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, requirementID, version, version-1)
		scopedFixtureOK(t, err)
	}
	pending := func(requirementID string, body string, origin core.RequirementOrigin) {
		versionStruct := core.RequirementVersion{RequirementID: requirementID, Content: body, Origin: origin, Statements: scopedFixtureStatements()}
		if origin == core.RequirementOriginChat {
			versionStruct.OriginSessionID = session.ID
		}
		_, err := st.ProposeRequirementVersion(ctx, versionStruct)
		scopedFixtureOK(t, err)
	}

	selected := file("Scoped requirement versions", "# Scoped requirement versions")
	for version := 2; version <= max(1, history); version++ {
		origin := core.RequirementOriginOperator
		if version%10 == 0 {
			origin = core.RequirementOriginChat
		}
		confirmed(selected.ID, version, scopedFixtureBody(fmt.Sprintf("Scoped requirement version %d", version), padBytes), origin)
	}
	pending(selected.ID, "# Scoped pending requirement\n\nPending prose.", core.RequirementOriginOperator)

	second := file("Scoped requirement second", "# Scoped requirement second")
	confirmed(second.ID, 2, "# Scoped requirement second v2", core.RequirementOriginOperator)
	pending(second.ID, "# Scoped requirement second pending", core.RequirementOriginChat)

	archived := file("Archived requirement", scopedFixtureBody("Archived requirement", padBytes))
	scopedFixtureOK(t, st.ArchiveRequirement(ctx, archived.ID, "fixture", nil))

	for i := range noise {
		file(fmt.Sprintf("Unrelated requirement %03d", i), scopedFixtureBody(fmt.Sprintf("Unrelated requirement %03d", i), padBytes))
	}
	return []string{selected.ID, second.ID}, map[string]int{
		"selected_requirements": 2, "confirmed_versions": max(1, history) + 2, "historical_versions": max(1, history),
		"pending_versions": 2, "archived_requirements": 1, "noise_requirements": noise, "history_content_bytes": padBytes,
	}
}

// ReadEgressScopedDesignVersion is the System Design collection's served
// version summary: immutable content, governed paths, and the version
// history of the per-document endpoint are not repeated.
type ReadEgressScopedDesignVersion struct {
	DocumentID      string                  `json:"document_id"`
	Version         int                     `json:"version"`
	Origin          core.SystemDesignOrigin `json:"origin"`
	OriginSessionID string                  `json:"origin_session_id,omitempty"`
	OriginTaskID    string                  `json:"origin_task_id,omitempty"`
	Confirmed       bool                    `json:"confirmed"`
	ConfirmedBy     string                  `json:"confirmed_by,omitempty"`
	ConfirmedAt     time.Time               `json:"confirmed_at,omitempty"`
	Dismissed       bool                    `json:"dismissed"`
	DismissedBy     string                  `json:"dismissed_by,omitempty"`
	DismissedAt     time.Time               `json:"dismissed_at,omitempty"`
	Workspace       string                  `json:"workspace"`
	CreatedAt       time.Time               `json:"created_at"`
}

// ReadEgressScopedDesignSummary is one served collection entry.
type ReadEgressScopedDesignSummary struct {
	ID      string                          `json:"id"`
	Current *ReadEgressScopedDesignVersion  `json:"current_version,omitempty"`
	Pending []ReadEgressScopedDesignVersion `json:"pending_versions"`
}

func scopedDesignVersionSummary(version core.SystemDesignVersion) ReadEgressScopedDesignVersion {
	return ReadEgressScopedDesignVersion{
		DocumentID: version.DocumentID, Version: version.Version, Origin: version.Origin,
		OriginSessionID: version.OriginSessionID, OriginTaskID: version.OriginTaskID,
		Confirmed: version.Confirmed, ConfirmedBy: version.ConfirmedBy, ConfirmedAt: version.ConfirmedAt,
		Dismissed: version.Dismissed, DismissedBy: version.DismissedBy, DismissedAt: version.DismissedAt,
		Workspace: version.Workspace, CreatedAt: version.CreatedAt,
	}
}

// SelectReadEgressScopedDesignSummaries applies the System Design
// collection's existing selection to either variant's version map: the
// current version and the unconfirmed, undismissed pending versions.
func SelectReadEgressScopedDesignSummaries(items []core.SystemDesign, versions map[string][]core.SystemDesignVersion) []ReadEgressScopedDesignSummary {
	out := make([]ReadEgressScopedDesignSummary, 0, len(items))
	for _, item := range items {
		summary := ReadEgressScopedDesignSummary{ID: item.ID, Pending: []ReadEgressScopedDesignVersion{}}
		for _, version := range versions[item.ID] {
			if version.Version == item.CurrentVersion {
				current := scopedDesignVersionSummary(version)
				summary.Current = &current
			}
			if !version.Confirmed && !version.Dismissed {
				summary.Pending = append(summary.Pending, scopedDesignVersionSummary(version))
			}
		}
		out = append(out, summary)
	}
	return out
}

// SeedReadEgressScopedSystemDesignVersions is the System Design analogue:
// the first selected design owns history confirmed versions with large
// bodies, one pending version, and one dismissed version; the second owns
// two confirmed versions and one pending version. An archived design and
// noise unrelated designs with large current bodies are never requested.
func SeedReadEgressScopedSystemDesignVersions(t testing.TB, ctx context.Context, st store.Store, history, noise, padBytes int) ([]string, map[string]int) {
	t.Helper()
	content := func(body string) string {
		return body + "\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```\n"
	}
	file := func(title string, body string) core.SystemDesign {
		actual := title + " " + core.NewTaskID()
		design, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "egress-scoped-design-" + core.NewTaskID(), Title: actual, Category: "Component design"},
			core.SystemDesignVersion{Content: content(readEgressTitleH1(actual, body)), Origin: core.SystemDesignOriginOperator})
		scopedFixtureOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, 1)
		scopedFixtureOK(t, err)
		return design
	}
	confirm := func(documentID string, version int, body string) {
		_, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: documentID, Content: content(body), Origin: core.SystemDesignOriginOperator})
		scopedFixtureOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, documentID, version, version-1)
		scopedFixtureOK(t, err)
	}
	propose := func(documentID string, body string) {
		_, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: documentID, Content: content(body), Origin: core.SystemDesignOriginOperator})
		scopedFixtureOK(t, err)
	}

	selected := file("Scoped design versions", "# Scoped design versions")
	for version := 2; version <= max(1, history); version++ {
		confirm(selected.ID, version, scopedFixtureBody(fmt.Sprintf("Scoped design version %d", version), padBytes))
	}
	propose(selected.ID, "# Scoped design pending\n\nPending prose.")
	dismissed := max(1, history) + 2
	propose(selected.ID, "# Scoped design dismissed\n\nDismissed prose.")
	_, _, err := st.DismissSystemDesignVersion(ctx, selected.ID, dismissed)
	scopedFixtureOK(t, err)

	second := file("Scoped design second", "# Scoped design second")
	confirm(second.ID, 2, "# Scoped design second v2")
	propose(second.ID, "# Scoped design second pending")

	archived := file("Archived design", scopedFixtureBody("Archived design", padBytes))
	scopedFixtureOK(t, st.ArchiveSystemDesign(ctx, archived.ID, "fixture", nil))

	for i := range noise {
		file(fmt.Sprintf("Unrelated design %03d", i), scopedFixtureBody(fmt.Sprintf("Unrelated design %03d", i), padBytes))
	}
	return []string{selected.ID, second.ID}, map[string]int{
		"selected_designs": 2, "confirmed_versions": max(1, history) + 2, "historical_versions": max(1, history),
		"pending_versions": 2, "dismissed_versions": 1, "archived_designs": 1, "noise_designs": noise, "history_content_bytes": padBytes,
	}
}

// SeedReadEgressScopedReconciliationTasks creates the startup review-policy
// reconciliation fixture: active tasks, a terminal task with an open
// status row, terminal tasks whose only status rows are published, and
// unrelated terminal tasks with no row at all. Task bodies are large so the
// whole-row list carries them while the scoped read does not. It returns
// fixture counts.
func SeedReadEgressScopedReconciliationTasks(t testing.TB, ctx context.Context, st store.Store, noise, bodyBytes int) map[string]int {
	t.Helper()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	ws, _ := store.WorkspaceFromContext(ctx)
	body := strings.Repeat("task body prose. ", bodyBytes/17+1)
	n := 0
	file := func(state core.TaskState) string {
		n++
		id := "egress-scoped-reconcile-" + core.NewTaskID()
		task := core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id,
			Title: "Reconcile " + id, Body: body, State: state, NextStage: core.StageImplement, CreatedAt: now.Add(time.Duration(n) * time.Second)}
		task.SetupContract = config.PolicyContract{
			MaxBounces:    3,
			Risk:          &riskpolicy.Policy{Mode: riskpolicy.ModeEnforce, Tiers: riskpolicy.DefaultTable()},
			Review:        config.ReviewPanel{Seats: []config.ReviewSeat{{}}},
			StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"},
		}
		scopedFixtureOK(t, st.CreateTask(ctx, task))
		return id
	}
	queue := func(taskID, head string) core.ReviewPolicyStatus {
		row, _, err := st.QueueReviewPolicyStatus(ctx, taskID, head, core.PolicyStatusPending)
		scopedFixtureOK(t, err)
		return row
	}
	publish := func(row core.ReviewPolicyStatus) {
		row.State, row.Published = core.ReviewPolicyStatusPublished, core.PolicyStatusSuccess
		_, err := st.UpdateReviewPolicyStatus(ctx, row)
		scopedFixtureOK(t, err)
	}

	for _, state := range []core.TaskState{core.TaskRunning, core.TaskQueued, core.TaskApproved} {
		queue(file(state), "head-"+string(state))
	}
	openTerminal := file(core.TaskMerged)
	queue(openTerminal, "head-open")
	published := 0
	for i := range max(1, noise/10) {
		publish(queue(file(core.TaskMerged), fmt.Sprintf("head-published-%d", i)))
		published++
	}
	for range noise {
		file(core.TaskClosed)
	}
	return map[string]int{
		"active_tasks": 3, "open_terminal_tasks": 1, "published_terminal_tasks": published,
		"noise_terminal_tasks": noise, "task_body_bytes": bodyBytes,
	}
}

// ReduceReadEgressScopedReconciliationTasks keeps only the fields the
// startup pass consumes and canonicalizes the set by task ID. The pass
// repairs an unordered set, so ordering is not part of its served work; a
// membership or projection difference still changes the digest.
func ReduceReadEgressScopedReconciliationTasks(tasks []core.Task) []core.Task {
	out := make([]core.Task, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, core.Task{ID: task.ID, State: task.State, SetupContract: task.SetupContract})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ReadEgressRequirementSelections pairs each listed requirement with the
// current version its row named, as the requirement list passes it to the
// summary read.
func ReadEgressRequirementSelections(requirements []core.Requirement) []store.DocumentVersionSelection {
	out := make([]store.DocumentVersionSelection, 0, len(requirements))
	for _, requirement := range requirements {
		out = append(out, store.DocumentVersionSelection{ID: requirement.ID, CurrentVersion: requirement.CurrentVersion})
	}
	return out
}

// ReadEgressSystemDesignSelections pairs each listed System Design with the
// current version its row named, as the System Design list passes it to the
// summary read.
func ReadEgressSystemDesignSelections(items []core.SystemDesign) []store.DocumentVersionSelection {
	out := make([]store.DocumentVersionSelection, 0, len(items))
	for _, item := range items {
		out = append(out, store.DocumentVersionSelection{ID: item.ID, CurrentVersion: item.CurrentVersion})
	}
	return out
}
