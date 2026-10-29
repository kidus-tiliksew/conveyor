package postgres

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
)

const serverTimingToken = "server-timing-token"

var serverTimingValue = regexp.MustCompile(`^db;dur=(\d+)\.(\d{3}), db_queries;desc="(\d+)", db_acquire;dur=(\d+)\.(\d{3})$`)

type parsedServerTiming struct {
	db, acquireMicros int64
	queries           uint64
}

func parseServerTiming(t *testing.T, header http.Header) parsedServerTiming {
	t.Helper()
	values := header.Values("Server-Timing")
	if len(values) != 1 {
		t.Fatalf("Server-Timing values = %q", values)
	}
	match := serverTimingValue.FindStringSubmatch(values[0])
	if match == nil {
		t.Fatalf("Server-Timing = %q", values[0])
	}
	number := func(index int) int64 {
		n, err := strconv.ParseInt(match[index], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	queries, err := strconv.ParseUint(match[3], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return parsedServerTiming{db: number(1)*1000 + number(2), queries: queries, acquireMicros: number(4)*1000 + number(5)}
}

// serverTimingSchemaURL creates a fresh schema and returns the test database
// URL that selects it.
func serverTimingSchemaURL(t *testing.T) string {
	t.Helper()
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "server_timing_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// openServerTimingStore opens through the production open path, tracer
// included, and registers its close.
func openServerTimingStore(t *testing.T, databaseURL string, acquireStarted func(context.Context)) *Store {
	t.Helper()
	options, err := resolveOpenOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	options.acquireStarted = acquireStarted
	st, err := openWithOptions(store.WithActor(t.Context(), store.SystemActor()), databaseURL, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func serverTimingHandler(t *testing.T, st *Store, bootstrap bool) http.Handler {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "server-timing")
	if bootstrap {
		if _, err := st.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Timing", Email: "owner@example.test", DisplayName: "Owner"}, serverTimingToken); err != nil {
			t.Fatal(err)
		}
		if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "server-timing"}); err != nil {
			t.Fatal(err)
		}
	}
	server := httpapi.NewServer(st)
	server.Workspaces = st
	server.Workspace = "server-timing"
	return server.Handler()
}

func meRequest(ctx context.Context) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+serverTimingToken)
	return request
}

func TestServerTimingHTTPIntegration(t *testing.T) {
	st := openServerTimingStore(t, serverTimingSchemaURL(t), nil)
	if got := st.PoolStats().MaxConns; got != DefaultMaxConns {
		t.Fatalf("opened pool MaxConns = %d, want %d", got, DefaultMaxConns)
	}
	handler := serverTimingHandler(t, st, true)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, meRequest(t.Context()))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "owner@example.test") {
		t.Fatalf("GET /v1/me = %d %s", recorder.Code, recorder.Body)
	}
	timing := parseServerTiming(t, recorder.Result().Header)
	if timing.queries == 0 {
		t.Fatalf("store-backed request reported no queries: %+v", timing)
	}
	// An unauthenticated request keeps its ordinary refusal and still
	// reports the credential lookup.
	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("invalid token = %d", recorder.Code)
	}
	parseServerTiming(t, recorder.Result().Header)
	// The health route is outside /v1.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if values := recorder.Result().Header.Values("Server-Timing"); len(values) != 0 {
		t.Fatalf("/healthz Server-Timing = %q", values)
	}
}

func TestServerTimingAcquireWaitIntegration(t *testing.T) {
	schemaURL := serverTimingSchemaURL(t)
	serverTimingHandler(t, openServerTimingStore(t, schemaURL, nil), true)

	// A second store on the same schema has a single connection; the hook
	// reports each request acquisition that starts.
	started := make(chan *dbmetrics.Collector, 64)
	parsed, err := url.Parse(schemaURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("pool_max_conns", "1")
	parsed.RawQuery = query.Encode()
	single := openServerTimingStore(t, parsed.String(), func(ctx context.Context) {
		select {
		case started <- dbmetrics.FromContext(ctx):
		default:
		}
	})
	if got := single.PoolStats().MaxConns; got != 1 {
		t.Fatalf("pool_max_conns did not win: MaxConns = %d", got)
	}
	handler := serverTimingHandler(t, single, false)

	held, err := single.Pool().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, meRequest(context.Background()))
	}()
	// The request's first acquisition has started and must wait for the
	// held connection.
	collector := <-started
	held.Release()
	<-done
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/me = %d %s", recorder.Code, recorder.Body)
	}
	timing := parseServerTiming(t, recorder.Result().Header)
	if snapshot := collector.Snapshot(); snapshot.AcquireDuration <= 0 || timing.queries == 0 || snapshot.Queries < timing.queries {
		t.Fatalf("acquire wait not reported: header %+v snapshot %+v", timing, snapshot)
	}
	drain(started)

	// A request cancelled while it waits reports the failed acquisition
	// and gets the route's ordinary store-failure response.
	held, err = single.Pool().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	recorder = httptest.NewRecorder()
	done = make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(recorder, meRequest(ctx))
	}()
	collector = <-started
	cancel()
	<-done
	held.Release()
	if recorder.Code != http.StatusInternalServerError || strings.TrimSpace(recorder.Body.String()) != "credential verification failed" {
		t.Fatalf("cancelled request = %d %q", recorder.Code, recorder.Body)
	}
	parseServerTiming(t, recorder.Result().Header)
	if snapshot := collector.Snapshot(); snapshot.AcquireDuration <= 0 || snapshot.Queries != 0 {
		t.Fatalf("cancelled acquisition snapshot = %+v", snapshot)
	}
}

func drain(started chan *dbmetrics.Collector) {
	for {
		select {
		case <-started:
		default:
			return
		}
	}
}

func TestServerTimingIsolationIntegration(t *testing.T) {
	st := openServerTimingStore(t, serverTimingSchemaURL(t), nil)
	handler := serverTimingHandler(t, st, true)
	alone := func() uint64 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, meRequest(context.Background()))
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET /v1/me = %d %s", recorder.Code, recorder.Body)
		}
		return parseServerTiming(t, recorder.Result().Header).queries
	}
	baseline := alone()
	if again := alone(); again != baseline {
		t.Fatalf("the same request reported %d then %d queries", baseline, again)
	}

	// Two requests and background queries without a collector start
	// together behind a barrier.
	const background = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]uint64, 2)
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, meRequest(context.Background()))
			if recorder.Code != http.StatusOK {
				t.Errorf("parallel GET /v1/me = %d", recorder.Code)
				return
			}
			results[index] = parseServerTiming(t, recorder.Result().Header).queries
		}()
	}
	for range background {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := st.Pool().Exec(context.Background(), "SELECT 1"); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for index, got := range results {
		if got != baseline {
			t.Fatalf("parallel request %d reported %d queries, alone %d", index, got, baseline)
		}
	}

	// A query error through a collector context counts once.
	collector := dbmetrics.NewCollector()
	ctx := dbmetrics.WithCollector(context.Background(), collector)
	if _, err := st.Pool().Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("expected a division error")
	}
	var value int
	if err := st.Pool().QueryRow(ctx, "SELECT missing_column FROM tasks").Scan(&value); err == nil || errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected a query error, got %v", err)
	}
	if snapshot := collector.Snapshot(); snapshot.Queries != 2 || snapshot.QueryDuration <= 0 || snapshot.AcquireDuration <= 0 {
		t.Fatalf("error snapshot = %+v", snapshot)
	}
}
