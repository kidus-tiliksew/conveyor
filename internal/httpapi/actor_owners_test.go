package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// fixtureActorOwners stands in for the stored rows. The backend read itself is
// proved on all three backends by the Tokens/RetiredAgentCredential conformance
// case, which also reads the REST surfaces over real rows.
type fixtureActorOwners struct {
	owners map[string]string
	err    error
	reads  atomic.Int32
}

func (f *fixtureActorOwners) AgentActorOwners(_ context.Context, ids []string) (map[string]string, error) {
	f.reads.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	found := map[string]string{}
	for _, id := range ids {
		if owner, ok := f.owners[id]; ok {
			found[id] = owner
		}
	}
	return found, nil
}

const (
	revokedAgentActor = "agent:agt_revoked"
	unknownAgentActor = "agent:agt_never_stored"
	ownerOfRevoked    = "usr_owner_of_revoked"
)

// actorOwnerFixture holds one task for each kind of recorded actor, read through
// the full handler by a viewer, the lowest role that displays them.
type actorOwnerFixture struct {
	server *Server
	owners *fixtureActorOwners
	ctx    context.Context
	token  string
	f      *governanceAuthFixture
}

func newActorOwnerFixture(t *testing.T) *actorOwnerFixture {
	t.Helper()
	f := newGovernanceAuthFixture(t)
	owners := &fixtureActorOwners{owners: map[string]string{"agt_revoked": ownerOfRevoked}}
	f.server.ActorOwners = owners
	token := f.user("usr_display_viewer", core.WorkspaceRoleViewer)
	fixture := &actorOwnerFixture{server: f.server, owners: owners, ctx: f.ctx, token: token, f: f}
	for id, actor := range map[string]store.Actor{
		"act-revoked": {ID: revokedAgentActor, Role: core.ActorAgent},
		"act-unknown": {ID: unknownAgentActor, Role: core.ActorAgent},
		"act-round":   {ID: store.ReviewRoundActorID(2), Role: core.ActorAgent},
		"act-person":  {ID: store.UserActorID("usr_person"), Role: core.ActorUser},
	} {
		ctx := store.WithActor(f.ctx, actor)
		if err := f.store.CreateTask(ctx, core.Task{ID: id, Workspace: governanceWorkspace, Repo: "conveyor", Branch: "conveyor/task-" + id, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.CreateIntervention(ctx, core.Intervention{TaskID: id, Action: core.InterventionRedirect, ActorID: actor.ID, ActorRole: actor.Role, ReasonCode: "fixture", At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (a *actorOwnerFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("X-Workspace-ID", governanceWorkspace)
	response := httptest.NewRecorder()
	a.f.handler.ServeHTTP(response, request)
	return response
}

type attributedActor struct {
	Kind    string `json:"kind"`
	ActorID string `json:"actor_id"`
	Owner   string `json:"actor_owner_user_id"`
}

func requireAttributed(t *testing.T, surface string, got attributedActor, wantActor, wantOwner string) {
	t.Helper()
	if got.ActorID != wantActor || got.Owner != wantOwner {
		t.Fatalf("%s: actor=%q owner=%q, want %q and %q", surface, got.ActorID, got.Owner, wantActor, wantOwner)
	}
}

var actorOwnerCases = []struct {
	task, actor, owner string
}{
	{"act-revoked", revokedAgentActor, ownerOfRevoked},
	{"act-unknown", unknownAgentActor, ""},
	{"act-round", "review:round:2", ""},
	{"act-person", "user:usr_person", ""},
}

// TestActorOwnerOnRESTEventSurfaces proves each REST read that presents a recorded
// actor resolves an agent:<id> to its owner, keeps the recorded actor, and leaves
// an unknown credential readable with no owner (req-security-boundaries AC-1.3).
func TestActorOwnerOnRESTEventSurfaces(t *testing.T) {
	a := newActorOwnerFixture(t)
	for _, tc := range actorOwnerCases {
		t.Run(tc.task, func(t *testing.T) {
			read := func(path string, into any) {
				t.Helper()
				response := a.get(t, path)
				if response.Code != http.StatusOK {
					t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
				}
				if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
					t.Fatalf("%s: %v", path, err)
				}
			}
			createdEvent := func(surface string, events []attributedActor) attributedActor {
				t.Helper()
				for _, event := range events {
					if event.Kind == "task.created" {
						return event
					}
				}
				t.Fatalf("%s: no task.created in %+v", surface, events)
				return attributedActor{}
			}
			var events []attributedActor
			read("/v1/tasks/"+tc.task+"/events", &events)
			requireAttributed(t, "events", createdEvent("events", events), tc.actor, tc.owner)

			var history struct {
				Events []attributedActor `json:"events"`
			}
			read("/v1/tasks/"+tc.task+"/history", &history)
			requireAttributed(t, "history", createdEvent("history", history.Events), tc.actor, tc.owner)

			var complete struct {
				Events        []attributedActor `json:"events"`
				Interventions []attributedActor `json:"interventions"`
			}
			read("/v1/tasks/"+tc.task+"/activity", &complete)
			requireAttributed(t, "complete activity", createdEvent("complete activity", complete.Events), tc.actor, tc.owner)
			requireAttributed(t, "complete activity intervention", complete.Interventions[0], tc.actor, tc.owner)

			var bounded struct {
				Summaries struct {
					Creator attributedActor `json:"creator"`
				} `json:"summaries"`
			}
			read("/v1/tasks/"+tc.task+"/activity?view=bounded", &bounded)
			requireAttributed(t, "bounded creator summary", bounded.Summaries.Creator, tc.actor, tc.owner)

			var interventions []attributedActor
			read("/v1/tasks/"+tc.task+"/interventions", &interventions)
			requireAttributed(t, "interventions", interventions[0], tc.actor, tc.owner)

			stored, err := a.f.store.ListEvents(a.ctx, tc.task)
			if err != nil || len(stored) == 0 {
				t.Fatalf("stored events=%v err=%v", stored, err)
			}
			var audit struct {
				Event attributedActor `json:"event"`
			}
			read("/v1/tasks/"+tc.task+"/audit/event/"+formatTaskEventID(stored[0].ID), &audit)
			requireAttributed(t, "audit event", audit.Event, tc.actor, tc.owner)
			if stored[0].ActorOwnerUserID != "" {
				t.Fatalf("the store returned a read-only owner: %+v", stored[0])
			}
		})
	}
}

// TestActorOwnerOnMCPEventSurfaces proves the same over MCP's list_task_events.
func TestActorOwnerOnMCPEventSurfaces(t *testing.T) {
	a := newActorOwnerFixture(t)
	for _, tc := range actorOwnerCases {
		t.Run(tc.task, func(t *testing.T) {
			text, isErr := a.f.call(a.token, "list_task_events", map[string]any{"workspace_id": governanceWorkspace, "task_id": tc.task, "event_kind": "task.created"})
			if isErr {
				t.Fatalf("list_task_events refused: %s", text)
			}
			var page struct {
				Items []attributedActor `json:"items"`
			}
			if err := json.Unmarshal([]byte(text), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("items=%+v", page.Items)
			}
			requireAttributed(t, "list_task_events", page.Items[0], tc.actor, tc.owner)
			if tc.owner == "" && strings.Contains(text, "actor_owner_user_id") {
				t.Fatalf("an owner was invented: %s", text)
			}
		})
	}
	// A document's history projects the same field.
	events := []core.Event{{ID: 1, Kind: "requirement.created", ActorID: revokedAgentActor, ActorRole: core.ActorAgent, Payload: json.RawMessage(`{}`), At: time.Now().UTC()}}
	if err := a.server.annotateEventActorOwners(a.ctx, events); err != nil {
		t.Fatal(err)
	}
	projected := mcpReadEvents(events, "")
	if item := projected[0].(map[string]any); item["actor_owner_user_id"] != ownerOfRevoked || item["actor_id"] != revokedAgentActor {
		t.Fatalf("document event item=%v", item)
	}
}

// TestActorOwnerOnRoundSummaryAuthorAndStream covers the round summary's author,
// which the dashboard prints, and the activity stream's frames.
func TestActorOwnerOnRoundSummaryAuthorAndStream(t *testing.T) {
	a := newActorOwnerFixture(t)
	summaries := taskDetailSummaries{
		Creator: &taskCreatorSummary{ActorID: revokedAgentActor, ActorRole: core.ActorAgent},
		ReviewRoundSummaries: taskRoundSummaries{
			"1": {Author: taskRoundSummaryAuthor{ActorID: revokedAgentActor, ActorRole: core.ActorAgent}},
			"2": {Author: taskRoundSummaryAuthor{ActorID: unknownAgentActor, ActorRole: core.ActorAgent}},
			"3": {Author: taskRoundSummaryAuthor{ActorID: "user:usr_person", ActorRole: core.ActorUser}},
		},
	}
	if err := a.server.annotateSummaryActorOwners(a.ctx, &summaries); err != nil {
		t.Fatal(err)
	}
	if summaries.Creator.ActorOwnerUserID != ownerOfRevoked {
		t.Fatalf("creator=%+v", summaries.Creator)
	}
	for round, want := range map[string]string{"1": ownerOfRevoked, "2": "", "3": ""} {
		if got := summaries.ReviewRoundSummaries[round].Author.ActorOwnerUserID; got != want {
			t.Fatalf("round %s author owner=%q, want %q", round, got, want)
		}
	}

	stream := newTaskEventStream(a.server.Store, "act-revoked", func(context.Context) (*redact.Redactor, error) { return redact.New(nil), nil })
	stream.annotate = a.server.annotateEventActorOwners
	var frames strings.Builder
	if err := stream.poll(a.ctx, &frames); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frames.String(), `"actor_id":"`+revokedAgentActor+`"`) || !strings.Contains(frames.String(), `"actor_owner_user_id":"`+ownerOfRevoked+`"`) {
		t.Fatalf("stream frames=%s", frames.String())
	}
}

// TestActorOwnerReadIssuesNoQueryWithoutAnAgentActor keeps ordinary reads free of
// the owner lookup: a page of people's and system acts reads nothing.
func TestActorOwnerReadIssuesNoQueryWithoutAnAgentActor(t *testing.T) {
	a := newActorOwnerFixture(t)
	if response := a.get(t, "/v1/tasks/act-person/events"); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if response := a.get(t, "/v1/tasks/act-round/events"); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if got := a.owners.reads.Load(); got != 0 {
		t.Fatalf("owner reads=%d, want none", got)
	}
}

// TestActorOwnerStoreFailureFailsTheReadWithoutLeaking proves a failed owner read
// is never presented as an unresolved actor: the read fails with the generic
// answer and no text from the store (DEC-105(1)).
func TestActorOwnerStoreFailureFailsTheReadWithoutLeaking(t *testing.T) {
	a := newActorOwnerFixture(t)
	const leaked = "pgx: secret-dsn-fragment"
	a.owners.err = errors.New(leaked)
	for _, path := range []string{"/v1/tasks/act-revoked/events", "/v1/tasks/act-revoked/interventions", "/v1/tasks/act-revoked/activity", "/v1/tasks/act-revoked/activity?view=bounded", "/v1/tasks/act-revoked/history"} {
		response := a.get(t, path)
		if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), leaked) {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
