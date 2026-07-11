package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/routing"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
)

func TestRiverNoCapacitySnoozesSingleJob(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	suffix := time.Now().UTC().Format("150405.000000000")
	cfg := &config.Config{
		Workspace: "river-capacity-" + suffix,
		CacheDir:  t.TempDir(),
		JobsDir:   t.TempDir(),
		Repos:     []config.Repo{{Name: "api", URL: "file:///unused", Base: "main"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := postgresstore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), &fakeRunner{}, cfg)
	dispatcher.Router = routing.New(noCapacityPool{}, config.Routing{OwnerID: "operator"})
	dispatcher.UseDurableQueue()
	client, err := NewRiverClient(st.Pool(), dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Stop(context.Background()) //nolint:errcheck

	task := core.Task{
		ID: "river-capacity-task-" + suffix, Workspace: cfg.Workspace, Source: "test", Title: "wait for capacity",
		Level: core.L2, Repo: "api", BaseBranch: "main", Branch: gitx.BranchName("river-capacity-task-" + suffix),
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for {
		var state string
		var count, attempt int
		err := st.Pool().QueryRow(ctx, `
SELECT state, count(*) OVER (), attempt
FROM river_job
WHERE kind = 'dispatch_task' AND args->>'task_id' = $1
ORDER BY id DESC LIMIT 1`, task.ID).Scan(&state, &count, &attempt)
		if err == nil && state == "scheduled" {
			if count != 1 || attempt != 0 {
				t.Fatalf("capacity wait jobs=%d attempt=%d, want one job with no consumed attempt", count, attempt)
			}
			persisted, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.State != core.TaskQueued {
				t.Fatalf("capacity wait task state = %s, want queued", persisted.State)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("River job was not snoozed: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func TestRiverDispatchesTransactionallyInsertedTask(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "init")

	suffix := time.Now().UTC().Format("150405.000000000")
	cfg := &config.Config{
		Workspace: "river-" + suffix,
		Image:     "conveyor:test",
		CacheDir:  filepath.Join(tmp, "cache"),
		JobsDir:   filepath.Join(tmp, "jobs"),
		Repos:     []config.Repo{{Name: "api", URL: "file://" + origin, Base: "main"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := postgresstore.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.BootstrapConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	dispatcher := New(st, gitx.NewManager(cfg.CacheDir, cfg.JobsDir), &fakeRunner{}, cfg)
	dispatcher.Router = routing.NewStatic(routing.Credential{
		ID: "test-codex", OwnerID: "operator", OwnerKind: "user",
		Kind: "personal_sub", Vendor: "openai", Harness: "codex",
	}, cfg.Routing)
	dispatcher.UseDurableQueue()
	client, err := NewRiverClient(st.Pool(), dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Stop(context.Background()) //nolint:errcheck

	task := core.Task{
		ID: "river-task-" + suffix, Workspace: cfg.Workspace, Source: "test", Title: "river dispatch",
		Level: core.L2, Repo: "api", BaseBranch: "main", Branch: gitx.BranchName("river-task-" + suffix),
		State: core.TaskQueued, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for {
		persisted, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.State == core.TaskParked {
			break // fake runner succeeds but creates no commits
		}
		select {
		case <-ctx.Done():
			t.Fatalf("River did not dispatch task; last state=%s", persisted.State)
		case <-time.After(50 * time.Millisecond):
		}
	}
	for {
		var state string
		if err := st.Pool().QueryRow(ctx,
			"SELECT state FROM river_job WHERE kind = 'dispatch_task' AND args->>'task_id' = $1 ORDER BY id DESC LIMIT 1",
			task.ID,
		).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "completed" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("River job did not finalize; last state=%s", state)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
