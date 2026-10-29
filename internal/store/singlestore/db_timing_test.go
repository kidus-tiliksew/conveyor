package singlestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// stepClock advances by one millisecond on every reading.
type stepClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(time.Millisecond)
	return c.current
}

// fixtureLog records the driver calls a test expects.
type fixtureLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *fixtureLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *fixtureLog) count(call string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c == call {
			n++
		}
	}
	return n
}

// fullDriver implements every optional interface the MySQL connection does.
// Like MySQL without parameter interpolation, a connection-level query or
// execution with arguments answers driver.ErrSkip.
type fullDriver struct {
	log *fixtureLog
	// queryEntered, when set, makes a query named "block" wait for its
	// context after signalling.
	queryEntered chan struct{}
}

type fullConnector struct{ driver *fullDriver }

func (c fullConnector) Connect(context.Context) (driver.Conn, error) {
	return &fullConn{driver: c.driver}, nil
}
func (c fullConnector) Driver() driver.Driver { return nil }
func (c fullConnector) Close() error          { c.driver.log.add("connector-close"); return nil }

type fullConn struct{ driver *fullDriver }

func (c *fullConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}
func (c *fullConn) Close() error { return nil }

//nolint:staticcheck // driver.Conn requires Begin.
func (c *fullConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fullConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.driver.log.add("begin")
	return fixtureTx{log: c.driver.log}, nil
}
func (c *fullConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	c.driver.log.add("prepare")
	if query == "prepare-fails" {
		return nil, errors.New("syntax error")
	}
	return &fullStmt{driver: c.driver, query: query}, nil
}
func (c *fullConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if len(args) != 0 {
		return nil, driver.ErrSkip
	}
	c.driver.log.add("exec")
	if query == "fails" {
		return nil, errors.New("duplicate key")
	}
	return driver.RowsAffected(1), nil
}
func (c *fullConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if len(args) != 0 {
		return nil, driver.ErrSkip
	}
	c.driver.log.add("query")
	switch query {
	case "fails":
		return nil, errors.New("table missing")
	case "block":
		close(c.driver.queryEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &fullRows{remaining: 3}, nil
}
func (c *fullConn) Ping(context.Context) error { c.driver.log.add("ping"); return nil }
func (c *fullConn) ResetSession(context.Context) error {
	c.driver.log.add("reset")
	return nil
}
func (c *fullConn) IsValid() bool { c.driver.log.add("valid"); return true }
func (c *fullConn) CheckNamedValue(value *driver.NamedValue) error {
	if custom, ok := value.Value.(customArg); ok {
		c.driver.log.add("check-named-value")
		value.Value = string(custom)
		return nil
	}
	return driver.ErrSkip
}

// customArg is accepted only through the connection's CheckNamedValue.
type customArg string

type fullStmt struct {
	driver *fullDriver
	query  string
}

func (s *fullStmt) Close() error  { s.driver.log.add("stmt-close"); return nil }
func (s *fullStmt) NumInput() int { return -1 }

//nolint:staticcheck // driver.Stmt requires Exec.
func (s *fullStmt) Exec([]driver.Value) (driver.Result, error) { return nil, errors.New("legacy exec") }

//nolint:staticcheck // driver.Stmt requires Query.
func (s *fullStmt) Query([]driver.Value) (driver.Rows, error) { return nil, errors.New("legacy query") }
func (s *fullStmt) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	s.driver.log.add("stmt-exec")
	return driver.RowsAffected(1), nil
}
func (s *fullStmt) QueryContext(context.Context, []driver.NamedValue) (driver.Rows, error) {
	s.driver.log.add("stmt-query")
	return &fullRows{remaining: 2}, nil
}

//nolint:staticcheck // Forwarding fixture.
func (s *fullStmt) ColumnConverter(int) driver.ValueConverter { return driver.Bool }

type fixtureTx struct{ log *fixtureLog }

func (t fixtureTx) Commit() error   { t.log.add("commit"); return nil }
func (t fixtureTx) Rollback() error { t.log.add("rollback"); return nil }

type fullRows struct{ remaining int }

func (r *fullRows) Columns() []string { return []string{"n"} }
func (r *fullRows) Close() error      { return nil }
func (r *fullRows) Next(dest []driver.Value) error {
	if r.remaining == 0 {
		return io.EOF
	}
	r.remaining--
	dest[0] = int64(r.remaining)
	return nil
}
func (r *fullRows) HasNextResultSet() bool                { return false }
func (r *fullRows) NextResultSet() error                  { return io.EOF }
func (r *fullRows) ColumnTypeScanType(int) reflect.Type   { return reflect.TypeFor[int64]() }
func (r *fullRows) ColumnTypeDatabaseTypeName(int) string { return "BIGINT" }
func (r *fullRows) ColumnTypeLength(int) (int64, bool)    { return 20, true }
func (r *fullRows) ColumnTypeNullable(int) (bool, bool)   { return true, true }
func (r *fullRows) ColumnTypePrecisionScale(int) (int64, int64, bool) {
	return 10, 2, true
}

func openFixture(t *testing.T, connector driver.Connector) (*sql.DB, *stepClock) {
	t.Helper()
	clock := &stepClock{current: time.Unix(0, 0)}
	db := sql.OpenDB(newMeteredConnector(connector, clock.now))
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, clock
}

func requestContext() (context.Context, *dbmetrics.Collector) {
	collector := dbmetrics.NewCollector()
	return dbmetrics.WithCollector(context.Background(), collector), collector
}

func TestMeteredConnectorCountsQueriesThroughRowsClose(t *testing.T) {
	log := &fixtureLog{}
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: log}})
	ctx, collector := requestContext()
	rows, err := db.QueryContext(ctx, "SELECT n")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for rows.Next() {
		read++
	}
	if err := rows.Err(); err != nil || read != 3 {
		t.Fatalf("read %d rows, err %v", read, err)
	}
	if got := collector.Snapshot(); got.Queries != 1 || got.QueryDuration != time.Millisecond {
		t.Fatalf("after EOF = %+v", got)
	}
	rows.Close()
	if got := collector.Snapshot(); got.Queries != 1 {
		t.Fatalf("a second close counted again: %+v", got)
	}

	// Closing early still counts once, with the duration through close.
	earlyCtx, early := requestContext()
	rows, err = db.QueryContext(earlyCtx, "SELECT n")
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	rows.Close()
	if got := early.Snapshot(); got.Queries != 1 || got.QueryDuration != time.Millisecond {
		t.Fatalf("early close = %+v", got)
	}
}

func TestMeteredConnectorFallbackCountsOnce(t *testing.T) {
	log := &fixtureLog{}
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: log}})
	ctx, collector := requestContext()
	// Arguments make the connection answer driver.ErrSkip, so database/sql
	// prepares and executes a statement.
	if _, err := db.ExecContext(ctx, "UPDATE t SET n=?", 1); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT n WHERE n=?", 1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if log.count("prepare") != 2 || log.count("stmt-exec") != 1 || log.count("stmt-query") != 1 || log.count("exec") != 0 || log.count("query") != 0 {
		t.Fatalf("driver calls = %v", log.calls)
	}
	// Two operations; each prepare (1 ms) and execution (1 ms) is database
	// time, but only the executions count.
	if got := collector.Snapshot(); got.Queries != 2 || got.QueryDuration != 4*time.Millisecond {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestMeteredConnectorPreparedStatementsCountEachExecution(t *testing.T) {
	log := &fixtureLog{}
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: log}})
	ctx, collector := requestContext()
	stmt, err := db.PrepareContext(ctx, "INSERT INTO t VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := stmt.ExecContext(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if got := collector.Snapshot(); got.Queries != 3 || got.QueryDuration != 4*time.Millisecond {
		t.Fatalf("snapshot = %+v", got)
	}
	failingCtx, failing := requestContext()
	if _, err := db.PrepareContext(failingCtx, "prepare-fails"); err == nil {
		t.Fatal("expected a prepare error")
	}
	if got := failing.Snapshot(); got.Queries != 0 || got.QueryDuration != time.Millisecond {
		t.Fatalf("failed prepare = %+v", got)
	}
}

func TestMeteredConnectorCountsErrors(t *testing.T) {
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: &fixtureLog{}}})
	ctx, collector := requestContext()
	if _, err := db.QueryContext(ctx, "fails"); err == nil {
		t.Fatal("expected a query error")
	}
	if _, err := db.ExecContext(ctx, "fails"); err == nil {
		t.Fatal("expected an exec error")
	}
	if got := collector.Snapshot(); got.Queries != 2 || got.QueryDuration != 2*time.Millisecond {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestMeteredConnectorCountsTransactions(t *testing.T) {
	log := &fixtureLog{}
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: log}})
	ctx, collector := requestContext()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE t SET n=1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A rollback after commit never reaches the driver.
	_ = tx.Rollback()
	if got := collector.Snapshot(); got.Queries != 3 {
		t.Fatalf("committed transaction = %+v", got)
	}
	rollbackCtx, rolledBack := requestContext()
	tx, err = db.BeginTx(rollbackCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := rolledBack.Snapshot(); got.Queries != 2 {
		t.Fatalf("rolled back transaction = %+v", got)
	}
	if log.count("commit") != 1 || log.count("rollback") != 1 {
		t.Fatalf("driver calls = %v", log.calls)
	}
}

func TestMeteredConnectorCountsCancelledQuery(t *testing.T) {
	entered := make(chan struct{})
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: &fixtureLog{}, queryEntered: entered}})
	ctx, collector := requestContext()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := db.QueryContext(ctx, "block")
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if got := collector.Snapshot(); got.Queries != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestMeteredConnectorWithoutCollectorRecordsNothing(t *testing.T) {
	log := &fixtureLog{}
	db, clock := openFixture(t, fullConnector{driver: &fullDriver{log: log}})
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SELECT n")
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if _, err := db.ExecContext(ctx, "UPDATE t SET n=?", 1); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if reading := clock.now(); reading != time.Unix(0, 0).Add(time.Millisecond) {
		t.Fatalf("the clock was read without a collector: %s", reading)
	}
}

func TestMeteredConnectorForwardsDriverCapabilities(t *testing.T) {
	log := &fixtureLog{}
	db, _ := openFixture(t, fullConnector{driver: &fullDriver{log: log}})
	ctx, _ := requestContext()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	// The custom type converts only through the connection's checker.
	if _, err := db.ExecContext(ctx, "UPDATE t SET s=?", customArg("x")); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT n")
	if err != nil {
		t.Fatal(err)
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	column := types[0]
	length, lengthOK := column.Length()
	nullable, nullableOK := column.Nullable()
	precision, scale, decimalOK := column.DecimalSize()
	if column.DatabaseTypeName() != "BIGINT" || column.ScanType() != reflect.TypeFor[int64]() || length != 20 || !lengthOK || !nullable || !nullableOK || precision != 10 || scale != 2 || !decimalOK {
		t.Fatalf("column type = %+v", column)
	}
	if rows.NextResultSet() {
		t.Fatal("unexpected next result set")
	}
	rows.Close()
	if log.count("ping") == 0 || log.count("check-named-value") != 1 {
		t.Fatalf("driver calls = %v", log.calls)
	}

	// database/sql itself reset and validated the pooled connection.
	if log.count("reset") == 0 || log.count("valid") == 0 {
		t.Fatalf("pool session calls = %v", log.calls)
	}
	direct := &fixtureLog{}
	conn := &meteredConn{base: &fullConn{driver: &fullDriver{log: direct}}, now: time.Now}
	if err := conn.ResetSession(ctx); err != nil || !conn.IsValid() || direct.count("reset") != 1 || direct.count("valid") != 1 {
		t.Fatalf("session calls = %v", direct.calls)
	}
	stmt := &meteredStmt{base: &fullStmt{driver: &fullDriver{log: log}}, now: time.Now}
	if converter := stmt.ColumnConverter(0); converter != driver.Bool {
		t.Fatalf("column converter = %v", converter)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if log.count("connector-close") != 1 {
		t.Fatalf("connector close calls = %v", log.calls)
	}
}

// minimalConn implements only the mandatory interfaces and the legacy
// Execer and Queryer.
type minimalConnector struct{ log *fixtureLog }

func (c minimalConnector) Connect(context.Context) (driver.Conn, error) {
	return minimalConn{log: c.log}, nil
}
func (c minimalConnector) Driver() driver.Driver { return nil }

type minimalConn struct{ log *fixtureLog }

func (c minimalConn) Prepare(string) (driver.Stmt, error) {
	c.log.add("prepare")
	return minimalStmt{log: c.log}, nil
}
func (c minimalConn) Close() error { return nil }

//nolint:staticcheck // Legacy fixture.
func (c minimalConn) Begin() (driver.Tx, error) {
	c.log.add("begin")
	return fixtureTx{log: c.log}, nil
}

//nolint:staticcheck // Legacy fixture.
func (c minimalConn) Exec(string, []driver.Value) (driver.Result, error) {
	c.log.add("exec")
	return driver.RowsAffected(1), nil
}

//nolint:staticcheck // Legacy fixture.
func (c minimalConn) Query(string, []driver.Value) (driver.Rows, error) {
	c.log.add("query")
	return &minimalRows{remaining: 1}, nil
}

type minimalStmt struct{ log *fixtureLog }

func (s minimalStmt) Close() error  { return nil }
func (s minimalStmt) NumInput() int { return -1 }

//nolint:staticcheck // Legacy fixture.
func (s minimalStmt) Exec([]driver.Value) (driver.Result, error) {
	s.log.add("stmt-exec")
	return driver.RowsAffected(1), nil
}

//nolint:staticcheck // Legacy fixture.
func (s minimalStmt) Query([]driver.Value) (driver.Rows, error) {
	s.log.add("stmt-query")
	return &minimalRows{remaining: 1}, nil
}

type minimalRows struct{ remaining int }

func (r *minimalRows) Columns() []string { return []string{"n"} }
func (r *minimalRows) Close() error      { return nil }
func (r *minimalRows) Next(dest []driver.Value) error {
	if r.remaining == 0 {
		return io.EOF
	}
	r.remaining--
	dest[0] = int64(1)
	return nil
}

func TestMeteredConnectorAbsentInterfaceFallbacks(t *testing.T) {
	log := &fixtureLog{}
	db, _ := openFixture(t, minimalConnector{log: log})
	ctx, collector := requestContext()
	if _, err := db.ExecContext(ctx, "UPDATE t SET n=?", 1); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT n WHERE n=?", 1)
	if err != nil {
		t.Fatal(err)
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		t.Fatal(err)
	}
	if types[0].DatabaseTypeName() != "" || types[0].ScanType() != reflect.TypeFor[any]() {
		t.Fatalf("default column type = %+v", types[0])
	}
	if _, ok := types[0].Length(); ok {
		t.Fatal("default length must be unknown")
	}
	for rows.Next() {
	}
	if rows.NextResultSet() || rows.Err() != nil {
		t.Fatalf("next result set err = %v", rows.Err())
	}
	rows.Close()
	if log.count("exec") != 1 || log.count("query") != 1 || log.count("prepare") != 0 {
		t.Fatalf("legacy calls = %v", log.calls)
	}
	if _, err := db.ExecContext(ctx, "UPDATE t SET n=:n", sql.Named("n", 1)); err == nil || !strings.Contains(err.Error(), "Named Parameters") {
		t.Fatalf("named argument error = %v", err)
	}
	if _, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable}); err == nil || !strings.Contains(err.Error(), "non-default isolation level") {
		t.Fatalf("isolation error = %v", err)
	}
	if _, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only error = %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx, "SELECT ?")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stmt.ExecContext(ctx, 1); err != nil {
		t.Fatal(err)
	}
	stmt.Close()
	// exec, query, a refused named exec, a refused isolation begin and a
	// refused read-only begin, begin, commit, and the prepared execution.
	if got := collector.Snapshot(); got.Queries != 8 {
		t.Fatalf("snapshot = %+v (calls %v)", got, log.calls)
	}
	conn := &meteredConn{base: minimalConn{log: log}, now: time.Now}
	if err := conn.Ping(ctx); err != nil || conn.ResetSession(ctx) != nil || !conn.IsValid() {
		t.Fatal("absent session interfaces must answer database/sql's defaults")
	}
	if err := conn.CheckNamedValue(&driver.NamedValue{Value: 1}); !errors.Is(err, driver.ErrSkip) {
		t.Fatalf("absent checker = %v", err)
	}
	if converter := (&meteredStmt{base: minimalStmt{log: log}}).ColumnConverter(0); converter != driver.DefaultParameterConverter {
		t.Fatalf("absent converter = %v", converter)
	}
}

func TestSingleStoreRequestMetricsOmitAcquire(t *testing.T) {
	if got := (&Store{}).RequestMetrics(); got != dbmetrics.QueryCount|dbmetrics.QueryDuration {
		t.Fatalf("capability = %b", got)
	}
}
