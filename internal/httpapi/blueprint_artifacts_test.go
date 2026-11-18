package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// blueprintArtifactCountingStore records the artifact reads a blueprint
// request makes: the workspace-wide listing, and each scoped owner read.
type blueprintArtifactCountingStore struct {
	store.Store
	mu       sync.Mutex
	listed   int
	scoped   int
	owners   []store.ArtifactOwners
	contents int
}

func (c *blueprintArtifactCountingStore) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	c.mu.Lock()
	c.listed++
	c.mu.Unlock()
	return c.Store.ListArtifacts(ctx)
}

func (c *blueprintArtifactCountingStore) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	c.mu.Lock()
	c.contents++
	c.mu.Unlock()
	return c.Store.GetArtifact(ctx, id)
}

func (c *blueprintArtifactCountingStore) ListArtifactsForOwners(ctx context.Context, owners store.ArtifactOwners) ([]core.Artifact, error) {
	c.mu.Lock()
	c.scoped++
	c.owners = append(c.owners, owners)
	c.mu.Unlock()
	return c.Store.ListArtifactsForOwners(ctx, owners)
}

// The blueprint projection reads only the anchors' own association rows
// through the scoped owner read: never the workspace listing, never an
// artifact body, and the public body carries each owned row once with its
// download URL, in creation order, whatever the noise around it.
func TestBlueprintsArtifactsUseScopedOwnerRead(t *testing.T) {
	memory := store.NewMemoryWithConfig(&config.Config{
		Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", Base: "main"}},
	})
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	anchor := materializeBlueprint(t, memory, "anchor-artifacts", []decompositionFixture{{ID: "SUB-1", Repo: "conveyor", Summary: "Only child"}})
	emptyAnchor := materializeBlueprint(t, memory, "anchor-no-artifacts", []decompositionFixture{{ID: "SUB-1", Repo: "conveyor", Summary: "Only child"}})
	noise := core.Task{
		ID: "plain-noise", Workspace: "demo", Repo: "conveyor", BaseBranch: "main",
		Branch: "conveyor/task-plain-noise", State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}
	if err := memory.CreateTask(ctx, noise); err != nil {
		t.Fatal(err)
	}

	const sentinel = "ARTIFACT-BODY-SENTINEL-ab2c4b"
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	create := func(name, owner string, at time.Time, body string) core.Artifact {
		t.Helper()
		artifact, err := memory.CreateArtifact(ctx, core.Artifact{Name: name, ContentType: "text/plain", TaskID: owner, CreatedAt: at}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	// Created out of order so the stored order differs from the public order.
	late := create("late.txt", anchor.ID, base.Add(2*time.Hour), sentinel+" late")
	early := create("early.txt", anchor.ID, base, sentinel+" early")
	shared := create("shared.txt", anchor.ID, base.Add(time.Hour), sentinel+" shared")
	if dup := create("shared-noise.txt", noise.ID, base.Add(time.Hour), sentinel+" shared"); dup.ID != shared.ID {
		t.Fatalf("duplicate content address %s, want %s", dup.ID, shared.ID)
	}
	for i := range 5 {
		create("noise.txt", noise.ID, base, sentinel+" noise "+string(rune('a'+i)))
	}
	create("orphan.txt", "", base, sentinel+" orphan")

	counting := &blueprintArtifactCountingStore{Store: memory}
	server := NewServer(counting)
	server.Workspace, server.BearerToken = "demo", "token"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.Handler().ServeHTTP(w, authenticatedMemoryRead(server, r))
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/blueprints?workspace_id=demo", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte(sentinel)) {
		t.Fatal("public blueprint body carried artifact content bytes")
	}
	views := listBlueprintViews(t, handler)

	if counting.listed != 0 || counting.contents != 0 {
		t.Fatalf("workspace listings=%d body reads=%d, want none", counting.listed, counting.contents)
	}
	if counting.scoped != 2 {
		t.Fatalf("scoped owner reads=%d, want one per request", counting.scoped)
	}
	for _, owners := range counting.owners {
		got := slices.Clone(owners.TaskIDs)
		slices.Sort(got)
		want := []string{emptyAnchor.ID, anchor.ID}
		slices.Sort(want)
		if !slices.Equal(got, want) || len(owners.RequirementIDs) != 0 || len(owners.SessionIDs) != 0 {
			t.Fatalf("scoped read named %+v, want only anchors %v", owners, want)
		}
	}

	byTask := map[string]blueprintView{}
	for _, view := range views {
		byTask[view.Task.ID] = view
	}
	owned := byTask[anchor.ID].Artifacts
	var order []string
	for _, artifact := range owned {
		order = append(order, artifact.ID)
		if artifact.TaskID != anchor.ID {
			t.Fatalf("anchor view leaked %+v", artifact)
		}
		if artifact.DownloadURL != "/v1/artifacts/"+artifact.ID {
			t.Fatalf("download URL %q for %s", artifact.DownloadURL, artifact.ID)
		}
	}
	if want := []string{early.ID, shared.ID, late.ID}; !slices.Equal(order, want) {
		t.Fatalf("anchor artifacts %v, want creation order %v", order, want)
	}
	if empty := byTask[emptyAnchor.ID].Artifacts; empty == nil || len(empty) != 0 {
		t.Fatalf("anchor without artifacts rendered %#v, want an empty list", empty)
	}
}
