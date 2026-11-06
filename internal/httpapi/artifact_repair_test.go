package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func TestArtifactRepairAuthorizationAndValidation(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
	a, err := st.CreateArtifact(ctx, core.Artifact{Name: "legacy", ContentType: "application/binary"}, testimage.JPEG("repair"))
	if err != nil {
		t.Fatal(err)
	}
	members := &membershipFixture{workspaces: []core.Workspace{{ID: "demo"}, {ID: "other"}}, roles: map[string]map[string]core.WorkspaceRole{}}
	server := NewServer(st)
	server.Workspaces, server.Memberships = members, members
	credentials := staticCredentialVerifier{}
	for _, role := range []core.WorkspaceRole{core.WorkspaceRoleOperator, core.WorkspaceRoleMaintainer, core.WorkspaceRoleViewer, core.WorkspaceRoleExecutor, core.WorkspaceRoleContributor} {
		id := string(role)
		members.roles[id] = map[string]core.WorkspaceRole{"demo": role, "other": role}
		credentials[id] = core.AuthenticatedCredential{ID: id, OwnerUserID: id, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}
	}
	credentials["agent"] = core.AuthenticatedCredential{ID: "agent", OwnerUserID: "operator", Kind: retiredAgentKind, Scope: core.CredentialScopeUser}
	server.Credentials = credentials
	body := `{"expected_old_content_type":"application/binary","new_content_type":"image/jpeg","request_id":"http-repair","dry_run":true}`
	call := func(token, query, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/artifacts/"+a.ID+"/metadata-repair"+query, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"viewer", "executor", "contributor", "maintainer", "agent", "worker"} {
		w := call(token, "?workspace_id=demo", body)
		if w.Code == http.StatusOK {
			t.Fatalf("%s admitted", token)
		}
	}
	for _, test := range []struct {
		query, body string
		status      int
	}{
		{"", body, 409}, // two visible workspaces are rejected before the explicit selector check
		{"?workspace_id=demo", body, 200},
		{"?workspace_id=other", body, 404},
		{"?workspace_id=demo", `{}`, 400},
		{"?workspace_id=demo", `{"workspace":"other"}`, 400},
		{"?workspace_id=demo", body + ` {}`, 400},
		{"?workspace_id=demo", strings.Replace(body, `"image/jpeg"`, `"image/png"`, 1), 400},
		{"?workspace_id=demo", strings.Replace(body, `"application/binary"`, `"image/png"`, 1), 409},
	} {
		w := call("operator", test.query, test.body)
		if w.Code != test.status {
			t.Fatalf("%s %s: %d %s", test.query, test.body, w.Code, w.Body)
		}
	}
	// The singleton case must also require an explicit selector.
	members.workspaces = members.workspaces[:1]
	if w := call("operator", "", body); w.Code != 400 {
		t.Fatalf("singleton inference: %d %s", w.Code, w.Body)
	}
	real := strings.Replace(body, `"dry_run":true`, `"dry_run":false`, 1)
	var original store.ArtifactRepairResult
	for i := 0; i < 2; i++ {
		w := call("operator", "?workspace_id=demo", real)
		if w.Code != 200 {
			t.Fatalf("repair: %d %s", w.Code, w.Body)
		}
		var got store.ArtifactRepairResult
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			original = got
		} else if got != original {
			t.Fatalf("replay differs: %+v %+v", original, got)
		}
	}
	if w := call("operator", "?workspace_id=demo", strings.Replace(real, `"image/jpeg"`, `"image/png"`, 1)); w.Code != 409 {
		t.Fatalf("conflict: %d %s", w.Code, w.Body)
	}
}

// retryableRepairStore answers every repair the way a SingleStore replay does
// when its wait for the ledger row runs out.
type retryableRepairStore struct {
	store.Store
	err error
}

func (s retryableRepairStore) RepairArtifactMetadata(context.Context, store.ArtifactRepairRequest) (store.ArtifactRepairResult, error) {
	return store.ArtifactRepairResult{}, s.err
}

// A wait that ran out is a store failure, so it answers the generic 500 and
// carries no error text (DEC-105(1); component-artifacts, State and failure).
func TestArtifactRepairRetryableAnswersGenericInternalError(t *testing.T) {
	const detail = "recorded artifact repair is not visible"
	members := &membershipFixture{workspaces: []core.Workspace{{ID: "demo"}}, roles: map[string]map[string]core.WorkspaceRole{"operator": {"demo": core.WorkspaceRoleOperator}}}
	server := NewServer(retryableRepairStore{Store: store.NewMemory(), err: fmt.Errorf("%w: %s", store.ErrRetryable, detail)})
	server.Workspaces, server.Memberships = members, members
	server.Credentials = staticCredentialVerifier{"operator": core.AuthenticatedCredential{ID: "operator", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	const requestID = "http-replay-request-id"
	body := `{"expected_old_content_type":"image/png","new_content_type":"image/jpeg","request_id":"` + requestID + `"}`
	r := httptest.NewRequest("POST", "/v1/artifacts/some-artifact/metadata-repair?workspace_id=demo", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer operator")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "internal server error" {
		t.Fatalf("body=%q, want the generic message", got)
	}
}

func TestArtifactUploadRejectsMismatchedImage(t *testing.T) {
	st := store.NewMemory()
	s := NewServer(st)
	s.BearerToken, s.Workspace = "token", "demo"
	for _, content := range [][]byte{testimage.JPEG("jpeg"), []byte("corrupt"), make([]byte, core.MaxArtifactBytes+1)} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, artifactUploadRequest(t, "", core.ArtifactRoleTaskContext, "bad.png", "image/png", content))
		if w.Code != 400 && w.Code != 413 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
	artifacts, err := st.ListArtifacts(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("partial artifacts: %+v %v", artifacts, err)
	}
}
