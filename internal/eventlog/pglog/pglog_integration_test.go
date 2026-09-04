package pglog

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/logtest"
)

// newIsolatedPool opens the integration database with search_path pinned to
// a fresh schema, the same isolation the store's migration tests use.
func newIsolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
	if !strings.Contains(databaseURL, "_test") {
		t.Fatalf("refusing integration database %q: name must contain _test", databaseURL)
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("pglog_%d", time.Now().UnixNano())
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.New(context.Background(), databaseURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := EnsureSchema(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPglogConformance(t *testing.T) {
	pool := newIsolatedPool(t)
	store := New(pool)
	logtest.Run(t, func(t *testing.T) eventlog.Store { return store })
}

func TestEnsureSchemaIsIdempotent(t *testing.T) {
	pool := newIsolatedPool(t)
	for i := 0; i < 2; i++ {
		if err := EnsureSchema(t.Context(), pool); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

// TestAppendInsideCallerTransaction is the shared-transaction contract: an append
// inside a transaction the caller rolls back leaves no trace, and one the
// caller commits is visible afterwards with the head advanced.
func TestAppendInsideCallerTransaction(t *testing.T) {
	pool := newIsolatedPool(t)
	store := New(pool)
	ctx := context.Background()
	workspace := "tx-" + fmt.Sprint(time.Now().UnixNano())
	stream := eventlog.TaskStream("t1")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(WithTx(ctx, tx), workspace, stream, eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "task.created"}}); err != nil {
		t.Fatal(err)
	}
	if head, _ := store.Head(WithTx(ctx, tx), workspace, stream); head != 1 {
		t.Fatalf("head inside tx=%d", head)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if head, _ := store.Head(ctx, workspace, stream); head != 0 {
		t.Fatalf("rolled-back append leaked: head=%d", head)
	}
	if events, _ := store.Tail(ctx, workspace, 0, 0); len(events) != 0 {
		t.Fatalf("rolled-back append leaked: %d events", len(events))
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendWith(ctx, tx, workspace, stream, eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "task.created"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := store.Read(ctx, workspace, stream, 0, 0)
	if err != nil || len(events) != 1 || events[0].Position != 1 {
		t.Fatalf("committed append: events=%+v err=%v", events, err)
	}
}

// TestPositionsCommitInOrder pins the tail guarantee: a transaction that
// appended first but commits second cannot leave a gap a tailer could skip
// over, because the workspace position row is held until commit.
func TestPositionsCommitInOrder(t *testing.T) {
	pool := newIsolatedPool(t)
	store := New(pool)
	ctx := context.Background()
	workspace := "order-" + fmt.Sprint(time.Now().UnixNano())

	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendWith(ctx, first, workspace, eventlog.TaskStream("a"), eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "a1"}}); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() {
		_, err := store.Append(ctx, workspace, eventlog.TaskStream("b"), eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "b1"}})
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("second append did not wait for the first transaction: err=%v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if events, _ := store.Tail(ctx, workspace, 0, 0); len(events) != 0 {
		t.Fatalf("uncommitted positions visible: %d", len(events))
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
	events, _ := store.Tail(ctx, workspace, 0, 0)
	if len(events) != 2 || events[0].Kind != "a1" || events[1].Kind != "b1" || events[0].Position != 1 || events[1].Position != 2 {
		t.Fatalf("tail=%+v", events)
	}
}
