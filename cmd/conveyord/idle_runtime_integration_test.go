package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/backend"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Idle-runtime checks (component-verification-strategy, Idle-runtime checks;
// feature-quiet-idle-runtime, Verification; req-quiet-idle-runtime AC-1.1,
// AC-1.2, AC-1.3). The real daemon starts through the child-process entry
// point of TestConveyordDurableStartupIntegration on a store that holds two
// workspaces, an approved task waiting on a repository whose workspace has
// webhook delivery, an open notify task stream, and a pending await_review,
// and the test measures 60 seconds of real time while nothing changes.

const (
	idleWindow         = 60 * time.Second
	idleWorkspace      = "idle"
	idleQuietWorkspace = "quiet"
	idleWebhookSecret  = "idle-webhook-secret"
	idleSession        = "idle-session"
	// idleSingleStoreBound is the most statements the SingleStore daemon
	// with peers may send in the window (req-quiet-idle-runtime AC-1.3).
	idleSingleStoreBound = 2
)

func TestConveyordIdleRuntimeIntegration(t *testing.T) {
	// The PostgreSQL daemon sends no transaction in 60 seconds
	// (AC-1.1, AC-1.2). CI runs it in the PostgreSQL integration tests'
	// daemon shard.
	t.Run("postgres", func(t *testing.T) {
		meter := newPostgresIdleMeter(t)
		phase := idlePhases(t)
		run := prepareIdleDaemon(t, "postgres", meter.daemonURL, meter.daemonURL, false)
		defer run.close(t)
		phase("prepare")
		meter.disableAutovacuum(t)
		meter.settle(t, run)
		phase("settle")
		report := meter.measure(t, run)
		phase("window")
		run.requirePending(t)
		if report != "" {
			t.Fatalf("the idle PostgreSQL daemon sent statements during %s:\n%s\ndaemon log:\n%s", idleWindow, report, run.log())
		}
	})
	// The SingleStore daemon with runtime.peers sends at most two statements
	// in 60 seconds (AC-1.3). CI runs it in the SingleStore integration
	// tests' commands shard.
	t.Run("singlestore-peers", func(t *testing.T) {
		direct := durableStartupDatabase(t, "singlestore")
		cfg, err := mysql.ParseDSN(direct)
		if err != nil {
			t.Fatal(err)
		}
		proxy := startStatementProxy(t, cfg.Addr)
		proxied := cfg.Clone()
		proxied.Addr = proxy.addr()
		proxied.TLSConfig = "false"
		phase := idlePhases(t)
		run := prepareIdleDaemon(t, "singlestore", proxied.FormatDSN(), direct, true)
		defer run.close(t)
		phase("prepare")
		settleProxy(t, proxy, run)
		phase("settle")
		start := time.Now()
		time.Sleep(idleWindow)
		window := proxy.since(start)
		phase("window")
		run.requirePending(t)
		if n := countStatements(window); n > idleSingleStoreBound {
			t.Fatalf("the idle SingleStore daemon with peers sent %d statements in %s, want at most %d:\n%s\ndaemon log:\n%s", n, idleWindow, idleSingleStoreBound, describeProxyCommands(start, window), run.log())
		}
		t.Logf("statements in the window: %d\n%s", countStatements(window), describeProxyCommands(start, window))
	})
}

// idlePhases logs the elapsed time of each phase, so a slow shard shows
// where its time went.
func idlePhases(t *testing.T) func(string) {
	last := time.Now()
	return func(name string) {
		t.Logf("phase %s: %s", name, time.Since(last).Round(10*time.Millisecond))
		last = time.Now()
	}
}

// idleRun is the measured daemon with its open stream and pending
// await_review.
type idleRun struct {
	daemon       *durableDaemon
	cancel       context.CancelFunc
	streamClosed chan error
	awaited      chan string
}

func (r *idleRun) log() string {
	b, _ := os.ReadFile(r.daemon.logPath)
	return string(b)
}

// requirePending fails when the stream ended or await_review returned.
func (r *idleRun) requirePending(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.streamClosed:
		t.Fatalf("the notify stream ended: %v\ndaemon log:\n%s", err, r.log())
	case body := <-r.awaited:
		t.Fatalf("await_review returned: %s\ndaemon log:\n%s", body, r.log())
	default:
	}
}

func (r *idleRun) close(t *testing.T) {
	t.Helper()
	r.cancel()
	started := time.Now()
	r.daemon.stop(t)
	t.Logf("phase stop: %s", time.Since(started).Round(10*time.Millisecond))
}

// prepareIdleDaemon runs the two starts described in
// component-verification-strategy, Idle-runtime checks: the first migrates,
// bootstraps idle, creates quiet, connects idle's GitHub App and webhook
// secret, and accepts one signed ping; the store is then seeded and the
// second start serves the measured daemon. daemonURL is the URL the daemon
// uses; seedURL is the test's own.
func prepareIdleDaemon(t *testing.T, name, daemonURL, seedURL string, peers bool) *idleRun {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	requireStartupWrite(t, cfgPath, []byte(idleDaemonConfig(name, peers)))
	phase := idlePhases(t)
	first := startDurableDaemon(t, daemonURL, cfgPath, 5*time.Minute)
	phase("first start")
	if code, body := first.post(t, "/v1/workspaces", durableStartupToken, `{"id":"`+idleQuietWorkspace+`","name":"Quiet","document":{"repos":[]}}`); code != http.StatusCreated {
		t.Fatalf("create workspace status=%d body=%s", code, body)
	}
	st, err := backend.Open(store.WithActor(t.Context(), store.SystemActor()), config.Database{Backend: name, URL: seedURL})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{8}, 32))
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), idleWorkspace)
	connectIdleGitHubApp(t, ctx, st)
	deliverIdlePing(t, first)
	phase("webhook setup")
	first.stop(t)
	phase("first stop")

	owner, err := st.VerifyPersonalAccessToken(ctx, durableStartupToken)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := st.GitHubWebhookStatus(ctx, idleWorkspace); err != nil || status.LastDeliveryAt == nil {
		t.Fatalf("webhook status=%+v err=%v, want an accepted delivery", status, err)
	}
	seedIdleTask(t, ctx, st, core.TaskApproved, true)
	running := seedIdleTask(t, ctx, st, core.TaskRunning, false)
	order := seedSubmittedImplementOrder(t, ctx, st, running, owner.ID)
	st.Close()

	phase("seed")
	daemon := startDurableDaemon(t, daemonURL, cfgPath, 5*time.Minute)
	phase("second start")
	streamCtx, cancel := context.WithCancel(context.Background())
	run := &idleRun{daemon: daemon, cancel: cancel, streamClosed: make(chan error, 1), awaited: make(chan string, 1)}
	openIdleStream(t, streamCtx, run, running)
	go awaitIdleReview(streamCtx, run, order)
	return run
}

func idleDaemonConfig(name string, peers bool) string {
	runtime := ""
	if peers {
		runtime = "runtime: {peers: true}\n"
	}
	return fmt.Sprintf(`workspace: %s
database: {backend: %s}
%srouting:
  stages:
    triage: {model: fixture, timeout: 20m, execution: in_process}
    spec: {model: fixture, timeout: 30m, execution: mcp}
    implement: {model: fixture, timeout: 4h, execution: mcp}
    review: {model: fixture, timeout: 1h, execution: mcp}
repos:
  - {name: repo, url: https://github.com/conveyor-idle/repo, github: conveyor-idle/repo, base: main}
`, idleWorkspace, name, runtime)
}

// connectIdleGitHubApp stores a GitHub App without an installation and a
// webhook secret for idle. Without an installation every GitHub read fails
// before any network call.
func connectIdleGitHubApp(t *testing.T, ctx context.Context, st store.Backend) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	app := core.WorkspaceGitHubAppCredential{PrivateKey: privateKey}
	app.WorkspaceID, app.AppID, app.AppSlug, app.ClientID = idleWorkspace, 1, "conveyor-idle", "Iv1.idle"
	if _, err = st.StoreWorkspaceGitHubApp(ctx, idleWorkspace, app); err != nil {
		t.Fatal(err)
	}
	if _, err = st.SetWorkspaceGitHubWebhookSecret(ctx, idleWorkspace, idleWebhookSecret); err != nil {
		t.Fatal(err)
	}
}

// deliverIdlePing posts one signed ping through the real receiver, so idle
// has webhook delivery from an accepted delivery.
func deliverIdlePing(t *testing.T, d *durableDaemon) {
	t.Helper()
	body := []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)
	mac := hmac.New(sha256.New, []byte(idleWebhookSecret))
	mac.Write(body)
	request, err := http.NewRequestWithContext(d.ctx, http.MethodPost, "http://"+d.addr+"/v1/github/webhook/"+idleWorkspace, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "ping")
	request.Header.Set("X-GitHub-Delivery", "idle-ping-1")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := d.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(response.Body)
		t.Fatalf("ping status=%d body=%s", response.StatusCode, b)
	}
}

func seedIdleTask(t *testing.T, ctx context.Context, st store.Backend, state core.TaskState, mergeApproval bool) string {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{
		ID: id, Workspace: idleWorkspace, Title: "idle " + id, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id,
		Mode: core.TaskModeAuto, PolicyVersion: 1, Level: core.L2, State: state, NextStage: core.StageImplement,
		MergeApproval: mergeApproval, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetTask(ctx, id); err != nil || got.State != state {
		t.Fatalf("seeded task state=%s err=%v, want %s", got.State, err, state)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: id, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 1, "url": "https://github.com/conveyor-idle/repo/pull/1"})}); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedSubmittedImplementOrder records the task's implement order as claimed
// by the owner's session and submitted, so await_review admits that session.
func seedSubmittedImplementOrder(t *testing.T, ctx context.Context, st store.Backend, taskID, ownerID string) string {
	t.Helper()
	now := time.Now().UTC()
	job := core.Job{ID: taskID + "-implement", TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(24 * time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	order, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: idleSession, ClientToken: "idle-client-token", ClaimantID: core.TaskRunClaimantID(ownerID), OwnerUserID: ownerID,
		Agent: "codex", Model: "gpt", Lease: time.Hour, ExecutionTimeout: 4 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderSubmitted
	if err = storetest.For(st).UpdateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	return order.ID
}

// openIdleStream opens the notify task stream and waits for its ready frame.
func openIdleStream(t *testing.T, ctx context.Context, run *idleRun, taskID string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+run.daemon.addr+"/v1/tasks/"+taskID+"/events/stream?mode=notify&workspace_id="+idleWorkspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+durableStartupToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("notify stream status=%d body=%s", response.StatusCode, b)
	}
	reader := bufio.NewReader(response.Body)
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			response.Body.Close()
			t.Fatalf("notify stream before ready: %v", err)
		}
		if line == "\n" {
			break
		}
		frame.WriteString(line)
	}
	if frame.String() != "event: ready\ndata: {}\n" {
		response.Body.Close()
		t.Fatalf("notify first frame=%q", frame.String())
	}
	// Keep reading: keep-alive comments arrive; any other frame is a
	// change the idle store never made.
	go func() {
		defer response.Body.Close()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if ctx.Err() == nil {
					run.streamClosed <- err
				}
				return
			}
			if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") {
				run.streamClosed <- fmt.Errorf("unexpected frame line %q", line)
				return
			}
		}
	}()
}

// awaitIdleReview calls MCP await_review with a 600-second wait; it stays
// pending while nothing about the task changes.
func awaitIdleReview(ctx context.Context, run *idleRun, orderID string) {
	body := core.JSONPayload(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "await_review", "arguments": map[string]any{
		"workspace_id": idleWorkspace, "work_order_id": orderID, "session_id": idleSession, "timeout_seconds": 600,
	}}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+run.daemon.addr+"/mcp", bytes.NewReader(body))
	if err != nil {
		run.awaited <- err.Error()
		return
	}
	request.Header.Set("Authorization", "Bearer "+durableStartupToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			run.awaited <- err.Error()
		}
		return
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	if ctx.Err() == nil {
		run.awaited <- fmt.Sprintf("status=%d body=%s", response.StatusCode, b)
	}
}

// settleProxy waits until the proxy records no statement for five seconds,
// so the window starts after start-up and the first stream and await_review
// reads.
func settleProxy(t *testing.T, proxy *statementProxy, run *idleRun) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		quietSince := time.Now().Add(-5 * time.Second)
		recent := proxy.since(quietSince)
		if countStatements(recent) == 0 && time.Since(proxy.started) > 5*time.Second {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the SingleStore daemon never went quiet for five seconds:\n%s\ndaemon log:\n%s", describeProxyCommands(proxy.started, proxy.since(time.Now().Add(-30*time.Second))), run.log())
		}
		run.requirePending(t)
		time.Sleep(250 * time.Millisecond)
	}
}

// postgresIdleMeter owns the dedicated role and database the daemon uses
// and reads their statistics through the test's own fixture connection.
type postgresIdleMeter struct {
	admin     *pgxpool.Pool
	role      string
	database  string
	daemonURL string
}

func newPostgresIdleMeter(t *testing.T) *postgresIdleMeter {
	t.Helper()
	raw := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is unset")
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("the idle-runtime check requires a PostgreSQL _test database")
	}
	admin, err := pgxpool.New(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	secret := make([]byte, 16)
	if _, err = rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(secret)
	m := &postgresIdleMeter{admin: admin, role: "conveyor_idle_" + suffix, database: "conveyor_idle_" + suffix + "_test"}
	role, database := pgx.Identifier{m.role}.Sanitize(), pgx.Identifier{m.database}.Sanitize()
	if _, err = admin.Exec(t.Context(), "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role); err != nil {
			t.Error(err)
		}
	})
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+database+" OWNER "+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	daemon := *u
	daemon.User = url.UserPassword(m.role, password)
	daemon.Path = "/" + m.database
	m.daemonURL = daemon.String()
	return m
}

// disableAutovacuum turns autovacuum off for the server until cleanup:
// autovacuum workers run transactions in every database about once a
// minute, which pg_stat_database would count.
func (m *postgresIdleMeter) disableAutovacuum(t *testing.T) {
	t.Helper()
	var superuser bool
	if err := m.admin.QueryRow(t.Context(), "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&superuser); err != nil || !superuser {
		t.Fatalf("the idle-runtime check needs a superuser fixture connection to turn autovacuum off (superuser=%t err=%v)", superuser, err)
	}
	for _, statement := range []string{"ALTER SYSTEM SET autovacuum = off", "SELECT pg_reload_conf()"} {
		if _, err := m.admin.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []string{"ALTER SYSTEM RESET autovacuum", "SELECT pg_reload_conf()"} {
			if _, err := m.admin.Exec(context.Background(), statement); err != nil {
				t.Error(err)
			}
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		var setting string
		if err := m.admin.QueryRow(t.Context(), "SHOW autovacuum").Scan(&setting); err != nil {
			t.Fatal(err)
		}
		if setting == "off" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("autovacuum=%s after reload", setting)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// transactions is xact_commit + xact_rollback of the daemon's database.
func (m *postgresIdleMeter) transactions(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := m.admin.QueryRow(t.Context(), "SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = $1", m.database).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// settle waits until the counter is unchanged for 12 seconds, longer than
// PostgreSQL's 10-second flush of an idle backend's counters.
func (m *postgresIdleMeter) settle(t *testing.T, run *idleRun) {
	t.Helper()
	const quiet = 12 * time.Second
	deadline := time.Now().Add(90 * time.Second)
	last, since := m.transactions(t), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("the PostgreSQL daemon's transaction count never held for %s:\n%s\ndaemon log:\n%s", quiet, m.activity(t), run.log())
		}
		run.requirePending(t)
		time.Sleep(500 * time.Millisecond)
		if n := m.transactions(t); n != last {
			last, since = n, time.Now()
		}
	}
}

// measure runs the 60-second window: it samples pg_stat_activity for the
// role every 500 milliseconds and compares the transaction count at both
// ends. It returns a non-empty report of every statement it saw.
func (m *postgresIdleMeter) measure(t *testing.T, run *idleRun) string {
	t.Helper()
	var serverStart time.Time
	if err := m.admin.QueryRow(t.Context(), "SELECT clock_timestamp()").Scan(&serverStart); err != nil {
		t.Fatal(err)
	}
	before := m.transactions(t)
	seen := map[string]bool{}
	var statements []string
	end := time.Now().Add(idleWindow)
	for time.Now().Before(end) {
		rows, err := m.admin.Query(t.Context(), `SELECT pid, backend_start, query_start, coalesce(state, ''), coalesce(query, '')
			FROM pg_stat_activity WHERE usename = $1 AND (backend_start >= $2 OR query_start >= $2)`, m.role, serverStart)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var pid int
			var backendStart time.Time
			var queryStart *time.Time
			var state, query string
			if err = rows.Scan(&pid, &backendStart, &queryStart, &state, &query); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			at := backendStart
			kind := "connection"
			if queryStart != nil && !queryStart.Before(serverStart) {
				at, kind = *queryStart, "statement"
			}
			key := fmt.Sprintf("%d %s", pid, at.Format(time.RFC3339Nano))
			if !seen[key] {
				seen[key] = true
				statements = append(statements, fmt.Sprintf("  +%6.2fs pid=%d %s state=%s %s", at.Sub(serverStart).Seconds(), pid, kind, state, strings.Join(strings.Fields(query), " ")))
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	after := m.transactions(t)
	if after == before && len(statements) == 0 {
		t.Logf("pg_stat_database transactions for %s: %d before and after %s", m.database, before, idleWindow)
		return ""
	}
	return fmt.Sprintf("pg_stat_database transactions for %s: %d before, %d after (+%d)\nstatements sampled from pg_stat_activity:\n%s\npg_stat_activity of role %s:\n%s",
		m.database, before, after, after-before, strings.Join(statements, "\n"), m.role, m.activity(t))
}

// activity renders every pg_stat_activity row of the role.
func (m *postgresIdleMeter) activity(t *testing.T) string {
	t.Helper()
	rows, err := m.admin.Query(t.Context(), `SELECT pid, backend_start, coalesce(query_start::text, ''), coalesce(state, ''), coalesce(query, '')
		FROM pg_stat_activity WHERE usename = $1 ORDER BY pid`, m.role)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var pid int
		var backendStart time.Time
		var queryStart, state, query string
		if err = rows.Scan(&pid, &backendStart, &queryStart, &state, &query); err != nil {
			return err.Error()
		}
		fmt.Fprintf(&b, "  pid=%d backend_start=%s query_start=%s state=%s query=%s\n", pid, backendStart.Format(time.RFC3339Nano), queryStart, state, strings.Join(strings.Fields(query), " "))
	}
	if b.Len() == 0 {
		return "  (no backend)"
	}
	return b.String()
}
