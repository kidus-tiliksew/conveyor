package s2log

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/logtest"
)

// Tests accept driver DSNs only, and never create, drop or clear a database.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")
	if raw == "" {
		t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is unset")
	}
	cfg, err := mysql.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	if !strings.HasSuffix(cfg.DBName, "_test") {
		t.Fatal("SingleStore integration database must end in _test")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	cfg.Params = map[string]string{"time_zone": "'+00:00'", "sql_mode": "'STRICT_ALL_TABLES'"}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(16)
	t.Cleanup(func() { db.Close() })
	if err = db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestSingleStoreLogConformance(t *testing.T) {
	logtest.Run(t, func(t *testing.T) eventlog.Store { return New(integrationDB(t)) })
}
func TestCallerTransactionIntegration(t *testing.T) {
	db := integrationDB(t)
	st := New(db)
	ws := t.Name() + time.Now().Format("150405.000000000")
	stream := eventlog.StreamID("task/atomic")
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx := WithTx(t.Context(), tx)
	if _, err = st.Append(ctx, ws, stream, eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "created"}}); err != nil {
		t.Fatal(err)
	}
	if head, err := st.Head(ctx, ws, stream); err != nil || head != 1 {
		t.Fatalf("inside tx: %d %v", head, err)
	}
	if head, err := st.Head(t.Context(), ws, stream); err != nil || head != 0 {
		t.Fatalf("before commit: %d %v", head, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if head, err := st.Head(context.Background(), ws, stream); err != nil || head != 0 {
		t.Fatalf("after rollback: %d %v", head, err)
	}
	tx, err = db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = st.Append(WithTx(t.Context(), tx), ws, stream, eventlog.ExpectNew, []eventlog.NewEvent{{Kind: "created"}}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if head, err := st.Head(t.Context(), ws, stream); err != nil || head != 1 {
		t.Fatalf("after commit: %d %v", head, err)
	}
}

func TestScopedReadsAndAppendRecencyIntegration(t *testing.T) {
	db := integrationDB(t)
	st := New(db)
	ctx := t.Context()
	ws := t.Name() + time.Now().Format("150405.000000000")
	other := ws + "-other"
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old.Add(24 * time.Hour)
	for _, item := range []struct {
		workspace string
		stream    eventlog.StreamID
		at        time.Time
	}{
		{ws, "task/a", newer}, {ws, "task/b", newer}, {other, "task/a", newer},
		{ws, "task/a", old}, {ws, "task/b", old}, {ws, "task/a", old},
	} {
		if _, err := st.Append(ctx, item.workspace, item.stream, eventlog.ExpectAny, []eventlog.NewEvent{{Kind: "created", At: item.at}}); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := st.Read(ctx, ws, "task/a", 1, 1)
	if err != nil || len(stream) != 1 || stream[0].Workspace != ws || stream[0].Stream != "task/a" || stream[0].Version != 2 || stream[0].Position != 3 {
		t.Fatalf("stream after version: %+v, %v", stream, err)
	}
	tail, err := st.Tail(ctx, ws, 2, 2)
	if err != nil || len(tail) != 2 || tail[0].Workspace != ws || tail[0].Stream != "task/a" || tail[0].Position != 3 || tail[1].Workspace != ws || tail[1].Stream != "task/b" || tail[1].Position != 4 {
		t.Fatalf("tail after position: %+v, %v", tail, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT position, stream_id, at FROM event_log WHERE workspace_id=? ORDER BY position DESC LIMIT 2`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var positions []int64
	for rows.Next() {
		var position int64
		var stream string
		var at time.Time
		if err := rows.Scan(&position, &stream, &at); err != nil {
			t.Fatal(err)
		}
		if (position == 5 && stream != "task/a") || (position == 4 && stream != "task/b") {
			t.Errorf("recent position %d has stream %q", position, stream)
		}
		if !at.Equal(old) {
			t.Errorf("recent append timestamp = %v, want backdated %v", at, old)
		}
		positions = append(positions, position)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(positions) != 2 || positions[0] != 5 || positions[1] != 4 {
		t.Fatalf("recent positions = %v", positions)
	}
}
