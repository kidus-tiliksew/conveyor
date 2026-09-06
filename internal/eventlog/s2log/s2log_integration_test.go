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
