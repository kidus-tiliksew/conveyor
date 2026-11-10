package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// A recorded merge bypass appears in the inbox acts log of GET
// /v1/next-actions, named by its task, forge actor, head, and missing
// approvals, whoever's read observed the merge. The merged task adds no
// decision item and no attention (req-risk-based-review AC-10.3;
// component-next-action invariant 21).
func TestInboxListsMergeBypassAsAnAct(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC().Truncate(time.Second)
	merged := core.Task{ID: "bypassed", Workspace: "demo", Repo: "conveyor", Branch: "bypassed", State: core.TaskMerged, CreatedAt: now}
	if err := st.CreateTask(ctx, merged); err != nil {
		t.Fatal(err)
	}
	reader := store.WithActor(ctx, core.Actor{ID: store.UserActorID("usr-reader"), Role: core.ActorUser})
	bypass := core.MergeBypass{ForgeActor: "octo-merger", HeadSHA: "abcdef0123456789", RequiredHumanApprovals: 2, RecordedHumanApprovals: 1, MissingHumanApprovals: 1, RequiredAgentReviews: 1, RecordedAgentReviews: 1}
	if err := st.AppendEvent(reader, core.Event{TaskID: merged.ID, Kind: core.MergeBypassRecordedEvent, At: now, Payload: core.JSONPayload(bypass)}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "token", "demo"
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest(http.MethodGet, "/v1/next-actions?workspace_id=demo", nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Items []struct {
			Band   string `json:"band"`
			TaskID string `json:"task_id"`
		} `json:"items"`
		Acts []struct {
			TaskID    string `json:"task_id"`
			Kind      string `json:"kind"`
			Automatic bool   `json:"automatic"`
			Summary   string `json:"summary"`
		} `json:"acts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Acts) != 1 {
		t.Fatalf("acts=%+v", payload.Acts)
	}
	act := payload.Acts[0]
	if act.TaskID != merged.ID || act.Kind != core.MergeBypassRecordedEvent || act.Automatic {
		t.Fatalf("act=%+v", act)
	}
	for _, want := range []string{"octo-merger", "abcdef012345", "1 of 2 human approvals"} {
		if !strings.Contains(act.Summary, want) {
			t.Fatalf("summary %q lacks %q", act.Summary, want)
		}
	}
	for _, item := range payload.Items {
		if item.TaskID == merged.ID {
			t.Fatalf("the merged task is an inbox item: %+v", item)
		}
	}
	if count, err := st.CountOperatorAttention(ctx); err != nil || count != 0 {
		t.Fatalf("attention=%d err=%v", count, err)
	}
}
