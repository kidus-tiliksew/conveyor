package singlestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

// meteredConnector wraps the MySQL connector so query operations made with a
// request collector in their context are counted and timed. It forwards
// every optional database/sql driver interface the wrapped value implements;
// where one is absent each method answers what database/sql does for its
// absence. database/sql offers no request-attributable acquire hook, so
// acquisition latency is never reported (component-persistence, Request
// database metrics and pool sizing).
type meteredConnector struct {
	base driver.Connector
	now  func() time.Time
}

func newMeteredConnector(base driver.Connector, now func() time.Time) *meteredConnector {
	if now == nil {
		now = time.Now
	}
	return &meteredConnector{base: base, now: now}
}

var (
	_ driver.Connector = (*meteredConnector)(nil)
	_ io.Closer        = (*meteredConnector)(nil)

	_ driver.Conn               = (*meteredConn)(nil)
	_ driver.ConnBeginTx        = (*meteredConn)(nil)
	_ driver.ConnPrepareContext = (*meteredConn)(nil)
	_ driver.ExecerContext      = (*meteredConn)(nil)
	_ driver.QueryerContext     = (*meteredConn)(nil)
	_ driver.Pinger             = (*meteredConn)(nil)
	_ driver.SessionResetter    = (*meteredConn)(nil)
	_ driver.Validator          = (*meteredConn)(nil)
	_ driver.NamedValueChecker  = (*meteredConn)(nil)

	_ driver.Stmt              = (*meteredStmt)(nil)
	_ driver.StmtExecContext   = (*meteredStmt)(nil)
	_ driver.StmtQueryContext  = (*meteredStmt)(nil)
	_ driver.NamedValueChecker = (*meteredStmt)(nil)
	_ driver.ColumnConverter   = (*meteredStmt)(nil)

	_ driver.Rows                           = (*meteredRows)(nil)
	_ driver.RowsNextResultSet              = (*meteredRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*meteredRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*meteredRows)(nil)
	_ driver.RowsColumnTypeLength           = (*meteredRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*meteredRows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*meteredRows)(nil)
)

// Connect passes a slot through the connect context so the counting dial
// binds the connection's read counter to the returned meteredConn
// (component-persistence, Database read accounting).
func (c *meteredConnector) Connect(ctx context.Context) (driver.Conn, error) {
	slot := &readConnSlot{}
	conn, err := c.base.Connect(context.WithValue(ctx, readConnSlotKey{}, slot))
	if err != nil {
		return nil, err
	}
	return &meteredConn{base: conn, now: c.now, reads: slot.conn}, nil
}

type readConnSlotKey struct{}

// readConnSlot carries one connection's counting wrapper from the dial back
// to Connect.
type readConnSlot struct{ conn *dbmetrics.ReadConn }

// countingDialFunc dials as the MySQL driver does for a connection it dials
// itself, keep-alive included, and wraps the connection before TLS so the
// bytes it receives are counted into reads.
func countingDialFunc(reads *dbmetrics.ReadLedger) func(context.Context, string, string) (net.Conn, error) {
	var dialer net.Dialer
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetKeepAlive(true)
		}
		wrapped := reads.WrapConn(conn)
		if slot, ok := ctx.Value(readConnSlotKey{}).(*readConnSlot); ok {
			slot.conn = dbmetrics.ReadConnOf(wrapped)
		}
		return wrapped, nil
	}
}

func (c *meteredConnector) Driver() driver.Driver { return c.base.Driver() }

// Close forwards to a closable wrapped connector; database/sql calls it from
// DB.Close.
func (c *meteredConnector) Close() error {
	if closer, ok := c.base.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

type meteredConn struct {
	base driver.Conn
	now  func() time.Time
	// reads counts this connection's received bytes; nil when it was not
	// dialed through countingDialFunc.
	reads *dbmetrics.ReadConn
}

// Prepare and Begin are the mandatory legacy methods; database/sql uses the
// context variants below, so these carry no request context and record
// nothing.
func (c *meteredConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.base.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &meteredStmt{base: stmt, now: c.now, reads: c.reads}, nil
}

func (c *meteredConn) Close() error { return c.base.Close() }

//nolint:staticcheck // driver.Conn requires Begin.
func (c *meteredConn) Begin() (driver.Tx, error) { return c.base.Begin() }

func (c *meteredConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	collector := dbmetrics.FromContext(ctx)
	started := c.start(collector)
	c.reads.Begin(dbmetrics.ReadLabel(ctx))
	stmt, err := c.prepare(ctx, query)
	c.reads.End(0, 0)
	if collector != nil {
		// Preparation is database time but not a query: the execution
		// that follows counts the query once.
		collector.AddQueryDuration(c.now().Sub(started))
	}
	if err != nil {
		return nil, err
	}
	return &meteredStmt{base: stmt, now: c.now, reads: c.reads}, nil
}

// prepare mirrors database/sql when the connection lacks
// ConnPrepareContext: the legacy Prepare, closed again if the context ended.
func (c *meteredConn) prepare(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.base.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	stmt, err := c.base.Prepare(query)
	if err == nil {
		select {
		default:
		case <-ctx.Done():
			stmt.Close()
			return nil, ctx.Err()
		}
	}
	return stmt, err
}

func (c *meteredConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	collector := dbmetrics.FromContext(ctx)
	started := c.start(collector)
	label := dbmetrics.ReadLabel(ctx)
	c.reads.Begin(label)
	tx, err := c.begin(ctx, opts)
	c.reads.End(1, 0)
	if collector == nil && c.reads == nil {
		return tx, err
	}
	if collector != nil {
		collector.AddQuery(c.now().Sub(started))
	}
	if err != nil {
		return nil, err
	}
	return &meteredTx{base: tx, collector: collector, now: c.now, reads: c.reads, label: label}, nil
}

// begin mirrors database/sql when the connection lacks ConnBeginTx: only
// default options reach the legacy Begin, rolled back if the context ended.
func (c *meteredConn) begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.base.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) {
		return nil, errors.New("sql: driver does not support non-default isolation level")
	}
	if opts.ReadOnly {
		return nil, errors.New("sql: driver does not support read-only transactions")
	}
	//nolint:staticcheck // Absent ConnBeginTx means the legacy method.
	tx, err := c.base.Begin()
	if err == nil {
		select {
		default:
		case <-ctx.Done():
			tx.Rollback()
			return nil, ctx.Err()
		}
	}
	return tx, err
}

func (c *meteredConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	var execute func() (driver.Result, error)
	if execer, ok := c.base.(driver.ExecerContext); ok {
		execute = func() (driver.Result, error) { return execer.ExecContext(ctx, query, args) }
	} else if execer, ok := c.base.(driver.Execer); ok { //nolint:staticcheck // Legacy fallback as database/sql.
		execute = func() (driver.Result, error) {
			values, err := namedValues(args)
			if err != nil {
				return nil, err
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			return execer.Exec(query, values)
		}
	} else {
		return nil, driver.ErrSkip
	}
	collector := dbmetrics.FromContext(ctx)
	started := c.start(collector)
	c.reads.Begin(dbmetrics.ReadLabel(ctx))
	result, err := execute()
	if errors.Is(err, driver.ErrSkip) {
		c.reads.End(0, 0)
		return nil, err
	}
	c.reads.End(1, 0)
	if collector != nil {
		collector.AddQuery(c.now().Sub(started))
	}
	return result, err
}

func (c *meteredConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	var run func() (driver.Rows, error)
	if queryer, ok := c.base.(driver.QueryerContext); ok {
		run = func() (driver.Rows, error) { return queryer.QueryContext(ctx, query, args) }
	} else if queryer, ok := c.base.(driver.Queryer); ok { //nolint:staticcheck // Legacy fallback as database/sql.
		run = func() (driver.Rows, error) {
			values, err := namedValues(args)
			if err != nil {
				return nil, err
			}
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			return queryer.Query(query, values)
		}
	} else {
		return nil, driver.ErrSkip
	}
	collector := dbmetrics.FromContext(ctx)
	started := c.start(collector)
	c.reads.Begin(dbmetrics.ReadLabel(ctx))
	rows, err := run()
	if errors.Is(err, driver.ErrSkip) {
		c.reads.End(0, 0)
		return nil, err
	}
	return meterRows(collector, c.reads, rows, err, started, c.now)
}

func (c *meteredConn) Ping(ctx context.Context) error {
	if pinger, ok := c.base.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *meteredConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.base.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *meteredConn) IsValid() bool {
	if validator, ok := c.base.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *meteredConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.base.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

func (c *meteredConn) start(collector *dbmetrics.Collector) time.Time {
	if collector == nil {
		return time.Time{}
	}
	return c.now()
}

type meteredStmt struct {
	base  driver.Stmt
	now   func() time.Time
	reads *dbmetrics.ReadConn
}

// Close is not measured: MySQL sends COM_STMT_CLOSE without a reply.
func (s *meteredStmt) Close() error  { return s.base.Close() }
func (s *meteredStmt) NumInput() int { return s.base.NumInput() }

//nolint:staticcheck // driver.Stmt requires Exec.
func (s *meteredStmt) Exec(args []driver.Value) (driver.Result, error) { return s.base.Exec(args) }

//nolint:staticcheck // driver.Stmt requires Query.
func (s *meteredStmt) Query(args []driver.Value) (driver.Rows, error) { return s.base.Query(args) }

func (s *meteredStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	collector := dbmetrics.FromContext(ctx)
	var started time.Time
	if collector != nil {
		started = s.now()
	}
	var result driver.Result
	var err error
	s.reads.Begin(dbmetrics.ReadLabel(ctx))
	if execer, ok := s.base.(driver.StmtExecContext); ok {
		result, err = execer.ExecContext(ctx, args)
	} else {
		var values []driver.Value
		if values, err = namedValues(args); err == nil {
			if err = ctx.Err(); err == nil {
				//nolint:staticcheck // Absent StmtExecContext means the legacy method.
				result, err = s.base.Exec(values)
			}
		}
	}
	s.reads.End(1, 0)
	if collector != nil {
		collector.AddQuery(s.now().Sub(started))
	}
	return result, err
}

func (s *meteredStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	collector := dbmetrics.FromContext(ctx)
	var started time.Time
	if collector != nil {
		started = s.now()
	}
	var rows driver.Rows
	var err error
	s.reads.Begin(dbmetrics.ReadLabel(ctx))
	if queryer, ok := s.base.(driver.StmtQueryContext); ok {
		rows, err = queryer.QueryContext(ctx, args)
	} else {
		var values []driver.Value
		if values, err = namedValues(args); err == nil {
			if err = ctx.Err(); err == nil {
				//nolint:staticcheck // Absent StmtQueryContext means the legacy method.
				rows, err = s.base.Query(values)
			}
		}
	}
	return meterRows(collector, s.reads, rows, err, started, s.now)
}

func (s *meteredStmt) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := s.base.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

//nolint:staticcheck // Forwarded for drivers that still implement it.
func (s *meteredStmt) ColumnConverter(index int) driver.ValueConverter {
	if converter, ok := s.base.(driver.ColumnConverter); ok { //nolint:staticcheck
		return converter.ColumnConverter(index)
	}
	return driver.DefaultParameterConverter
}

// namedValues mirrors database/sql's conversion for a statement without the
// context interfaces: named arguments are not supported.
func namedValues(args []driver.NamedValue) ([]driver.Value, error) {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		if arg.Name != "" {
			return nil, errNamedArgumentsUnsupported
		}
		values[i] = arg.Value
	}
	return values, nil
}

type namedArgumentsError struct{}

func (namedArgumentsError) Error() string {
	return "sql: driver does not support the use of Named Parameters"
}

var errNamedArgumentsUnsupported error = namedArgumentsError{}

// meterRows counts a failed query at once and a successful one when its rows
// close, so its duration includes row consumption and the read counter
// receives the query's rows and bytes until close.
func meterRows(collector *dbmetrics.Collector, reads *dbmetrics.ReadConn, rows driver.Rows, err error, started time.Time, now func() time.Time) (driver.Rows, error) {
	if err != nil {
		reads.End(1, 0)
		if collector != nil {
			collector.AddQuery(now().Sub(started))
		}
		return nil, err
	}
	if collector == nil && reads == nil {
		return rows, nil
	}
	return &meteredRows{base: rows, collector: collector, reads: reads, started: started, now: now}, nil
}

type meteredTx struct {
	base      driver.Tx
	collector *dbmetrics.Collector
	now       func() time.Time
	reads     *dbmetrics.ReadConn
	label     string
}

// Commit and Rollback keep the collector and read label of the context the
// transaction began with, because driver.Tx carries no context.
func (t *meteredTx) Commit() error {
	started := t.now()
	t.reads.Begin(t.label)
	err := t.base.Commit()
	t.reads.End(1, 0)
	t.collector.AddQuery(t.now().Sub(started))
	return err
}

func (t *meteredTx) Rollback() error {
	started := t.now()
	t.reads.Begin(t.label)
	err := t.base.Rollback()
	t.reads.End(1, 0)
	t.collector.AddQuery(t.now().Sub(started))
	return err
}

type meteredRows struct {
	base      driver.Rows
	collector *dbmetrics.Collector
	reads     *dbmetrics.ReadConn
	rows      atomic.Uint64
	started   time.Time
	now       func() time.Time
	closed    atomic.Bool
}

func (r *meteredRows) Columns() []string { return r.base.Columns() }

// Next counts each row returned.
func (r *meteredRows) Next(dest []driver.Value) error {
	err := r.base.Next(dest)
	if err == nil {
		r.rows.Add(1)
	}
	return err
}

// Close records the query once, even when database/sql closes the rows from
// its cancellation goroutine. The driver discards unread rows inside Close,
// so their bytes stay with the query's label.
func (r *meteredRows) Close() error {
	err := r.base.Close()
	if r.closed.CompareAndSwap(false, true) {
		r.reads.End(1, r.rows.Load())
		if r.collector != nil {
			r.collector.AddQuery(r.now().Sub(r.started))
		}
	}
	return err
}

func (r *meteredRows) HasNextResultSet() bool {
	if next, ok := r.base.(driver.RowsNextResultSet); ok {
		return next.HasNextResultSet()
	}
	return false
}

func (r *meteredRows) NextResultSet() error {
	if next, ok := r.base.(driver.RowsNextResultSet); ok {
		return next.NextResultSet()
	}
	return io.EOF
}

func (r *meteredRows) ColumnTypeScanType(index int) reflect.Type {
	if typed, ok := r.base.(driver.RowsColumnTypeScanType); ok {
		return typed.ColumnTypeScanType(index)
	}
	return reflect.TypeFor[any]()
}

func (r *meteredRows) ColumnTypeDatabaseTypeName(index int) string {
	if typed, ok := r.base.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return typed.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *meteredRows) ColumnTypeLength(index int) (int64, bool) {
	if typed, ok := r.base.(driver.RowsColumnTypeLength); ok {
		return typed.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *meteredRows) ColumnTypeNullable(index int) (bool, bool) {
	if typed, ok := r.base.(driver.RowsColumnTypeNullable); ok {
		return typed.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *meteredRows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if typed, ok := r.base.(driver.RowsColumnTypePrecisionScale); ok {
		return typed.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

// ReadStats drains the store's read ledger for the daemon's read statistics
// line (component-persistence, Database read accounting).
func (s *Store) ReadStats(top int) dbmetrics.ReadInterval {
	return s.reads.Drain(top)
}

// RequestMetrics reports that SingleStore measures query count and duration
// but not acquisition latency.
func (s *Store) RequestMetrics() dbmetrics.Capability {
	return dbmetrics.QueryCount | dbmetrics.QueryDuration
}
