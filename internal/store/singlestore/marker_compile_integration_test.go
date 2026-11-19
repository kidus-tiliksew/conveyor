package singlestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// The marker fold compile guard measures how long SingleStore takes to
// compile each statement shape the activity-marker fold read can produce.
// A statement whose compile reaches max_compilation_time_s fails with error
// 2408 and is not cached, so every later read compiles it again and fails
// again; that is how the board reads answered 500 after v0.73.0. Each fixed
// shape must complete its cold first use within markerCompileCeiling. The
// v0.72.0 and pre-fix (95133fd2) statements are measured in the same run for
// the record only; they set no budget.
//
// Measurement: a dedicated connection runs with interpreter_mode=compile, so
// the first execution compiles synchronously instead of interpreting while a
// background compile runs. Before each trial the guard evicts every cached
// plan of the test's own private database on every node, aggregator and
// leaf, with DROP <plan_id> FROM PLANCACHE ON NODE <node_id>. It never runs
// DROP ALL FROM PLANCACHE, which also clears no leaf plan, and it touches no
// other database's plans. It then times the statement from send through
// result drain on an empty event table with the normal schema and the
// production binding path (client-side interpolation). The cold time is the
// compile proxy; a warm rerun right after a cold trial is the control. A
// discarded first round moves every leaf plan the statements share under
// the private database, so the measured rounds evict them too. Shapes are
// measured in rounds, so slow drift of the engine affects every shape alike.

// markerCompileCeiling bounds every cold trial of every fixed fold shape.
const markerCompileCeiling = 15 * time.Second

// markerCompileTrials is the number of cold trials per shape.
const markerCompileTrials = 3

// markerCompileFixture is one pinned statement with its bound arguments.
type markerCompileFixture struct {
	Query string `json:"query"`
	Args  []any  `json:"args"`
}

// loadMarkerCompileFixture reads a statement captured at an earlier revision
// (testdata/marker_fold_*.json): the revision's own markerFoldQuery output
// for markerCompileInput, with "no_live" and "live" shapes. JSON numbers
// are the clock arguments and bind as integers.
func loadMarkerCompileFixture(t testing.TB, name string) map[string]markerCompileFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]markerCompileFixture
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	for shape, statement := range fixture {
		for i, arg := range statement.Args {
			if number, ok := arg.(float64); ok {
				statement.Args[i] = int64(number)
			}
		}
		if got := strings.Count(statement.Query, "?"); got != len(statement.Args) {
			t.Fatalf("%s %s holds %d placeholders for %d arguments", name, shape, got, len(statement.Args))
		}
		fixture[shape] = statement
	}
	return fixture
}

// markerCompileInput is the page every revision's statement was captured
// for: 100 tasks, 10 of them review tasks, and for the live shapes 5 live
// review tasks with one open review order each.
func markerCompileInput(live, keepAll bool) store.MarkerFoldInput {
	page := make([]string, 100)
	for i := range page {
		page[i] = fmt.Sprintf("marker-compile-task-%03d", i)
	}
	in := store.MarkerFoldInput{Workspace: "marker-compile", Page: page, ReviewTasks: page[:10],
		Now: time.Date(2026, 11, 19, 12, 0, 0, 123456789, time.UTC), KeepAllClaims: keepAll}
	if live {
		in.LiveReviewTasks = page[:5]
		for _, taskID := range page[:5] {
			in.OpenReviewOrders = append(in.OpenReviewOrders, store.MarkerReviewOrder{TaskID: taskID, ID: taskID + "-review-1", JobID: taskID + "-review-1"})
		}
	}
	return in
}

type markerCompileShape struct {
	name    string
	query   string
	args    []any
	fixed   bool          // a statement the fold read produces; the ceiling applies
	first   time.Duration // round 0, the first use on the fresh database
	cold    []time.Duration
	timeout []string
	stopped bool // a recorded baseline timeout ended this shape's trials
	warm    time.Duration
}

func (shape *markerCompileShape) median() (time.Duration, bool) {
	if len(shape.cold) < markerCompileTrials {
		return 0, false
	}
	sorted := slices.Clone(shape.cold)
	slices.Sort(sorted)
	return sorted[len(sorted)/2], true
}

// markerCompileSession is the dedicated connection of the guard.
type markerCompileSession struct {
	t        *testing.T
	st       *Store
	conn     *sql.Conn
	database string
	// connectionID is the server's ID of conn.
	connectionID int64
}

func (s *markerCompileSession) open(ctx context.Context) {
	s.t.Helper()
	conn, err := s.st.db.Conn(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "SET SESSION interpreter_mode = compile"); err != nil {
		s.t.Fatal(err)
	}
	if err = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&s.database); err != nil || !strings.HasSuffix(s.database, "_test") {
		s.t.Fatalf("compile guard database %q: %v", s.database, err)
	}
	if err = conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&s.connectionID); err != nil {
		s.t.Fatal(err)
	}
	s.conn = conn
}

// stop ends the statement the guard's own connection may still run on the
// server after the client lost that connection, and waits until the server
// reports it gone. KILL QUERY and the PROCESSLIST poll run on a separate
// control connection, taken while the lost connection is still checked out
// of the pool, so the poll never runs on the connection it inspects.
func (s *markerCompileSession) stop(ctx context.Context) {
	s.t.Helper()
	defer s.conn.Close()
	control, err := s.st.db.Conn(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer control.Close()
	var controlID int64
	if err = control.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&controlID); err != nil {
		s.t.Fatal(err)
	}
	if controlID == s.connectionID {
		s.t.Fatalf("the control connection is the lost connection %d", controlID)
	}
	if _, err = control.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", s.connectionID)); err != nil {
		s.t.Logf("stop connection %d: %v", s.connectionID, err)
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		var running int
		if err = control.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID=? AND COMMAND='Query'", s.connectionID).Scan(&running); err != nil {
			s.t.Fatal(err)
		}
		if running == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.t.Fatalf("the timed-out statement on connection %d still runs", s.connectionID)
}

// markerPlanMissingError is the server's error number for DROP <plan_id>
// FROM PLANCACHE ON NODE <node_id> when that node holds no such plan.
const markerPlanMissingError uint16 = 1885

// markerCompilePlan is one cached plan of the test database on one node.
type markerCompilePlan struct{ id, node int64 }

// plans lists every cached plan of the test database and its partition
// databases on every node, aggregator and leaf (MV_PLANCACHE). The listing
// statement itself runs in the test database and caches its own plan there;
// it is the only plan left out, so every statement the guard measures is
// listed.
func (s *markerCompileSession) plans(ctx context.Context) []markerCompilePlan {
	s.t.Helper()
	rows, err := s.conn.QueryContext(ctx, "SELECT PLAN_ID,NODE_ID FROM information_schema.MV_PLANCACHE WHERE (DATABASE_NAME=? OR LEFT(DATABASE_NAME,?)=?) AND INSTR(QUERY_TEXT,'information_schema.MV_PLANCACHE')=0", s.database, len(s.database)+1, s.database+"_")
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var plans []markerCompilePlan
	for rows.Next() {
		var p markerCompilePlan
		if err = rows.Scan(&p.id, &p.node); err != nil {
			s.t.Fatal(err)
		}
		plans = append(plans, p)
	}
	if err = rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return plans
}

// evict drops every cached plan of the test database and its partition
// databases on every node, then proves none remains, so no cold trial can
// run on a cached plan. The database is private to this test binary. A drop
// may fail only because the plan left the cache between the read and the
// drop (error 1885), and only when a fresh read confirms that plan is gone
// from that node; every other drop error fails the guard.
func (s *markerCompileSession) evict(ctx context.Context) {
	s.t.Helper()
	for pass := 0; ; pass++ {
		plans := s.plans(ctx)
		if len(plans) == 0 {
			return
		}
		if pass == 3 {
			s.t.Fatalf("%d cached plans of %s remain after eviction: %v", len(plans), s.database, plans)
		}
		for _, p := range plans {
			_, err := s.conn.ExecContext(ctx, fmt.Sprintf("DROP %d FROM PLANCACHE ON NODE %d", p.id, p.node))
			if err == nil {
				continue
			}
			var driverErr *mysql.MySQLError
			if !errors.As(err, &driverErr) || driverErr.Number != markerPlanMissingError {
				s.t.Fatalf("drop plan %d on node %d: %v", p.id, p.node, err)
			}
			if slices.Contains(s.plans(ctx), p) {
				s.t.Fatalf("drop plan %d on node %d reported a missing plan, but the plan is still cached", p.id, p.node)
			}
		}
	}
}

// run times one execution from send through result drain.
func (s *markerCompileSession) run(ctx context.Context, query string, args []any) (time.Duration, error) {
	started := time.Now()
	rows, err := s.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return time.Since(started), err
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	return time.Since(started), err
}

// warm reruns a shape right after its cold trial, without eviction.
func (s *markerCompileSession) warm(ctx context.Context, shape *markerCompileShape) time.Duration {
	s.t.Helper()
	elapsed, err := s.run(ctx, shape.query, shape.args)
	if err != nil {
		s.t.Fatalf("%s warm control failed: %v", shape.name, err)
	}
	return elapsed
}

// markerTrialOutcome classifies one execution of a measured statement.
type markerTrialOutcome int

const (
	markerTrialOK markerTrialOutcome = iota
	// markerTrialServerTimeout is error 2408: compiling reached
	// max_compilation_time_s and the server ended the statement, so the
	// connection stays usable.
	markerTrialServerTimeout
	// markerTrialLostConnection is a read timeout or a broken connection: the
	// server may still compile the statement.
	markerTrialLostConnection
	// markerTrialFailed is any other execution error.
	markerTrialFailed
)

func markerTrialClassify(err error) markerTrialOutcome {
	var driverErr *mysql.MySQLError
	switch {
	case err == nil:
		return markerTrialOK
	case errors.As(err, &driverErr) && driverErr.Number == compilationTimeoutError:
		return markerTrialServerTimeout
	case errors.Is(err, mysql.ErrInvalidConn), errors.Is(err, driver.ErrBadConn), errors.Is(err, sql.ErrConnDone), errors.Is(err, context.DeadlineExceeded):
		return markerTrialLostConnection
	}
	return markerTrialFailed
}

// markerTrialAction is what the guard does with one trial.
type markerTrialAction struct {
	// record is a baseline timeout kept as evidence; it is never a
	// measurement.
	record string
	// stopShape ends the shape's trials: a plan whose compile does not
	// finish is not cached, so every later trial would compile it again.
	stopShape bool
	// reconnect stops the server statement and opens a fresh connection.
	reconnect bool
	// fatal fails the guard.
	fatal string
}

// markerTrialDecide applies the guard's rule to one trial. A fixed shape
// fails the guard on any timeout or execution error. A historical baseline
// (v0.72.0 or pre-fix main) records a timeout as evidence and never sets the
// guard result; any other baseline execution error still fails, because the
// measurement itself is broken. Only a lost connection needs the server
// statement stopped; on error 2408 the server has already ended it.
func markerTrialDecide(name string, fixed bool, elapsed time.Duration, err error) markerTrialAction {
	outcome := markerTrialClassify(err)
	took := elapsed.Round(time.Millisecond)
	switch {
	case outcome == markerTrialOK:
		return markerTrialAction{}
	case outcome == markerTrialFailed:
		return markerTrialAction{fatal: fmt.Sprintf("%s failed after %s: %v", name, took, err)}
	case fixed:
		return markerTrialAction{fatal: fmt.Sprintf("%s timed out after %s: %v", name, took, err)}
	case outcome == markerTrialServerTimeout:
		return markerTrialAction{record: fmt.Sprintf("error 2408 after %s", took), stopShape: true}
	}
	return markerTrialAction{record: fmt.Sprintf("read timeout after %s", took), stopShape: true, reconnect: true}
}

// TestMarkerFoldCompileIntegration is the compile-time guard. Every fold
// statement text the read can produce must complete each cold first use,
// round 0 included, within markerCompileCeiling. A fixed shape that fails,
// times out, exceeds the ceiling, or has fewer than markerCompileTrials
// measurements fails the guard; a timeout never counts as a measurement.
// The v0.72.0 and 95133fd2 statements are measured for the record only:
// their timeouts are recorded and never set the guard result
// (markerTrialDecide).
func TestMarkerFoldCompileIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	session := &markerCompileSession{t: t, st: st}
	session.open(ctx)
	defer func() { session.conn.Close() }()

	tag := loadMarkerCompileFixture(t, "marker_fold_v0.72.0.json")
	prefix := loadMarkerCompileFixture(t, "marker_fold_95133fd2.json")
	baselines := []*markerCompileShape{
		{name: "v0.72.0 no open review", query: tag["no_live"].Query, args: tag["no_live"].Args},
		{name: "v0.72.0 open review", query: tag["live"].Query, args: tag["live"].Args},
	}
	prefixShapes := []*markerCompileShape{
		{name: "95133fd2 no open review", query: prefix["no_live"].Query, args: prefix["no_live"].Args},
		{name: "95133fd2 open review", query: prefix["live"].Query, args: prefix["live"].Args},
	}
	var fixed []*markerCompileShape
	for _, variant := range []struct {
		name          string
		live, keepAll bool
	}{
		{"fix no open review, reduced claims", false, false},
		{"fix no open review, every claim", false, true},
		{"fix open review, reduced claims", true, false},
		{"fix open review, every claim", true, true},
	} {
		query, args := markerFoldQuery(markerCompileInput(variant.live, variant.keepAll))
		fixed = append(fixed, &markerCompileShape{name: variant.name, query: query, args: args, fixed: true})
	}
	rounds := append(slices.Clone(baselines), fixed...)

	// The first statement on a fresh database pays for loading its tables;
	// an unmeasured read takes that cost out of every trial.
	if _, err := session.run(ctx, "SELECT COUNT(*) FROM events_columnstore", nil); err != nil {
		t.Fatal(err)
	}
	// trial runs one cold trial of a shape and applies markerTrialDecide.
	trial := func(shape *markerCompileShape) (time.Duration, bool) {
		session.evict(ctx)
		elapsed, err := session.run(ctx, shape.query, shape.args)
		action := markerTrialDecide(shape.name, shape.fixed, elapsed, err)
		if action.fatal != "" {
			t.Fatal(action.fatal)
		}
		if action.record != "" {
			shape.timeout = append(shape.timeout, action.record)
			shape.stopped = action.stopShape
			if action.reconnect {
				session.stop(ctx)
				session.open(ctx)
			}
			return 0, false
		}
		return elapsed, true
	}
	// The v0.72.0 and fixed shapes run in rounds. Round 0 is each shape's
	// first use on the fresh database: it is logged and, for a fixed shape,
	// held to the ceiling, and it moves every leaf plan the statements share
	// under the private database so the measured rounds evict them too. The
	// pre-fix shapes run after them, so a compile that times out and keeps
	// the engine busy cannot slow another measurement. A baseline stops at
	// its first recorded timeout.
	for round := range markerCompileTrials + 1 {
		for _, shape := range rounds {
			if shape.stopped {
				continue
			}
			elapsed, ok := trial(shape)
			if !ok {
				continue
			}
			if round == 0 {
				shape.first = elapsed
				continue
			}
			shape.cold = append(shape.cold, elapsed)
			if round == markerCompileTrials {
				shape.warm = session.warm(ctx, shape)
			}
		}
	}
	for _, shape := range prefixShapes {
		for range markerCompileTrials {
			if shape.stopped {
				break
			}
			if elapsed, ok := trial(shape); ok {
				shape.cold = append(shape.cold, elapsed)
			}
		}
		if !shape.stopped {
			shape.warm = session.warm(ctx, shape)
		}
	}

	var report strings.Builder
	fmt.Fprintf(&report, "marker fold cold first use (ceiling %s per fixed-shape trial, round 0 included; median of %d cold trials; warm control):\n", markerCompileCeiling, markerCompileTrials)
	for _, shape := range append(append(slices.Clone(baselines), prefixShapes...), fixed...) {
		median, measured := shape.median()
		line := fmt.Sprintf("  %-36s", shape.name)
		if measured {
			line += fmt.Sprintf(" median %8s", median.Round(time.Millisecond))
		} else {
			line += "  median        -"
		}
		cold := make([]string, len(shape.cold))
		for i, d := range shape.cold {
			cold[i] = d.Round(time.Millisecond).String()
		}
		if shape.first > 0 {
			line += " round0 " + shape.first.Round(time.Millisecond).String()
		}
		line += fmt.Sprintf(" cold [%s]", strings.Join(cold, " "))
		if shape.warm > 0 {
			line += " warm " + shape.warm.Round(time.Millisecond).String()
		}
		if len(shape.timeout) > 0 {
			line += " timeouts [" + strings.Join(shape.timeout, "; ") + "]"
		}
		report.WriteString(line + "\n")
	}
	t.Log(report.String())

	for _, shape := range fixed {
		if len(shape.cold) < markerCompileTrials {
			t.Fatalf("%s has %d of %d measurements", shape.name, len(shape.cold), markerCompileTrials)
		}
		if shape.first > markerCompileCeiling {
			t.Errorf("%s round 0 took %s, over the %s ceiling", shape.name, shape.first.Round(time.Millisecond), markerCompileCeiling)
		}
		for i, cold := range shape.cold {
			if cold > markerCompileCeiling {
				t.Errorf("%s cold trial %d took %s, over the %s ceiling", shape.name, i+1, cold.Round(time.Millisecond), markerCompileCeiling)
			}
		}
	}
}

// TestMarkerFoldPlanReuseIntegration proves the four statement texts reuse
// one engine plan each when the bound values change: page sizes 1, 41, and
// 100, and one, five, and 250 open review orders. The values travel
// interpolated, as in production, so the engine's own parameterization must
// fold the padded lists, the empty-list flag, and the order array into one
// plan (component-web-task-surfaces invariant 19).
func TestMarkerFoldPlanReuseIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := t.Context()
	session := &markerCompileSession{t: t, st: st}
	session.open(ctx)
	defer func() { session.conn.Close() }()
	for _, variant := range []struct {
		name          string
		live, keepAll bool
	}{
		{"no open review, reduced claims", false, false},
		{"no open review, every claim", false, true},
		{"open review, reduced claims", true, false},
		{"open review, every claim", true, true},
	} {
		session.evict(ctx)
		orderCounts := []int{0}
		if variant.live {
			orderCounts = []int{1, 5, 250}
		}
		runs := 0
		for _, pages := range []int{1, 41, 100} {
			for _, orders := range orderCounts {
				in := markerShapeInput(pages, orders, variant.keepAll)
				if pages == 1 {
					in.ReviewTasks = nil
				}
				query, args := markerFoldQuery(in)
				if _, err := session.run(ctx, query, args); err != nil {
					t.Fatalf("%s pages=%d orders=%d: %v", variant.name, pages, orders, err)
				}
				runs++
			}
		}
		var plans, executions int64
		if err := session.conn.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(COMMITS),0) FROM information_schema.PLANCACHE WHERE DATABASE_NAME=? AND QUERY_TEXT LIKE '%events_columnstore%'", session.database).Scan(&plans, &executions); err != nil {
			t.Fatal(err)
		}
		if plans != 1 || executions != int64(runs) {
			t.Fatalf("%s: %d runs left %d aggregator plans with %d executions, want one plan run %d times", variant.name, runs, plans, executions, runs)
		}
		t.Logf("%s: %d inputs, one plan", variant.name, runs)
	}
}
