package storetest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func runArtifactReadScoping(t *testing.T, x Fixture) {
	t.Helper()
	st, ctx := x.Backend, x.Context
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	task := core.Task{ID: "artifact-scope-" + core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskQueued, CreatedAt: now}
	task.Branch = "conveyor/task-" + task.ID
	noise := core.Task{ID: "artifact-noise-" + core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskQueued, CreatedAt: now}
	noise.Branch = "conveyor/task-" + noise.ID
	requireOK(t, st.CreateTask(ctx, task))
	requireOK(t, st.CreateTask(ctx, noise))
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "artifact-session-" + core.NewTaskID(), Title: "Scoped session"})
	requireOK(t, err)
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "artifact-req-" + core.NewTaskID(), Title: "Scoped requirement"}, core.RequirementVersion{
		Content: "# Scoped", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep owner reads isolated."}},
	})
	requireOK(t, err)

	const payloadSize = 4096
	sentinel := []byte("CONTENT-SENTINEL-ab2c4b")
	payload := bytes.Repeat(sentinel, payloadSize/len(sentinel)+1)[:payloadSize]
	equalCount := 110
	for i := range equalCount {
		body := append(append([]byte(nil), payload...), []byte(fmt.Sprintf("\nequal-%03d-%s", i, task.ID))...)
		_, err := st.CreateArtifact(ctx, core.Artifact{Name: fmt.Sprintf("equal-%03d.txt", i), ContentType: "text/plain", TaskID: task.ID, CreatedAt: now}, body)
		requireOK(t, err)
	}
	for i := range 20 {
		body := append(append([]byte(nil), payload...), []byte(fmt.Sprintf("\nnoise-%03d-%s", i, noise.ID))...)
		_, err := st.CreateArtifact(ctx, core.Artifact{Name: fmt.Sprintf("noise-%03d.txt", i), ContentType: "text/plain", TaskID: noise.ID, CreatedAt: now}, body)
		requireOK(t, err)
	}
	orphan, err := st.CreateArtifact(ctx, core.Artifact{Name: "orphan.txt", ContentType: "text/plain", CreatedAt: now}, append(append([]byte(nil), payload...), []byte("\norphan-"+task.ID)...))
	requireOK(t, err)
	shared := append(append([]byte(nil), payload...), []byte("\nshared-"+task.ID)...)
	first, err := st.CreateArtifact(ctx, core.Artifact{Name: "shared.txt", ContentType: "text/plain", TaskID: task.ID, CreatedAt: now}, shared)
	requireOK(t, err)
	duplicate, err := st.CreateArtifact(ctx, core.Artifact{Name: "shared-req.txt", ContentType: "text/plain", RequirementID: requirement.ID, CreatedAt: now}, shared)
	requireOK(t, err)
	if duplicate.ID != first.ID {
		t.Fatalf("duplicate content address = %s, want %s", duplicate.ID, first.ID)
	}
	sessionArt, err := st.CreateArtifact(ctx, core.Artifact{Name: "session.txt", ContentType: "text/plain", PlanningSessionID: session.ID, CreatedAt: now}, append(append([]byte(nil), payload...), []byte("\nsession-"+session.ID)...))
	requireOK(t, err)

	reader, ok := st.(store.ArtifactOwnerReader)
	if !ok {
		t.Fatal("store does not implement ArtifactOwnerReader")
	}
	// Each metric gets its own listing window: drain that metric, read once,
	// then drain the same metric to measure the operation.
	measureListing := func(drain func() map[string]uint64, label string) uint64 {
		drain()
		_, listErr := st.ListArtifacts(ctx)
		requireOK(t, listErr)
		return drain()[label]
	}

	listed, err := st.ListArtifacts(ctx)
	requireOK(t, err)
	encoded, err := json.Marshal(listed)
	requireOK(t, err)
	if bytes.Contains(encoded, sentinel) {
		t.Fatal("ListArtifacts body carried artifact content bytes")
	}
	if x.DrainReadBytes != nil {
		got := measureListing(x.DrainReadBytes, "ListArtifacts")
		contentBytes := uint64(payloadSize * (equalCount + 20 + 3))
		if got == 0 || got >= contentBytes {
			t.Fatalf("ListArtifacts received %d bytes, want below %d content bytes", got, contentBytes)
		}
	}
	if x.DrainReadQueries != nil {
		queries := measureListing(x.DrainReadQueries, "ListArtifacts")
		if len(listed) > store.ArtifactMetadataPageSize && queries < 5 {
			t.Fatalf("ListArtifacts statements = %d, want snapshot plus paged hydrations", queries)
		}
	}
	again, err := st.ListArtifacts(ctx)
	requireOK(t, err)
	if !artifactAssocEqual(listed, again) {
		t.Fatal("ListArtifacts body changed across identical reads")
	}

	var orphanRows, sharedTaskRows, sessionRows, noiseRows, ownedRows int
	for _, artifact := range listed {
		if artifact.DownloadURL != "" {
			t.Fatalf("listing set a download URL: %+v", artifact)
		}
		if artifact.ID == orphan.ID {
			orphanRows++
		}
		if artifact.ID == first.ID && artifact.TaskID == task.ID {
			sharedTaskRows++
		}
		if artifact.ID == sessionArt.ID {
			sessionRows++
		}
		if artifact.TaskID == noise.ID {
			noiseRows++
		}
		if artifact.TaskID == task.ID {
			ownedRows++
		}
	}
	if orphanRows != 1 {
		t.Fatalf("orphan metadata rows = %d, want 1", orphanRows)
	}
	// The task link is the original insert, so it survives any duplicate
	// collapse. The requirement link of the duplicate upload is backend
	// dependent and is checked against the retained-list oracle below.
	if sharedTaskRows != 1 {
		t.Fatalf("shared artifact task association rows = %d, want 1", sharedTaskRows)
	}
	if sessionRows != 1 {
		t.Fatalf("session association rows = %d, want 1", sessionRows)
	}
	if noiseRows != 20 {
		t.Fatalf("noise association rows = %d, want 20", noiseRows)
	}
	// Counted independently of the shared artifact: the shared task link is
	// one of the task's equalCount+1 associations.
	if ownedRows != equalCount+1 {
		t.Fatalf("task association rows = %d, want %d", ownedRows, equalCount+1)
	}
	if len(listed) <= store.ArtifactMetadataPageSize {
		t.Fatalf("fixture listed %d rows, want more than one %d-artifact page", len(listed), store.ArtifactMetadataPageSize)
	}

	if x.DrainReadRows != nil {
		x.DrainReadRows()
	}
	// 133 requested task IDs cross the owner chunk boundary; the task repeats
	// and an empty ID rides along. The retained listing is the oracle: the
	// owner read must return exactly its task rows, in order, with duplicates.
	taskOwners := []string{task.ID, task.ID, ""}
	for i := range 130 {
		taskOwners = append(taskOwners, fmt.Sprintf("missing-%03d", i))
	}
	owned, err := reader.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: taskOwners})
	requireOK(t, err)
	if x.DrainReadRows != nil {
		if got := x.DrainReadRows()["ListArtifactsForOwners"]; got != uint64(len(owned)) {
			t.Fatalf("ListArtifactsForOwners returned %d rows for %d associations", got, len(owned))
		}
	}
	requireExactArtifacts(t, "task owner", owned, retainedOwnerOracle(listed, store.ArtifactOwners{TaskIDs: []string{task.ID}}))
	for _, artifact := range owned {
		if artifact.ID == orphan.ID || artifact.ID == sessionArt.ID {
			t.Fatalf("task owner read included non-task row %+v", artifact)
		}
	}
	if len(owned) != equalCount+1 {
		t.Fatalf("task owner associations = %d, want %d", len(owned), equalCount+1)
	}
	// Noise rows join only when their task is requested among the same
	// 130+ owner IDs: the read returns the retained rows of both tasks.
	mixed, err := reader.ListArtifactsForOwners(ctx, store.ArtifactOwners{TaskIDs: append([]string{noise.ID}, taskOwners...)})
	requireOK(t, err)
	requireExactArtifacts(t, "task and noise owners", mixed, retainedOwnerOracle(listed, store.ArtifactOwners{TaskIDs: []string{task.ID, noise.ID}}))
	reqs, err := reader.ListArtifactsForOwners(ctx, store.ArtifactOwners{RequirementIDs: []string{requirement.ID}})
	requireOK(t, err)
	if len(reqs) != 1 || reqs[0].ID != first.ID || reqs[0].RequirementID != requirement.ID {
		t.Fatalf("duplicate upload requirement association = %+v, want exactly artifact %s owned by requirement %s", reqs, first.ID, requirement.ID)
	}
	requireExactArtifacts(t, "requirement owner", reqs, retainedOwnerOracle(listed, store.ArtifactOwners{RequirementIDs: []string{requirement.ID}}))
	for _, artifact := range reqs {
		if artifact.RequirementID != requirement.ID {
			t.Fatalf("requirement owner read leaked %+v", artifact)
		}
	}
	sessions, err := reader.ListArtifactsForOwners(ctx, store.ArtifactOwners{SessionIDs: []string{session.ID}})
	requireOK(t, err)
	if len(sessions) != 1 || sessions[0].ID != sessionArt.ID {
		t.Fatalf("session owner associations = %+v", sessions)
	}
	empty, err := reader.ListArtifactsForOwners(ctx, store.ArtifactOwners{})
	requireOK(t, err)
	if len(empty) != 0 {
		t.Fatalf("empty owners returned %+v", empty)
	}
	lineage, err := st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}})
	requireOK(t, err)
	if !artifactAssocEqual(lineage, owned) {
		t.Fatalf("task lineage associations %v, owner associations %v", artifactAssocKeys(lineage), artifactAssocKeys(owned))
	}

	// Other conformance cases may already have newer artifacts in this workspace.
	// Place the concurrent insert beyond the actual forward key boundary.
	afterFence := now.Add(2 * time.Hour)
	for _, artifact := range listed {
		if !afterFence.After(artifact.CreatedAt) {
			afterFence = artifact.CreatedAt.Add(time.Hour)
		}
	}
	inserted := core.Artifact{}
	store.ArtifactListPageHook = func() {
		if inserted.ID != "" {
			return
		}
		created, createErr := st.CreateArtifact(ctx, core.Artifact{Name: "concurrent.txt", ContentType: "text/plain", TaskID: noise.ID, CreatedAt: afterFence}, append(append([]byte(nil), payload...), []byte("\nconcurrent-"+core.NewTaskID())...))
		if createErr != nil {
			t.Errorf("concurrent insert: %v", createErr)
			return
		}
		inserted = created
	}
	snapshot, err := st.ListArtifacts(ctx)
	store.ArtifactListPageHook = nil
	requireOK(t, err)
	for _, artifact := range snapshot {
		if inserted.ID != "" && artifact.ID == inserted.ID {
			t.Fatalf("listing observed an insert past the forward key boundary: %+v", artifact)
		}
	}
	seen := map[string]int{}
	for _, artifact := range snapshot {
		seen[artifactAssocKey(artifact)]++
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("duplicate listing row %q x%d", key, count)
		}
	}
}

func artifactAssocKey(artifact core.Artifact) string {
	return artifact.ID + "\x00" + string(artifact.Role) + "\x00" + artifact.TaskID + "\x00" + artifact.RequirementID + "\x00" + artifact.PlanningSessionID
}

func artifactAssocKeys(artifacts []core.Artifact) []string {
	keys := make([]string, len(artifacts))
	for i, artifact := range artifacts {
		keys[i] = artifactAssocKey(artifact)
	}
	return keys
}

func artifactAssocEqual(left, right []core.Artifact) bool {
	if len(left) != len(right) {
		return false
	}
	counts := map[string]int{}
	for _, artifact := range left {
		counts[artifactAssocKey(artifact)]++
	}
	for _, artifact := range right {
		key := artifactAssocKey(artifact)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

// retainedOwnerOracle is the old owner filter applied to the retained workspace
// listing: a row belongs to a requested task, requirement, or session by its
// own link fields, with duplicates and listing order preserved.
func retainedOwnerOracle(retained []core.Artifact, owners store.ArtifactOwners) []core.Artifact {
	set := func(ids []string) map[string]bool {
		m := make(map[string]bool, len(ids))
		for _, id := range ids {
			if id != "" {
				m[id] = true
			}
		}
		return m
	}
	tasks, requirements, sessions := set(owners.TaskIDs), set(owners.RequirementIDs), set(owners.SessionIDs)
	out := []core.Artifact{}
	for _, artifact := range retained {
		if tasks[artifact.TaskID] || requirements[artifact.RequirementID] || sessions[artifact.PlanningSessionID] {
			out = append(out, artifact)
		}
	}
	return out
}

// requireExactArtifacts demands the same rows in the same order with every
// field equal, as their JSON wire form.
func requireExactArtifacts(t *testing.T, label string, got, want []core.Artifact) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d rows, retained oracle has %d", label, len(got), len(want))
	}
	for i := range want {
		gotJSON, err := json.Marshal(got[i])
		requireOK(t, err)
		wantJSON, err := json.Marshal(want[i])
		requireOK(t, err)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("%s row %d = %s, want %s", label, i, gotJSON, wantJSON)
		}
	}
}
