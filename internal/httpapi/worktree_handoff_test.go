package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestWorktreeHandoffHTTPExactSessionAndRelease(t *testing.T) {
	server, st, handler := taskRunHTTPFixture(t)
	order := createTaskRunOrder(t, st, "handoff-http")
	binding := core.AuthenticatedCredential{ID: "child", OwnerUserID: "local-operator", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser, RunWorkspaceID: "demo", RunWorkOrderID: order.ID, RunSessionID: "session"}
	foreign := binding
	foreign.OwnerUserID = "other-user"
	wrongOrder := binding
	wrongOrder.RunWorkOrderID = "other-order"
	wrongWorkspace := binding
	wrongWorkspace.RunWorkspaceID = "other-workspace"
	server.Credentials = staticCredentialVerifier{"user-token": {ID: "user", OwnerUserID: "local-operator", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}, "child": binding, "foreign": foreign, "wrong-order": wrongOrder, "wrong-workspace": wrongWorkspace}
	path := "/v1/tasks/" + order.TaskID + "/run-orders/" + order.ID
	claim := taskRunHTTPCall(handler, http.MethodPost, path+"/claim", `{"session_id":"session","client_token":"secret","agent":"codex","model":"fixture"}`)
	if claim.Code != 200 {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body.String())
	}
	request := core.WorktreeHandoffRequest{SessionID: "session", Generation: "writer-one", Action: "admit"}
	call := func(token string, r core.WorktreeHandoffRequest) int {
		data, _ := json.Marshal(r)
		response := taskRunHTTPCallAs(handler, token, http.MethodPost, path+"/worktree-handoff", string(data))
		return response.Code
	}
	for _, token := range []string{"foreign", "wrong-order", "wrong-workspace"} {
		if code := call(token, request); code < 400 {
			t.Fatalf("%s admitted: %d", token, code)
		}
	}
	wrong := request
	wrong.SessionID = "other"
	if code := call("child", wrong); code < 400 {
		t.Fatal("foreign session admitted")
	}
	if code := call("child", request); code != 200 {
		t.Fatalf("bound child admission=%d", code)
	}
	if code := call("child", request); code != 200 {
		t.Fatalf("idempotent admission=%d", code)
	}
	request.Action = "ready"
	if code := call("child", request); code != 200 {
		t.Fatalf("ready=%d", code)
	}
	release := taskRunHTTPCall(handler, http.MethodPost, path+"/release", `{"session_id":"session","reason":"operator checkpoint reached","checkpoint":{"decision_request":"fixture decision"},"outcome":"released"}`)
	if release.Code != 200 {
		t.Fatalf("release: %d %s", release.Code, release.Body.String())
	}
	request.Action = "preserve"
	if code := call("child", request); code != 200 {
		t.Fatalf("exact released child preservation=%d", code)
	}
	request.Action = "ready"
	if code := call("child", request); code < 400 {
		t.Fatal("released child regained implementation authority")
	}
	wrong = request
	wrong.Action = "preserve"
	wrong.Generation = "another-writer"
	if code := call("child", wrong); code < 400 {
		t.Fatal("different writer generation authorized")
	}
}
