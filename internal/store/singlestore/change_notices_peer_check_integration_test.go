package singlestore

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// The peer check on SingleStore (component-persistence, Change notices;
// feature-quiet-idle-runtime, Peers; req-quiet-idle-runtime AC-1.3, AC-2.5).
// A wire proxy between the daemon's pool and SingleStore counts every
// statement the daemon sends: each COM_QUERY, COM_STMT_PREPARE, and
// COM_STMT_EXECUTE packet. The store interpolates parameters, so each
// database/sql query or exec is one COM_QUERY.

// statementProxy forwards MySQL protocol connections and records each
// command packet that carries a statement.
type statementProxy struct {
	ln       net.Listener
	upstream string
	mu       sync.Mutex
	commands []proxyCommand
}

type proxyCommand struct {
	at   time.Time
	text string
}

func startStatementProxy(t *testing.T, upstream string) *statementProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &statementProxy{ln: ln, upstream: upstream}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(client)
		}
	}()
	return p
}

func (p *statementProxy) addr() string { return p.ln.Addr().String() }

func (p *statementProxy) serve(client net.Conn) {
	defer client.Close()
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		return
	}
	defer server.Close()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	reader := bufio.NewReader(client)
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			return
		}
		length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		// A command opens with sequence 0; the handshake response and
		// authentication packets never do.
		if header[3] == 0 && length > 0 {
			switch payload[0] {
			case 0x03, 0x16: // COM_QUERY, COM_STMT_PREPARE
				p.record(string(payload[1:]))
			case 0x17: // COM_STMT_EXECUTE
				p.record(fmt.Sprintf("execute statement %d", binary.LittleEndian.Uint32(payload[1:5])))
			}
		}
		if _, err := server.Write(append(append([]byte(nil), header...), payload...)); err != nil {
			return
		}
	}
}

func (p *statementProxy) record(text string) {
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	p.mu.Lock()
	p.commands = append(p.commands, proxyCommand{at: time.Now(), text: text})
	p.mu.Unlock()
}

func (p *statementProxy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.commands)
}

func (p *statementProxy) since(index int) []proxyCommand {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxyCommand(nil), p.commands[index:]...)
}

func describeCommands(start time.Time, commands []proxyCommand) string {
	var b strings.Builder
	for _, c := range commands {
		fmt.Fprintf(&b, "  +%s %s\n", c.at.Sub(start).Round(time.Millisecond), strings.Join(strings.Fields(c.text), " "))
	}
	return b.String()
}

// openProxiedStore opens a daemon pool on the shared database through a
// statement proxy, without resetting the database.
func openProxiedStore(t *testing.T) (*Store, *statementProxy) {
	t.Helper()
	proxy := startStatementProxy(t, sharedDatabase.cfg.Addr)
	proxied := sharedDatabase.cfg.Clone()
	proxied.Addr = proxy.addr()
	proxied.TLSConfig = "false"
	st, err := Open(t.Context(), proxied.FormatDSN(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, proxy
}

// TestSingleStoreCompressionOpenIntegration checks that Open reports
// compression from its own connections' handshakes and does nothing else for
// it: no log line and no statement of its own. conveyord logs the one
// summary at startup (component-runtime invariant 43; component-persistence
// invariant 28).
func TestSingleStoreCompressionOpenIntegration(t *testing.T) {
	resetSharedDatabase(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	proxy := startStatementProxy(t, sharedDatabase.cfg.Addr)
	proxied := sharedDatabase.cfg.Clone()
	proxied.Addr = proxy.addr()
	proxied.TLSConfig = "false"
	st, err := Open(t.Context(), proxied.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if strings.Contains(strings.ToLower(logs.String()), "compression") {
		t.Fatalf("Open logged compression: %s", logs.String())
	}
	for _, command := range proxy.since(0) {
		if strings.Contains(strings.ToUpper(command.text), "SHOW SESSION STATUS") {
			t.Fatalf("Open sent a compression status statement: %s", command.text)
		}
	}
	requested, negotiation := st.Compression()
	if !requested || (negotiation != CompressionNegotiated && negotiation != CompressionNotNegotiated) {
		t.Fatalf("Compression() = %t %s, want the default request and a handshake-derived state", requested, negotiation)
	}
}

// serverGreeting reads the protocol 10 greeting of a fresh connection to
// addr and returns its version string and capability flags.
func serverGreeting(t *testing.T, addr string) (string, uint32) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatal(err)
	}
	size := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	packet := make([]byte, size)
	if _, err := io.ReadFull(conn, packet); err != nil {
		t.Fatal(err)
	}
	if len(packet) == 0 || packet[0] != 10 {
		t.Fatalf("the server sent no protocol 10 greeting: % x", packet)
	}
	versionEnd := bytes.IndexByte(packet[1:], 0)
	lower := 1 + versionEnd + 1 + 4 + 8 + 1
	if versionEnd < 0 || lower+2+1+2+2 > len(packet) {
		t.Fatal("invalid server handshake")
	}
	flags := uint32(binary.LittleEndian.Uint16(packet[lower:])) | uint32(binary.LittleEndian.Uint16(packet[lower+2+1+2:]))<<16
	return string(packet[1 : 1+versionEnd]), flags
}

// TestSingleStoreCompressionWireIntegration requires a store that requests
// compression to negotiate it whenever the server offers CLIENT_COMPRESS,
// read from the tested store's own handshake, and then to receive
// compressed frames: a 256 KiB repetitive result arrives in far fewer wire
// bytes than on the disabled control store. When the server does not offer
// compression, the requested store must still connect uncompressed with no
// error (component-runtime invariant 43), and the test skips with the
// server's version and flags rather than passing. A connection failure, a
// malformed greeting, or a handshake the store did not observe fails.
func TestSingleStoreCompressionWireIntegration(t *testing.T) {
	on := openOwnedStore(t)
	version, flags := serverGreeting(t, sharedDatabase.cfg.Addr)
	advertised := flags&clientCompress != 0
	var memsqlVersion string
	if err := on.db.QueryRowContext(t.Context(), "SELECT @@memsql_version").Scan(&memsqlVersion); err != nil {
		t.Fatal(err)
	}
	t.Logf("server %s (memsql_version %s) capability flags 0x%08x; CLIENT_COMPRESS offered: %t", version, memsqlVersion, flags, advertised)
	off, err := Open(t.Context(), sharedDatabase.cfg.FormatDSN(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer off.Close()
	if requested, negotiation := off.Compression(); requested || negotiation != CompressionNotNegotiated {
		t.Fatalf("disabled control store reports %t %s, want false %s", requested, negotiation, CompressionNotNegotiated)
	}
	requested, negotiation := on.Compression()
	t.Logf("requested store: requested=%t negotiation=%s", requested, negotiation)
	switch {
	case !requested:
		t.Fatal("the default store did not request compression")
	case negotiation == CompressionUnknown:
		t.Fatal("the requested store's handshake was not observed")
	case advertised && negotiation != CompressionNegotiated:
		t.Fatalf("the server offers CLIENT_COMPRESS, but the requested store reports %s", negotiation)
	case !advertised && negotiation != CompressionNotNegotiated:
		t.Fatalf("the server does not offer CLIENT_COMPRESS, but the requested store reports %s", negotiation)
	}
	wire := map[string]uint64{}
	for _, tc := range []struct {
		name string
		st   *Store
	}{{"requested", on}, {"disabled", off}} {
		tc.st.ReadStats(0)
		var payload string
		if err := tc.st.db.QueryRowContext(t.Context(), "SELECT RPAD('', 262144, 'x')").Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 262144 {
			t.Fatalf("%s returned %d payload bytes", tc.name, len(payload))
		}
		wire[tc.name] = tc.st.ReadStats(0).Bytes
		t.Logf("%s received %d wire bytes for %d decoded bytes", tc.name, wire[tc.name], len(payload))
	}
	if wire["disabled"] < 262144 {
		t.Fatalf("the disabled control received %d wire bytes for 262144 decoded bytes", wire["disabled"])
	}
	if !advertised {
		if wire["requested"] < 262144 {
			t.Fatalf("an unnegotiated requested store received %d wire bytes for 262144 decoded bytes", wire["requested"])
		}
		t.Skipf("SingleStore %s (memsql_version %s) does not offer CLIENT_COMPRESS in its greeting (capability flags 0x%08x), so protocol compression cannot be negotiated on this server; the requested store connected uncompressed without error", version, memsqlVersion, flags)
	}
	if wire["requested"]*4 > wire["disabled"] {
		t.Fatalf("compression was negotiated, but the requested read used %d wire bytes versus %d disabled", wire["requested"], wire["disabled"])
	}
}

// TestSingleStorePeerMarksIntegration checks both kinds of cross-connection
// changes. A read while no workspace changed returns no witness row, so its
// one statement looks up no event row.
func TestSingleStorePeerMarksIntegration(t *testing.T) {
	witnessRows := -1
	observe := func(rows int) { witnessRows = rows }
	observePeerWitnessRows.Store(&observe)
	t.Cleanup(func() { observePeerWitnessRows.Store(nil) })
	writer := openOwnedStore(t)
	ctxA, wsA, taskA := seedSingleStoreNoticeTask(t, writer)
	_, wsB, _ := seedSingleStoreNoticeTask(t, writer)
	daemon, proxy := openProxiedStore(t)
	read := func(what string) map[string]changes.PeerMark {
		t.Helper()
		before := proxy.count()
		marks, err := daemon.PeerMarks(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if sent := proxy.since(before); len(sent) != 1 {
			t.Fatalf("%s: PeerMarks should send one statement:\n%s", what, describeCommands(time.Now(), sent))
		}
		return marks
	}
	read("warm-up") // a fresh pool connection is in place before counting
	before := read("before")
	if witnessRows != 0 {
		t.Fatalf("a read after an accepted read returned %d witness rows, want none", witnessRows)
	}
	if again := read("again"); again[wsA] != before[wsA] || again[wsB] != before[wsB] {
		t.Fatalf("a read without a change moved the marks: %v then %v", before, again)
	}
	if witnessRows != 0 {
		t.Fatalf("an idle read returned %d witness rows, want none", witnessRows)
	}
	daemon.peerMu.Lock()
	idleQuery, idleArgs := peerMarksStatement(daemon.peerAccepted)
	daemon.peerMu.Unlock()
	requireIdlePeerReadProfile(t, writer, idleQuery, idleArgs)
	if _, err := logqueue.Enqueue(ctxA, writer.Log(), wsA, "peer_probe", "one", struct{}{}, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	afterJob := read("after enqueue")
	if afterJob[wsA].Log <= before[wsA].Log || afterJob[wsA].Events != before[wsA].Events {
		t.Fatalf("enqueue: %+v -> %+v", before[wsA], afterJob[wsA])
	}
	if afterJob[wsB] != before[wsB] {
		t.Fatalf("enqueue in %s moved %s: %+v -> %+v", wsA, wsB, before[wsB], afterJob[wsB])
	}
	if err := writer.AppendEvent(ctxA, core.Event{TaskID: taskA, Kind: "peer.probe"}); err != nil {
		t.Fatal(err)
	}
	afterEvent := read("after task event")
	if afterEvent[wsA].Events != afterJob[wsA].Events+1 || afterEvent[wsA].Log != afterJob[wsA].Log {
		t.Fatalf("task event: %+v -> %+v", afterJob[wsA], afterEvent[wsA])
	}
	if afterEvent[wsB] != afterJob[wsB] {
		t.Fatalf("task event in %s moved %s", wsA, wsB)
	}
	rollback := errors.New("rollback probe")
	err := writer.withTx(ctxA, func(tx *sql.Tx) error {
		if err := taskEvent(ctxA, tx, core.Event{TaskID: taskA, Kind: "peer.rollback"}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback probe: %v", err)
	}
	if afterRollback := read("after rollback"); afterRollback[wsA] != afterEvent[wsA] {
		t.Fatalf("rolled-back event changed peer mark: %+v -> %+v", afterEvent[wsA], afterRollback[wsA])
	}
	if err := writer.AuditTask(ctxA, taskA, "peer.audit", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	afterAudit := read("after task audit")
	if afterAudit[wsA].Events != afterEvent[wsA].Events+1 {
		t.Fatalf("audit did not advance task mark: %+v -> %+v", afterEvent[wsA], afterAudit[wsA])
	}
	if _, err := writer.db.ExecContext(ctxA, "UPDATE tasks SET assignee_user_id=? WHERE workspace_id=? AND id=?", "peer-user", wsA, taskA); err != nil {
		t.Fatal(err)
	}
	if err := writer.withTx(ctxA, func(tx *sql.Tx) error {
		if err := clearMemberAssignments(ctxA, tx, wsA, "peer-user"); err != nil {
			return err
		}
		var position int64
		if err := tx.QueryRowContext(ctxA, "SELECT position FROM task_event_positions WHERE workspace_id=?", wsA).Scan(&position); err != nil {
			return err
		}
		if position != afterAudit[wsA].Events {
			return fmt.Errorf("task mark advanced before commit: %d", position)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	afterClear := read("after assignment clear")
	if afterClear[wsA].Events != afterAudit[wsA].Events+1 {
		t.Fatalf("assignment clear did not advance task mark: %+v -> %+v", afterAudit[wsA], afterClear[wsA])
	}
}

const peerIdleToken = "peer-idle-token"

// peerIdleAwaitStore signals each completed await_review read.
type peerIdleAwaitStore struct {
	store.Store
	reads chan struct{}
}

func (s *peerIdleAwaitStore) ListCompactEvents(ctx context.Context, taskID string) ([]core.Event, error) {
	events, err := s.Store.ListCompactEvents(ctx, taskID)
	s.reads <- struct{}{}
	return events, err
}

func peerIdleFrames(t *testing.T, ctx context.Context, url string) <-chan string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+peerIdleToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, response.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		defer response.Body.Close()
		defer close(frames)
		reader := bufio.NewReader(response.Body)
		var frame strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				// A keep-alive comment reads nothing and is not a frame.
				if text := frame.String(); !strings.HasPrefix(text, ":") {
					frames <- text
				}
				frame.Reset()
				continue
			}
			frame.WriteString(line)
		}
	}()
	return frames
}

// TestSingleStorePeerCheckIdleDaemonIntegration: a daemon wired as conveyord
// wires it with runtime.peers: true on SingleStore (the queue runtime with
// its deadline alarm, the daemon-wide peer check, the HTTP server, and the
// work-order service), serving two workspaces, an open notify stream, and a
// pending await_review, sends at most two statements in 60 seconds of idle
// time (req-quiet-idle-runtime AC-1.3). Another daemon's task event and
// settled review round then reach the stream and await_review at the next
// check (AC-2.5).
func TestSingleStorePeerCheckIdleDaemonIntegration(t *testing.T) {
	writer := openOwnedStore(t)
	suffix := strings.ToLower(core.NewTaskID())
	workspaces := []string{"peer-idle-a-" + suffix, "peer-idle-b-" + suffix}
	system := func(ws string) context.Context {
		return store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), ws)
	}
	if _, err := writer.BootstrapIdentity(system(workspaces[0]), config.FirstOperatorIdentity{OrganizationName: "Peers", Email: "owner@example.test", DisplayName: "Owner"}, peerIdleToken); err != nil {
		t.Fatal(err)
	}
	owner, err := writer.VerifyPersonalAccessToken(system(workspaces[0]), peerIdleToken)
	if err != nil {
		t.Fatal(err)
	}
	granter := store.WithActor(store.WithCredential(system(""), core.AuthenticatedCredential{ID: "fixture", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}),
		store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	for _, ws := range workspaces {
		if _, err = writer.BootstrapWorkspaceConfig(system(ws), &config.Config{Workspace: ws, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
			t.Fatal(err)
		}
		if members, _ := writer.ListWorkspaceMembers(granter, ws, ""); len(members) == 0 {
			if _, err = writer.GrantWorkspaceRole(granter, owner.Email, ws, core.WorkspaceRoleOperator); err != nil {
				t.Fatalf("grant %s: %v", ws, err)
			}
		}
	}
	now := time.Now().UTC()
	newTask := func(ws string) string {
		t.Helper()
		id := core.NewTaskID()
		if err := writer.CreateTask(system(ws), core.Task{ID: id, Workspace: ws, Repo: "repo", BaseBranch: "main", Branch: "conveyor/task-" + id, Mode: core.TaskModeAuto, PolicyVersion: 1, Level: core.L2, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	streamTask := newTask(workspaces[0])
	awaitTask := newTask(workspaces[1])
	awaitCtx := system(workspaces[1])
	job := core.Job{ID: awaitTask + "-implement", TaskID: awaitTask, Stage: core.StageImplement, State: core.JobPending}
	if err = writer.CreateJob(awaitCtx, job); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(writer).CreateWorkOrder(awaitCtx, core.WorkOrder{ID: job.ID, TaskID: awaitTask, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	order, err := storetest.For(writer).ClaimWorkOrder(awaitCtx, job.ID, core.WorkOrderClaim{SessionID: "peer-idle-session", ClientToken: "peer-idle-client", Agent: "codex", Model: "gpt", Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	order.State = core.WorkOrderSubmitted
	if err = storetest.For(writer).UpdateWorkOrder(awaitCtx, order); err != nil {
		t.Fatal(err)
	}

	// The daemon, through the statement proxy, in conveyord's order.
	daemon, proxy := openProxiedStore(t)
	hub := daemon.Changes()
	runCtx, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	check := changes.SelectPeerCheck(runCtx, config.RuntimeSettings{Peers: true}.Peers, daemon, hub, logqueue.PeerTailInterval, t.Logf)
	if check == nil {
		t.Fatal("SingleStore with peers selected no peer check")
	}
	outcomes := make(chan int, 16)
	check.Checked = func(published int, err error) {
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			t.Errorf("peer check: %v", err)
			published = -1
		}
		outcomes <- published
	}
	runtime := logqueue.NewRuntime(daemon.Log(), logqueue.Options{
		Workspaces: workspaces, Changes: hub, Deadlines: dispatch.StoreDeadlines(daemon), Logf: t.Logf,
	})
	runtime.Register(queue.Registration{Kind: "peer_probe", Handle: func(context.Context, queue.Job) error { return nil }})
	if err = check.Check(runCtx); err != nil {
		t.Fatal(err)
	}
	<-outcomes
	if err = runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runtime.Stop(ctx)
	}()
	go check.Run(runCtx)
	server := httpapi.NewServer(daemon)
	server.Workspaces = daemon
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	streamCtx, closeStream := context.WithCancel(context.Background())
	defer closeStream()
	frames := peerIdleFrames(t, streamCtx, httpServer.URL+"/v1/tasks/"+streamTask+"/events/stream?mode=notify&workspace_id="+workspaces[0])
	select {
	case frame := <-frames:
		if frame != "event: ready\ndata: {}\n" {
			t.Fatalf("first frame=%q", frame)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the notify stream wrote no ready frame")
	}
	awaitStore := &peerIdleAwaitStore{Store: daemon, reads: make(chan struct{}, 8)}
	service := &workorder.Service{Store: awaitStore, Changes: hub}
	type awaitOutcome struct {
		result map[string]any
		err    error
	}
	awaited := make(chan awaitOutcome, 1)
	go func() {
		result, err := service.AwaitReview(awaitCtx, order.ID, "peer-idle-session", 10*time.Minute)
		awaited <- awaitOutcome{result, err}
	}()
	select {
	case <-awaitStore.reads:
	case <-time.After(30 * time.Second):
		t.Fatal("await_review made no first read")
	}

	// Settle: wait for a check that found nothing to publish, so start-up
	// reads and writes are over, then measure 60 seconds.
	settled := false
	for deadline := time.After(3 * logqueue.PeerTailInterval); !settled; {
		select {
		case published := <-outcomes:
			settled = published == 0
		case <-deadline:
			t.Fatal("no peer check found the daemon settled")
		}
	}
	start, first := time.Now(), proxy.count()
	time.Sleep(60 * time.Second)
	window := proxy.since(first)
	t.Logf("statements in 60 s of idle time: %d\n%s", len(window), describeCommands(start, window))
	if len(window) > 2 {
		t.Fatalf("the idle daemon with peers sent %d statements in 60 s, want at most 2:\n%s", len(window), describeCommands(start, window))
	}
	select {
	case frame := <-frames:
		t.Fatalf("the stream wrote %q while idle", frame)
	case outcome := <-awaited:
		t.Fatalf("await_review returned while idle: %+v", outcome)
	default:
	}

	// Another daemon commits a task event and settles the review round
	// partway through a period; both reach this daemon at the next check.
	// Drop the outcomes of the checks inside the window, then wait for a
	// fresh one to start the period.
	for drained := false; !drained; {
		select {
		case <-outcomes:
		default:
			drained = true
		}
	}
	select {
	case <-outcomes:
	case <-time.After(2 * logqueue.PeerTailInterval):
		t.Fatal("no peer check ran")
	}
	time.Sleep(12 * time.Second)
	if err = writer.AppendEvent(system(workspaces[0]), core.Event{TaskID: streamTask, Kind: "peer.probe"}); err != nil {
		t.Fatal(err)
	}
	if err = writer.AppendEvent(awaitCtx, core.Event{TaskID: awaitTask, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "approved", "review_round": 1}), At: time.Now().UTC().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	committed := time.Now()
	select {
	case published := <-outcomes:
		if published < 2 {
			t.Fatalf("the next check published %d notices, want a task notice for each workspace", published)
		}
	case <-time.After(logqueue.PeerTailInterval):
		t.Fatalf("no peer check within %s of the commit", logqueue.PeerTailInterval)
	}
	select {
	case frame := <-frames:
		if frame != "event: activity\ndata: {}\n" {
			t.Fatalf("activity frame=%q", frame)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream wrote no activity frame after the check")
	}
	select {
	case outcome := <-awaited:
		if outcome.err != nil || outcome.result["verdict"] != "approved" {
			t.Fatalf("await_review result=%v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("await_review did not return after the check")
	}
	if latency := time.Since(committed); latency > logqueue.PeerTailInterval {
		t.Fatalf("another daemon's commit reached the stream and await_review %s after it, over %s", latency, logqueue.PeerTailInterval)
	} else {
		t.Logf("another daemon's commit reached the stream and await_review %s after it", latency)
	}
	stopDaemon()
}

// peerReadProfile is what PROFILE reports for one peer read.
type peerReadProfile struct {
	// eventOperators names each operator on events_columnstore.
	eventOperators []string
	// eventRows and eventSegments sum the rows read and the on-disk
	// segments scanned by those operators.
	eventRows, eventSegments float64
	// witnessRows sums the rows the task_event_witnesses operators read.
	witnessRows float64
	// repartitions names each event-table operator that sits below a
	// repartition or broadcast, or reads a repartitioned result table.
	repartitions []string
	json         string
}

// profilePeerRead profiles query on its own connection and walks the JSON
// profile.
func profilePeerRead(t *testing.T, st *Store, query string, args []any) peerReadProfile {
	t.Helper()
	conn, err := st.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(t.Context(), "PROFILE "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var out peerReadProfile
	if err = conn.QueryRowContext(t.Context(), "SHOW PROFILE JSON").Scan(&out.json); err != nil {
		t.Fatal(err)
	}
	var tree any
	if err = json.Unmarshal([]byte(out.json), &tree); err != nil {
		t.Fatalf("profile JSON: %v\n%s", err, out.json)
	}
	var walk func(node any, shuffled bool)
	walk = func(node any, shuffled bool) {
		switch v := node.(type) {
		case map[string]any:
			executor, _ := v["executor"].(string)
			query, _ := v["query"].(string)
			upper := strings.ToUpper(executor + " " + query)
			if strings.Contains(upper, "REPARTITION") || strings.Contains(upper, "BROADCAST") || strings.Contains(upper, "CREATE RESULT TABLE") {
				shuffled = true
			}
			switch table, _ := v["table"].(string); table {
			case "events_columnstore":
				alias, _ := v["alias"].(string)
				out.eventOperators = append(out.eventOperators, executor)
				out.eventRows += profileCounter(v["actual_row_count"])
				out.eventSegments += profileCounter(v["segments_scanned"])
				if shuffled || strings.HasPrefix(alias, "remote_") {
					out.repartitions = append(out.repartitions, executor+" AS "+alias)
				}
			case "task_event_witnesses":
				out.witnessRows += profileCounter(v["actual_row_count"])
			}
			for _, child := range v {
				walk(child, shuffled)
			}
		case []any:
			for _, child := range v {
				walk(child, shuffled)
			}
		}
	}
	walk(tree, false)
	return out
}

// profileCounter reads a SHOW PROFILE JSON counter, a number or an object
// whose value field holds it.
func profileCounter(raw any) float64 {
	switch v := raw.(type) {
	case float64:
		return v
	case map[string]any:
		value, _ := v["value"].(float64)
		return value
	}
	return 0
}

// requireIdlePeerReadProfile proves, from PROFILE, that an idle peer read
// reads no witness row, scans no on-disk segment of events_columnstore, and
// never repartitions it (plan version 2 of task 261118-a22ffc). It profiles
// the statement twice: with the events in the unflushed in-memory segment,
// whose probe is allowed and logged, and after a flush moves them to on-disk
// segments. After the flush, a join on the event ID alone shows the checks
// reject a read that reaches the whole event table. The optimizer either
// repartitions the event table for that join or gathers the event scan to the
// aggregator, which it chose in CI for a few estimated rows; either plan
// scans the flushed on-disk segments, so the control trips a check under
// every plan the optimizer picks.
func requireIdlePeerReadProfile(t *testing.T, st *Store, query string, args []any) {
	t.Helper()
	check := func(stage string) peerReadProfile {
		t.Helper()
		p := profilePeerRead(t, st, query, args)
		if len(p.eventOperators) == 0 {
			t.Fatalf("%s: the idle peer read has no event-table operator to check:\n%s", stage, p.json)
		}
		if p.witnessRows != 0 {
			t.Fatalf("%s: the idle peer read read %v witness rows, want none:\n%s", stage, p.witnessRows, p.json)
		}
		if p.eventSegments != 0 {
			t.Fatalf("%s: the idle peer read scanned %v on-disk event segments, want none:\n%s", stage, p.eventSegments, p.json)
		}
		if len(p.repartitions) != 0 {
			t.Fatalf("%s: the idle peer read repartitioned the event table (%v); the FORCE INDEX hint no longer holds:\n%s", stage, p.repartitions, p.json)
		}
		return p
	}
	unflushed := check("unflushed")
	t.Logf("idle peer read, events unflushed: event-table operators %v, in-memory event rows probed %v, on-disk segments scanned %v, witness rows read %v",
		unflushed.eventOperators, unflushed.eventRows, unflushed.eventSegments, unflushed.witnessRows)
	if _, err := st.db.ExecContext(t.Context(), "OPTIMIZE TABLE events_columnstore FLUSH"); err != nil {
		t.Fatal(err)
	}
	var segments int
	if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM information_schema.columnar_segments WHERE database_name=DATABASE() AND table_name='events_columnstore' AND column_name='id'`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if segments == 0 {
		t.Fatal("the flush left no on-disk event segment to prove the idle read skips")
	}
	control := profilePeerRead(t, st, `SELECT COUNT(*) FROM task_event_witnesses w JOIN events_columnstore e ON e.id = w.event_id`, nil)
	if len(control.eventOperators) == 0 || (len(control.repartitions) == 0 && control.eventSegments == 0) {
		t.Fatalf("the checks reject neither a repartition nor an on-disk segment scan in a join on the ID alone: operators %v\n%s", control.eventOperators, control.json)
	}
	t.Logf("a join on the event ID alone: event-table operators %v, repartitioned %v, on-disk segments scanned %v", control.eventOperators, control.repartitions, control.eventSegments)
	flushed := check("flushed")
	t.Logf("idle peer read, events flushed to %d on-disk segments: in-memory event rows probed %v, on-disk segments scanned %v, witness rows read %v",
		segments, flushed.eventRows, flushed.eventSegments, flushed.witnessRows)
}
