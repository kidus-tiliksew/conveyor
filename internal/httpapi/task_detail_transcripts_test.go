package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// transcriptSentinel marks every stored transcript, so a response that leaks
// content is found by a substring search.
const transcriptSentinel = "TRANSCRIPT-SENTINEL"

// transcriptFixture holds three ended attempts on one implement order of a
// demo task (non-ASCII, empty, and long content), one capture on another demo
// task, and one capture on a task of the foreign workspace.
type transcriptFixture struct {
	*batchHTTPFixture
	task, other, foreign            core.Task
	order, otherOrder, foreignOrder string
	attempts                        []string
	contents                        []string
	otherAttempt, foreignAttempt    string
	foreignCtx                      context.Context
	credentials                     tokenCredentials
	members                         *membershipFixture
	workerSecret                    string
	server                          *Server
	counter                         *taskDetailCountingStore
}

func newTranscriptFixture(t *testing.T, long int) *transcriptFixture {
	t.Helper()
	f := &transcriptFixture{batchHTTPFixture: newBatchHTTPFixture(t)}
	f.foreignCtx = store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "foreign")
	f.task = f.batchHTTPFixture.task(t, "transcripts", core.TaskRunning, "")
	f.order = f.batchHTTPFixture.order(t, f.task, core.StageImplement, nil).ID
	f.contents = []string{
		transcriptSentinel + " naïve café ✓ 終了",
		"",
		transcriptSentinel + " " + strings.Repeat("x", long),
	}
	for i, content := range f.contents {
		f.attempts = append(f.attempts, captureAttempt(t, f.ctx, f.st, f.order, fmt.Sprintf("s%d", i), content))
	}
	f.other = f.batchHTTPFixture.task(t, "other", core.TaskRunning, "")
	f.otherOrder = f.batchHTTPFixture.order(t, f.other, core.StageImplement, nil).ID
	f.otherAttempt = captureAttempt(t, f.ctx, f.st, f.otherOrder, "other", transcriptSentinel+" other task")

	f.foreign = core.Task{ID: core.NewTaskID(), Workspace: "foreign", Title: "foreign", Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: f.now}
	if err := f.st.CreateTask(f.foreignCtx, f.foreign); err != nil {
		t.Fatal(err)
	}
	f.foreignOrder = f.foreign.ID + "-implement-1"
	if err := f.st.CreateJob(f.foreignCtx, core.Job{ID: f.foreignOrder, TaskID: f.foreign.ID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.CreateWorkOrder(f.foreignCtx, f.st, core.WorkOrder{ID: f.foreignOrder, TaskID: f.foreign.ID, JobID: f.foreignOrder, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: f.now, QueueDeadline: f.now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.foreignAttempt = captureAttempt(t, f.foreignCtx, f.st, f.foreignOrder, "foreign", transcriptSentinel+" foreign workspace")

	f.credentials = tokenCredentials{
		"tok-viewer":   {ID: "pat-viewer", OwnerUserID: "usr_viewer", Kind: core.CredentialUser},
		"tok-operator": {ID: "pat-operator", OwnerUserID: "usr_operator", Kind: core.CredentialUser},
		"tok-missing":  {ID: "pat-missing", OwnerUserID: "usr_missing", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator},
		"tok-agent":    {ID: "agent-1", OwnerUserID: "usr_operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeOperator},
	}
	f.members = &membershipFixture{workspaces: []core.Workspace{{ID: "demo"}, {ID: "foreign"}}, roles: map[string]map[string]core.WorkspaceRole{
		"usr_viewer":   {"demo": core.WorkspaceRoleViewer},
		"usr_operator": {"demo": core.WorkspaceRoleOperator},
	}}
	f.workerSecret = "cv_wkr_transcript_fixture"
	sum := sha256.Sum256([]byte(f.workerSecret))
	if err := f.st.CreateWorker(f.ctx, core.Worker{ID: "transcript-worker", Workspace: "demo", OwnerUserID: "usr_operator", Name: "transcript-worker", CredentialHash: hex.EncodeToString(sum[:]), CreatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	f.counter = newTaskDetailCountingStore(f.st)
	f.server = f.newServer(f.counter)
	return f
}

func (f *transcriptFixture) newServer(st store.Store) *Server {
	server := NewServer(st)
	server.Workspace = "demo"
	server.Credentials, server.Workspaces, server.Memberships = f.credentials, f.members, f.members
	return server
}

// captureAttempt claims the order, ends the attempt, and records its
// transcript capture; it returns the attempt ID.
func captureAttempt(t *testing.T, ctx context.Context, st store.Store, orderID, session, content string) string {
	t.Helper()
	const worker = "capture-worker"
	claimed, err := storetest.ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: worker, ClaimantID: worker, SessionID: session + "-" + orderID, ClientToken: session + "-" + orderID, Lease: time.Hour, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatalf("claim %s: %v", orderID, err)
	}
	identity := core.WorkOrderClaimIdentity{WorkerID: worker, ClaimantID: worker, SessionID: claimed.SessionID}
	if _, err = storetest.ReleaseWorkerClaim(ctx, st, orderID, worker, core.WorkOrderRelease{SessionID: claimed.SessionID, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "harness exited", InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond}); err != nil {
		t.Fatalf("release %s: %v", orderID, err)
	}
	if _, err = st.RecordWorkOrderAttemptCapture(ctx, orderID, identity, core.WorkOrderAttemptCapture{SessionID: claimed.SessionID, AttemptID: claimed.AttemptID, Transcript: &core.WorkOrderAttemptTranscript{Content: content, Truncated: true}}); err != nil {
		t.Fatalf("capture %s: %v", orderID, err)
	}
	return claimed.AttemptID
}

type transcriptRequest struct{ token, header string }

func serveTranscript(server *Server, path string, caller transcriptRequest) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if caller.token != "" {
		request.Header.Set("Authorization", "Bearer "+caller.token)
	}
	if caller.header != "" {
		request.Header.Set("X-Workspace-ID", caller.header)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func transcriptPath(workspace, taskID, orderID, attemptID string) string {
	return "/v1/tasks/" + taskID + "/work-orders/" + orderID + "/transcript-captures/" + attemptID + "?workspace_id=" + workspace
}

// boundedOrderCaptures returns the raw transcript_captures entries of one
// order in a detail body, and whether the key was present.
func boundedOrderCaptures(t *testing.T, body []byte, orderID string) ([]map[string]json.RawMessage, bool) {
	t.Helper()
	var detail struct {
		WorkOrders []map[string]json.RawMessage `json:"work_orders"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("detail body: %v", err)
	}
	for _, order := range detail.WorkOrders {
		var id string
		if err := json.Unmarshal(order["id"], &id); err != nil || id != orderID {
			continue
		}
		raw, present := order["transcript_captures"]
		if !present {
			return nil, false
		}
		var captures []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &captures); err != nil {
			t.Fatalf("captures of %s: %v", orderID, err)
		}
		return captures, true
	}
	t.Fatalf("order %s missing from detail", orderID)
	return nil, false
}

// TestBoundedTaskDetailOmitsTranscriptContent proves the bounded view carries
// exactly five metadata fields per capture with UTF-8 byte lengths and no
// content, that only the length numeral grows with the transcript, and that
// the complete view of the same fixture keeps every capture's content
// (component-http-api, Bounded detail view).
func TestBoundedTaskDetailOmitsTranscriptContent(t *testing.T) {
	small, large := newTranscriptFixture(t, 10), newTranscriptFixture(t, 100_000)
	metadataBytes := map[string]int{}
	for name, f := range map[string]*transcriptFixture{"small": small, "large": large} {
		bounded := serveTranscript(f.server, detailPath(f.task.ID, true), transcriptRequest{token: "tok-viewer"})
		if bounded.Code != http.StatusOK {
			t.Fatalf("%s bounded: %d %s", name, bounded.Code, bounded.Body)
		}
		if strings.Contains(bounded.Body.String(), transcriptSentinel) {
			// Work orders are also embedded in stalled and recovery states.
			t.Fatalf("%s bounded detail leaks transcript text at %v", name, sentinelPaths(t, bounded.Body.Bytes()))
		}
		// The stalled state embeds the order with the same metadata entries.
		var stalled struct {
			Stalled struct {
				WorkOrder struct {
					TranscriptCaptures []map[string]json.RawMessage `json:"transcript_captures"`
				} `json:"work_order"`
			} `json:"stalled"`
		}
		if err := json.Unmarshal(bounded.Body.Bytes(), &stalled); err != nil || len(stalled.Stalled.WorkOrder.TranscriptCaptures) != len(f.contents) || stalled.Stalled.WorkOrder.TranscriptCaptures[0]["content_bytes"] == nil {
			t.Fatalf("%s stalled work order captures: %v %+v", name, err, stalled)
		}
		captures, present := boundedOrderCaptures(t, bounded.Body.Bytes(), f.order)
		if !present || len(captures) != len(f.contents) {
			t.Fatalf("%s bounded captures: present=%v %d, want %d", name, present, len(captures), len(f.contents))
		}
		for i, capture := range captures {
			keys := make([]string, 0, len(capture))
			for key := range capture {
				keys = append(keys, key)
			}
			want := []string{"attempt_id", "captured_at", "content_bytes", "termination_reason", "truncated"}
			if !sameKeys(keys, want) {
				t.Fatalf("%s capture %d keys %v, want %v", name, i, keys, want)
			}
			var bytesCount int
			var attempt string
			if json.Unmarshal(capture["content_bytes"], &bytesCount) != nil || json.Unmarshal(capture["attempt_id"], &attempt) != nil {
				t.Fatalf("%s capture %d fields: %v", name, i, capture)
			}
			if attempt != f.attempts[i] || bytesCount != len([]byte(f.contents[i])) {
				t.Fatalf("%s capture %d: attempt %s bytes %d, want %s %d", name, i, attempt, bytesCount, f.attempts[i], len([]byte(f.contents[i])))
			}
			if string(capture["truncated"]) != "true" {
				t.Fatalf("%s capture %d truncated: %s", name, i, capture["truncated"])
			}
		}
		// The non-ASCII capture counts bytes, not runes.
		if first := f.contents[0]; len(first) == len([]rune(first)) {
			t.Fatal("fixture content is not multibyte")
		}
		raw, _ := json.Marshal(captures)
		lengths := 0
		for i := range f.contents {
			lengths += len(fmt.Sprint(len(f.contents[i])))
		}
		// Attempt IDs and capture times differ per fixture; remove them and
		// the length numerals so the remaining bytes must be equal.
		metadataBytes[name] = len(raw) - lengths - attemptAndTimeBytes(captures)
		// An order without captures keeps its absent key.
		if _, present := boundedOrderCaptures(t, serveTranscript(f.server, detailPath(f.other.ID, true), transcriptRequest{token: "tok-viewer"}).Body.Bytes(), f.otherOrder); !present {
			t.Fatalf("%s: other order lost its captures", name)
		}

		complete := serveTranscript(f.server, detailPath(f.task.ID, false), transcriptRequest{token: "tok-viewer"})
		if complete.Code != http.StatusOK {
			t.Fatalf("%s complete: %d %s", name, complete.Code, complete.Body)
		}
		var detail struct {
			WorkOrders []core.WorkOrder `json:"work_orders"`
		}
		if err := json.Unmarshal(complete.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		stored, err := f.st.ListWorkOrderTranscriptCaptures(f.ctx, f.order)
		if err != nil {
			t.Fatal(err)
		}
		for _, order := range detail.WorkOrders {
			if order.ID == f.order && !reflect.DeepEqual(order.TranscriptCaptures, stored) {
				t.Fatalf("%s complete captures differ from the store:\n%+v\n%+v", name, order.TranscriptCaptures, stored)
			}
		}
		if !strings.Contains(complete.Body.String(), `"content":""`) {
			t.Fatalf("%s complete view lost the empty capture's content key", name)
		}
		// The bounded projection never mutated the store's captures.
		if again, _ := f.st.ListWorkOrderTranscriptCaptures(f.ctx, f.order); !reflect.DeepEqual(again, stored) || again[0].Content != f.contents[0] {
			t.Fatalf("%s store captures changed", name)
		}
	}
	if metadataBytes["small"] != metadataBytes["large"] {
		t.Fatalf("bounded capture bytes grow with transcript length beyond the length numeral: %v", metadataBytes)
	}
}

// sentinelPaths names every string value of body that holds transcript text.
func sentinelPaths(t *testing.T, body []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var walk func(any, string)
	walk = func(value any, path string) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				walk(child, path+"."+key)
			}
		case []any:
			for i, child := range typed {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		case string:
			if strings.Contains(typed, transcriptSentinel) {
				paths = append(paths, path)
			}
		}
	}
	walk(doc, "")
	return paths
}

func sameKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := map[string]bool{}
	for _, key := range got {
		set[key] = true
	}
	for _, key := range want {
		if !set[key] {
			return false
		}
	}
	return true
}

func attemptAndTimeBytes(captures []map[string]json.RawMessage) int {
	total := 0
	for _, capture := range captures {
		total += len(capture["attempt_id"]) + len(capture["captured_at"])
	}
	return total
}

// failingTranscriptStore fails one named read with an infrastructure error.
type failingTranscriptStore struct {
	store.Store
	failOrder, failCaptures bool
}

var errTranscriptStoreDown = errors.New("store unavailable")

func (s failingTranscriptStore) GetWorkOrder(ctx context.Context, id string) (core.WorkOrder, error) {
	if s.failOrder {
		return core.WorkOrder{}, errTranscriptStoreDown
	}
	return s.Store.GetWorkOrder(ctx, id)
}

func (s failingTranscriptStore) ListWorkOrderTranscriptCaptures(ctx context.Context, id string) ([]core.WorkOrderTranscriptCapture, error) {
	if s.failCaptures {
		return nil, errTranscriptStoreDown
	}
	return s.Store.ListWorkOrderTranscriptCaptures(ctx, id)
}

// TestTaskTranscriptCaptureRead proves the content route returns exactly the
// requested stored capture, refuses every absent or foreign record with the
// same 404 and no content, matches task detail's admission answers, and
// answers 500 for a failed read (component-http-api, Transcript capture
// content read).
func TestTaskTranscriptCaptureRead(t *testing.T) {
	f := newTranscriptFixture(t, 10)
	stored, err := f.st.ListWorkOrderTranscriptCaptures(f.ctx, f.order)
	if err != nil {
		t.Fatal(err)
	}
	for i, capture := range stored {
		f.counter.reset()
		response := serveTranscript(f.server, transcriptPath("demo", f.task.ID, f.order, f.attempts[i]), transcriptRequest{token: "tok-viewer"})
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("capture %d: %d %q %s", i, response.Code, response.Header().Get("Cache-Control"), response.Body)
		}
		var got transcriptCaptureContentView
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		want := transcriptCaptureContentView{AttemptID: capture.AttemptID, Content: capture.Content, TerminationReason: capture.TerminationReason, Truncated: capture.Truncated, CapturedAt: capture.CapturedAt, ContentBytes: len(capture.Content)}
		if !reflect.DeepEqual(got, want) || got.Content != f.contents[i] {
			t.Fatalf("capture %d: %+v, want %+v", i, got, want)
		}
		if f.contents[i] == "" && !strings.Contains(response.Body.String(), `"content":""`) {
			t.Fatalf("empty capture lost its content key: %s", response.Body)
		}
		calls := f.counter.reset()
		if calls["GetTask"] != 1 || calls["GetWorkOrder"] != 1 || calls["ListWorkOrderTranscriptCaptures"] != 1 || calls["ListTaskTranscriptCaptures"] != 0 {
			t.Fatalf("capture %d store calls: %v", i, calls)
		}
	}

	notFound := map[string]string{
		"missing task":               transcriptPath("demo", "missing-task", f.order, f.attempts[0]),
		"missing order":              transcriptPath("demo", f.task.ID, "missing-order", f.attempts[0]),
		"missing attempt":            transcriptPath("demo", f.task.ID, f.order, "attempt-missing"),
		"order of another task":      transcriptPath("demo", f.task.ID, f.otherOrder, f.otherAttempt),
		"other task through order":   transcriptPath("demo", f.other.ID, f.order, f.attempts[0]),
		"attempt of another order":   transcriptPath("demo", f.task.ID, f.order, f.otherAttempt),
		"task of another workspace":  transcriptPath("demo", f.foreign.ID, f.foreignOrder, f.foreignAttempt),
		"order of another workspace": transcriptPath("demo", f.task.ID, f.foreignOrder, f.foreignAttempt),
	}
	for name, path := range notFound {
		response := serveTranscript(f.server, path, transcriptRequest{token: "tok-operator"})
		if response.Code != http.StatusNotFound || response.Body.String() != "404 page not found\n" {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body)
		}
	}

	// Admission matches task detail for the same caller and selectors.
	callers := map[string]struct {
		request   transcriptRequest
		workspace string
	}{
		"viewer":                       {transcriptRequest{token: "tok-viewer"}, "demo"},
		"unauthenticated":              {transcriptRequest{}, "demo"},
		"unknown token":                {transcriptRequest{token: "tok-unknown"}, "demo"},
		"agent credential":             {transcriptRequest{token: "tok-agent"}, "demo"},
		"worker credential":            {transcriptRequest{token: f.workerSecret}, "demo"},
		"missing binding":              {transcriptRequest{token: "tok-missing"}, "demo"},
		"foreign workspace no binding": {transcriptRequest{token: "tok-operator"}, "foreign"},
		"conflicting selectors":        {transcriptRequest{token: "tok-operator", header: "foreign"}, "demo"},
	}
	for name, caller := range callers {
		transcript := serveTranscript(f.server, transcriptPath(caller.workspace, f.task.ID, f.order, f.attempts[0]), caller.request)
		detail := serveTranscript(f.server, "/v1/tasks/"+f.task.ID+"/activity?view=bounded&workspace_id="+caller.workspace, caller.request)
		if transcript.Code != detail.Code {
			t.Fatalf("%s: transcript %d, detail %d", name, transcript.Code, detail.Code)
		}
		if transcript.Code != http.StatusOK {
			if !bytes.Equal(transcript.Body.Bytes(), detail.Body.Bytes()) || transcript.Header().Get("WWW-Authenticate") != detail.Header().Get("WWW-Authenticate") {
				t.Fatalf("%s: refusal differs from task detail:\ntranscript %q\ndetail     %q", name, transcript.Body, detail.Body)
			}
			if strings.Contains(transcript.Body.String(), transcriptSentinel) {
				t.Fatalf("%s: refusal leaks transcript text", name)
			}
		}
	}
	for name, want := range map[string]int{"viewer": http.StatusOK, "unauthenticated": http.StatusUnauthorized, "agent credential": http.StatusUnauthorized, "worker credential": http.StatusUnauthorized, "missing binding": http.StatusNotFound, "conflicting selectors": http.StatusBadRequest} {
		caller := callers[name]
		if got := serveTranscript(f.server, transcriptPath(caller.workspace, f.task.ID, f.order, f.attempts[0]), caller.request).Code; got != want {
			t.Fatalf("%s: status %d, want %d", name, got, want)
		}
	}

	for name, st := range map[string]failingTranscriptStore{
		"order read":   {Store: f.st, failOrder: true},
		"capture read": {Store: f.st, failCaptures: true},
	} {
		response := serveTranscript(f.newServer(st), transcriptPath("demo", f.task.ID, f.order, f.attempts[0]), transcriptRequest{token: "tok-viewer"})
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), transcriptSentinel) {
			t.Fatalf("%s failure: %d %s", name, response.Code, response.Body)
		}
	}
}
