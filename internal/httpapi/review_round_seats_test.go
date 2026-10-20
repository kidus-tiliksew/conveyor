package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// failingSeatStore fails the RS-5 store command with a scripted error.
type failingSeatStore struct {
	store.Store
	err error
}

func (s *failingSeatStore) AddReviewSeatsCommand(context.Context, taskops.TaskLease, store.ReviewSeatAdditionRequest) (store.ReviewSeatAdditionResult, error) {
	return store.ReviewSeatAdditionResult{}, s.err
}

// seatHTTPFixture serves a workspace "alpha" task with an open one-seat
// review round to an operator, a contributor, an operator of another
// workspace, and an agent credential.
func seatHTTPFixture(t *testing.T, wrap func(store.Store) store.Store) (http.Handler, store.Store) {
	t.Helper()
	base := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha")
	for _, id := range []string{"seat-http", "seat-settled"} {
		requireOKHTTP(t, base.CreateTask(ctx, core.Task{ID: id, Workspace: "alpha", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}))
		jobID := id + "-review-1-seat-1"
		now := time.Now().UTC()
		requireOKHTTP(t, storetest.CreateReviewRound(ctx, base, id, []core.Job{{ID: jobID, TaskID: id, Stage: core.StageReview, State: core.JobPending}},
			[]core.WorkOrder{{ID: jobID, JobID: jobID, TaskID: id, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, HeadSHA: "head", QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}}))
	}
	requireOKHTTP(t, taskops.New(base).AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: "seat-settled", JobID: "seat-settled-review-1-seat-1", ReviewWorkOrderID: "seat-settled-review-1-seat-1",
		ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "verified", Summary: "settled", MaxBounces: 3}))
	st := base
	if wrap != nil {
		st = wrap(base)
	}
	fixture := &membershipFixture{
		workspaces: []core.Workspace{{ID: "alpha"}, {ID: "beta"}},
		roles: map[string]map[string]core.WorkspaceRole{
			"operator":    {"alpha": core.WorkspaceRoleOperator},
			"contributor": {"alpha": core.WorkspaceRoleContributor},
			"outsider":    {"beta": core.WorkspaceRoleOperator},
		},
	}
	server := NewServer(st)
	server.Workspaces, server.Memberships = fixture, fixture
	server.WorkOrders = &workorder.Service{Store: st}
	server.Credentials = staticCredentialVerifier{
		"operator-token":    {ID: "pat_operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"contributor-token": {ID: "pat_contributor", OwnerUserID: "contributor", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"outsider-token":    {ID: "pat_outsider", OwnerUserID: "outsider", Kind: core.CredentialUser, Scope: core.CredentialScopeUser},
		"agent-token":       {ID: "agt_operator", OwnerUserID: "operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser},
	}
	return server.Handler(), base
}

func requireOKHTTP(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func postSeats(handler http.Handler, token, taskID, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/review-round/seats?workspace_id=alpha", strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func seatOrderCount(t *testing.T, st store.Store, taskID string) int {
	t.Helper()
	orders, err := st.ListTaskWorkOrders(store.WithWorkspace(t.Context(), "alpha"), taskID)
	requireOKHTTP(t, err)
	return len(orders)
}

func seatErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", response.Body.String(), err)
	}
	return body.Error
}

// TestAddReviewSeatsHTTPAuthorization: only a user credential holding
// operate_gates in the task's workspace may add seats (RS-5).
func TestAddReviewSeatsHTTPAuthorization(t *testing.T) {
	if core.RoleAllows(core.WorkspaceRoleContributor, core.CapabilityOperateGates) {
		t.Fatal("contributor fixture unexpectedly holds operate_gates")
	}
	handler, st := seatHTTPFixture(t, nil)
	body := `{"request_id":"auth-1","reason":"second opinion","add":1}`
	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"Unauthenticated", "", http.StatusUnauthorized},
		{"AgentCredential", "agent-token", http.StatusUnauthorized},
		{"OtherWorkspaceOperator", "outsider-token", http.StatusNotFound},
		{"ContributorWithoutOperateGates", "contributor-token", http.StatusNotFound},
	} {
		if response := postSeats(handler, tc.token, "seat-http", body); response.Code != tc.status {
			t.Fatalf("%s: status=%d body=%s", tc.name, response.Code, response.Body)
		}
	}
	if count := seatOrderCount(t, st, "seat-http"); count != 1 {
		t.Fatalf("refused callers wrote orders: %d", count)
	}
	response := postSeats(handler, "operator-token", "seat-http", body)
	if response.Code != http.StatusOK {
		t.Fatalf("operator status=%d body=%s", response.Code, response.Body)
	}
	var result store.ReviewSeatAdditionResult
	requireOKHTTP(t, json.Unmarshal(response.Body.Bytes(), &result))
	if result != (store.ReviewSeatAdditionResult{RequestID: "auth-1", TaskID: "seat-http", ReviewRound: 1, PriorSeatCount: 1, SeatCount: 2}) {
		t.Fatalf("result=%+v", result)
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "alpha"), "seat-http")
	requireOKHTTP(t, err)
	if last := events[len(events)-1]; last.Kind != "review.seats_added" || last.ActorID != store.UserActorID("operator") || last.ActorRole != core.ActorUser {
		t.Fatalf("last event=%+v", last)
	}
}

// TestAddReviewSeatsHTTPStrictBody: every malformed body is 400
// invalid_review_seats and writes nothing.
func TestAddReviewSeatsHTTPStrictBody(t *testing.T) {
	handler, st := seatHTTPFixture(t, nil)
	for _, body := range []string{
		``,
		`null`,
		`[]`,
		`{}`,
		`{"request_id":"r","reason":"x","add":1,"harness":"codex"}`,
		`{"request_id":"r","reason":"x","add":1} {}`,
		`{"request_id":"r","reason":"x","add":1}x`,
		`{"reason":"x","add":1}`,
		`{"request_id":" ","reason":"x","add":1}`,
		`{"request_id":"r","add":1}`,
		`{"request_id":"r","reason":"\t","add":1}`,
		`{"request_id":"r","reason":null,"add":1}`,
		`{"request_id":7,"reason":"x","add":1}`,
		`{"request_id":"r","reason":"x"}`,
		`{"request_id":"r","reason":"x","add":null}`,
		`{"request_id":"r","reason":"x","add":"1"}`,
		`{"request_id":"r","reason":"x","add":1.5}`,
		`{"request_id":"r","reason":"x","add":1.0}`,
		`{"request_id":"r","reason":"x","add":1e1}`,
		`{"request_id":"r","reason":"x","add":0}`,
		`{"request_id":"r","reason":"x","add":-1}`,
		`{"request_id":"r","reason":"x","add":2147483648}`,
		`{"request_id":"r","reason":"x","add":99999999999999999999}`,
		`{"request_id":"r","reason":"x","add":true}`,
		fmt.Sprintf(`{"request_id":%q,"reason":"x","add":1}`, strings.Repeat("r", 256)),
	} {
		response := postSeats(handler, "operator-token", "seat-http", body)
		if response.Code != http.StatusBadRequest || seatErrorCode(t, response) != "invalid_review_seats" {
			t.Fatalf("body %q: status=%d body=%s", body, response.Code, response.Body)
		}
	}
	if count := seatOrderCount(t, st, "seat-http"); count != 1 {
		t.Fatalf("invalid bodies wrote orders: %d", count)
	}
}

// TestAddReviewSeatsHTTPConflictsAndReplay covers the exact 409 codes, an
// identical replay, and a changed body under the same request ID.
func TestAddReviewSeatsHTTPConflictsAndReplay(t *testing.T) {
	handler, st := seatHTTPFixture(t, nil)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha")
	requireOKHTTP(t, st.CreateTask(ctx, core.Task{ID: "seat-none", Workspace: "alpha", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}))
	for _, tc := range []struct{ task, code string }{{"seat-none", "review_round_not_open"}, {"seat-settled", "review_round_settled"}} {
		response := postSeats(handler, "operator-token", tc.task, `{"request_id":"refused-`+tc.task+`","reason":"x","add":1}`)
		if response.Code != http.StatusConflict || seatErrorCode(t, response) != tc.code {
			t.Fatalf("%s: status=%d body=%s", tc.task, response.Code, response.Body)
		}
	}
	body := `{"request_id":"replay-1","reason":"second opinion","add":2}`
	first := postSeats(handler, "operator-token", "seat-http", body)
	again := postSeats(handler, "operator-token", "seat-http", body)
	if first.Code != http.StatusOK || again.Code != http.StatusOK || first.Body.String() != again.Body.String() {
		t.Fatalf("replay first=%d %s again=%d %s", first.Code, first.Body, again.Code, again.Body)
	}
	if count := seatOrderCount(t, st, "seat-http"); count != 3 {
		t.Fatalf("replay orders=%d", count)
	}
	changed := postSeats(handler, "operator-token", "seat-http", `{"request_id":"replay-1","reason":"second opinion","add":1}`)
	if changed.Code != http.StatusConflict || seatErrorCode(t, changed) != "review_seat_addition_conflict" {
		t.Fatalf("changed replay status=%d body=%s", changed.Code, changed.Body)
	}
	if missing := postSeats(handler, "operator-token", "seat-missing", `{"request_id":"missing","reason":"x","add":1}`); missing.Code != http.StatusNotFound {
		t.Fatalf("missing task status=%d body=%s", missing.Code, missing.Body)
	}
}

// TestAddReviewSeatsHTTPStoreFailure: an unexpected store failure is 500
// without its text and never a success or a 409.
func TestAddReviewSeatsHTTPStoreFailure(t *testing.T) {
	handler, _ := seatHTTPFixture(t, func(st store.Store) store.Store {
		return &failingSeatStore{Store: st, err: fmt.Errorf("wrapped: %w", errors.New("secret database detail"))}
	})
	response := postSeats(handler, "operator-token", "seat-http", `{"request_id":"fail-1","reason":"x","add":1}`)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("store failure status=%d body=%s", response.Code, response.Body)
	}
	handler, _ = seatHTTPFixture(t, func(st store.Store) store.Store {
		return &failingSeatStore{Store: st, err: fmt.Errorf("%w: task gone", store.ErrNotFound)}
	})
	if response := postSeats(handler, "operator-token", "seat-http", `{"request_id":"fail-2","reason":"x","add":1}`); response.Code != http.StatusNotFound {
		t.Fatalf("not-found status=%d body=%s", response.Code, response.Body)
	}
}
