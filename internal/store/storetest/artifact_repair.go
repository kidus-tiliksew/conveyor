package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func artifactJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func repairActor(ctx context.Context) context.Context {
	return store.WithActor(ctx, store.Actor{ID: "user:repair-operator", Role: core.ActorUser})
}

func runArtifactRepair(t *testing.T, x Fixture) {
	st, ctx := x.Backend, repairActor(x.Context)
	if x.SeedArtifact == nil {
		t.Fatal("artifact repair requires historical fixture seeding")
	}
	content := artifactJPEG(t)
	a, err := st.CreateArtifact(ctx, core.Artifact{Name: "historical.png", ContentType: "image/jpeg"}, content)
	if err != nil {
		t.Fatal(err)
	}
	a.ContentType = "image/png"
	x.SeedArtifact(t, ctx, a, content)
	r := store.ArtifactRepairRequest{ArtifactID: a.ID, ExpectedOldContentType: "image/png", NewContentType: "image/jpeg", RequestID: "repair-1", DryRun: true}
	before, _ := x.ArtifactRepairEvents(ctx)
	preview, err := st.RepairArtifactMetadata(ctx, r)
	if err != nil || !preview.WouldChange || preview.DetectedContentType != "image/jpeg" {
		t.Fatalf("dry run: %+v %v", preview, err)
	}
	got, raw, err := st.GetArtifact(ctx, a.ID)
	if err != nil || got.ContentType != "image/png" || !bytes.Equal(raw, content) {
		t.Fatalf("dry run wrote: %+v %v", got, err)
	}
	after, _ := x.ArtifactRepairEvents(ctx)
	if len(after) != len(before) {
		t.Fatal("dry run appended event")
	}
	// A validated duplicate must report the stored metadata without repairing it.
	duplicate, err := st.CreateArtifact(ctx, core.Artifact{Name: "other.jpg", ContentType: "image/jpeg"}, content)
	if err != nil || duplicate.ContentType != "image/png" || duplicate.Name != a.Name {
		t.Fatalf("dedup changed metadata: %+v %v", duplicate, err)
	}
	r.DryRun = false
	var wg sync.WaitGroup
	results := make([]store.ArtifactRepairResult, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = st.RepairArtifactMetadata(ctx, r) }(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != preview {
			t.Fatalf("replay %d: %+v %v", i, results[i], errs[i])
		}
	}
	got, raw, err = st.GetArtifact(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := a
	expected.ContentType = "image/jpeg"
	if !reflect.DeepEqual(got, expected) || !bytes.Equal(raw, content) {
		t.Fatalf("immutable fields changed: got %+v expected %+v", got, expected)
	}
	after, err = x.ArtifactRepairEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changes := 0
	for _, e := range after {
		if e.Kind == "artifact.metadata_repaired" {
			changes++
			if e.ActorID != "user:repair-operator" || !bytes.Contains(e.Payload, []byte(a.ID)) {
				t.Fatalf("audit provenance: %+v", e)
			}
		}
	}
	if changes != 1 {
		t.Fatalf("change events: %d", changes)
	}
	freshPreview := r
	freshPreview.DryRun = true
	if _, err := st.RepairArtifactMetadata(ctx, freshPreview); !errors.Is(err, store.ErrArtifactRepairConflict) {
		t.Fatalf("recorded preview must revalidate current old type: %v", err)
	}
	conflict := r
	conflict.NewContentType = "image/png"
	for _, dry := range []bool{false, true} {
		conflict.DryRun = dry
		if _, err = st.RepairArtifactMetadata(ctx, conflict); !errors.Is(err, store.ErrArtifactRepairConflict) {
			t.Fatalf("reused key: %v", err)
		}
	}
	stale := r
	stale.RequestID = "stale"
	if _, err = st.RepairArtifactMetadata(ctx, stale); !errors.Is(err, store.ErrArtifactRepairConflict) {
		t.Fatalf("stale old type: %v", err)
	}
	noop := r
	noop.RequestID = "noop"
	noop.ExpectedOldContentType = "image/jpeg"
	result, err := st.RepairArtifactMetadata(ctx, noop)
	if err != nil || result.WouldChange {
		t.Fatalf("no-op: %+v %v", result, err)
	}
	again, err := st.RepairArtifactMetadata(ctx, noop)
	if err != nil || again != result {
		t.Fatalf("no-op replay: %+v %v", again, err)
	}
	noOpEvents, eventErr := x.ArtifactRepairEvents(ctx)
	if eventErr != nil || len(noOpEvents) != len(after) {
		t.Fatalf("no-op or refusal appended event: %d %d %v", len(after), len(noOpEvents), eventErr)
	}
	// Preserve every task ownership link and row provenance across repair.
	linkedBytes := testimage.JPEG("linked")
	var linked core.Artifact
	for _, id := range []string{"artifact-owner-one", "artifact-owner-two"} {
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Branch: "conveyor/" + id, State: core.TaskQueued, Title: id, Body: id, CreatedAt: time.Now().UTC()}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		linked, err = st.CreateArtifact(ctx, core.Artifact{TaskID: id, Name: "linked.jpg", ContentType: "image/jpeg"}, linkedBytes)
		if err != nil {
			t.Fatal(err)
		}
	}
	linked.ContentType = "image/png"
	x.SeedArtifact(t, ctx, linked, linkedBytes)
	linksBefore, err := st.ListArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.RepairArtifactMetadata(ctx, store.ArtifactRepairRequest{ArtifactID: linked.ID, ExpectedOldContentType: "image/png", NewContentType: "image/jpeg", RequestID: "linked"}); err != nil {
		t.Fatal(err)
	}
	linksAfter, err := st.ListArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	beforeByOwner := map[string]core.Artifact{}
	afterByOwner := map[string]core.Artifact{}
	for _, a := range linksBefore {
		if a.ID == linked.ID {
			a.ContentType = "image/jpeg"
			beforeByOwner[a.TaskID] = a
		}
	}
	for _, a := range linksAfter {
		if a.ID == linked.ID {
			afterByOwner[a.TaskID] = a
		}
	}
	if len(beforeByOwner) != 2 || !reflect.DeepEqual(beforeByOwner, afterByOwner) {
		t.Fatalf("repair changed links: %+v %+v", beforeByOwner, afterByOwner)
	}
	if _, err = st.RepairArtifactMetadata(store.WithWorkspace(ctx, x.Workspace+"-foreign"), r); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign workspace: %v", err)
	}
	foreign := store.WithWorkspace(ctx, x.Workspace+"-second")
	cfg := &config.Config{Workspace: x.Workspace + "-second", Repos: x.Config.Repos}
	if _, err = st.BootstrapWorkspaceConfig(foreign, cfg); err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateArtifact(foreign, core.Artifact{Name: "second", ContentType: "image/jpeg"}, content)
	if err != nil {
		t.Fatal(err)
	}
	other.ContentType = "image/png"
	x.SeedArtifact(t, foreign, other, content)
	if _, err = st.RepairArtifactMetadata(foreign, r); err != nil {
		t.Fatalf("workspace request key independence: %v", err)
	}
	// An event failure must roll back both metadata and retry receipt.
	a.ContentType = "image/png"
	x.SeedArtifact(t, ctx, a, content)
	failed := r
	failed.RequestID = "audit-failure"
	badActor := store.WithActor(ctx, store.Actor{ID: "user:bad\x00actor", Role: core.ActorUser})
	if _, err = st.RepairArtifactMetadata(badActor, failed); err == nil {
		t.Fatal("bad audit succeeded")
	}
	got, _, _ = st.GetArtifact(ctx, a.ID)
	if got.ContentType != "image/png" {
		t.Fatal("audit failure changed metadata")
	}
	if _, err = st.RepairArtifactMetadata(ctx, failed); err != nil {
		t.Fatalf("failed audit consumed receipt: %v", err)
	}
	for _, kind := range []string{"hash", "size", "corrupt", "unsupported"} {
		t.Run(kind, func(t *testing.T) {
			b := append([]byte(nil), content...)
			broken := a
			broken.ContentType = "image/png"
			switch kind {
			case "hash":
				b[0] ^= 1
			case "size":
				broken.SizeBytes++
			case "corrupt":
				b = b[:20]
				broken.SizeBytes = int64(len(b))
				broken.ID = fmt.Sprintf("%x", sha256.Sum256(b))
				seeded, err := st.CreateArtifact(ctx, core.Artifact{Name: "corrupt", ContentType: "application/binary"}, b)
				if err != nil {
					t.Fatal(err)
				}
				broken = seeded
				broken.ContentType = "image/png"
			}
			x.SeedArtifact(t, ctx, broken, b)
			bad := r
			bad.ArtifactID = broken.ID
			bad.RequestID = "bad-" + kind
			if kind == "unsupported" {
				bad.NewContentType = "image/svg+xml"
			}
			if _, err := st.RepairArtifactMetadata(ctx, bad); !errors.Is(err, store.ErrArtifactRepairInvalid) {
				t.Fatalf("invalid fixture: %v", err)
			}
		})
	}
}

func runArtifactIntake(t *testing.T, x Fixture) {
	ctx, st := repairActor(x.Context), x.Backend
	tsk := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-intake", Body: "attachments", Title: "attachments", State: core.TaskQueued, CreatedAt: time.Now().UTC()}
	content := artifactJPEG(t)
	good := store.ArtifactUpload{Name: "image.jpg", ContentType: "image/jpeg", Content: content}
	bad := good
	bad.ContentType = "image/png"
	before, _ := st.ListArtifacts(ctx)
	events, _ := st.ListEvents(ctx, "")
	if err := st.CreateTaskWithAttachments(ctx, tsk, nil, store.TaskContextInput{}, []store.ArtifactUpload{good, bad}); err == nil {
		t.Fatal("second invalid attachment accepted")
	}
	if _, err := st.GetTask(ctx, tsk.ID); err == nil {
		t.Fatal("invalid intake created task")
	}
	after, _ := st.ListArtifacts(ctx)
	afterEvents, _ := st.ListEvents(ctx, "")
	if len(before) != len(after) || len(events) != len(afterEvents) {
		t.Fatal("invalid intake partially persisted")
	}
	if err := st.CreateTaskWithAttachments(store.WithActor(ctx, store.Actor{ID: "user:bad\x00actor", Role: core.ActorUser}), tsk, nil, store.TaskContextInput{}, []store.ArtifactUpload{good}); err == nil {
		t.Fatal("audit failure succeeded")
	}
	if _, err := st.GetTask(ctx, tsk.ID); err == nil {
		t.Fatal("audit failure created task")
	}
	if err := st.CreateTaskWithAttachments(ctx, tsk, nil, store.TaskContextInput{}, []store.ArtifactUpload{good, {Name: "notes.txt", ContentType: "text/plain", Content: []byte("notes")}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(ctx, tsk.ID)
	if err != nil || got.State != core.TaskQueued {
		t.Fatalf("finalized intake: %+v %v", got, err)
	}
	after, err = st.ListArtifacts(ctx)
	if err != nil || len(after) != len(before)+2 {
		t.Fatalf("attachments: %+v %v", after, err)
	}
	for _, a := range after {
		if a.TaskID != tsk.ID {
			t.Fatalf("ownership: %+v", a)
		}
	}
}
