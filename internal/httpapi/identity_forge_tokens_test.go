package httpapi

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetiredForgeTokenRoutesReturnNotFound(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Credentials = staticCredentialVerifier{"human": {ID: "pat", OwnerUserID: "usr", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	for _, path := range []string{"/v1/forge-token", "/v1/workspaces/demo/forge-token"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Authorization", "Bearer human")
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusNotFound {
				t.Fatalf("%s %s status=%d", method, path, w.Code)
			}
		}
	}
}
func TestClaimabilityWithoutStoredForgeTokens(t *testing.T) {
	ctx := store.WithCredential(t.Context(), core.AuthenticatedCredential{ID: "agent", OwnerUserID: "usr", Kind: core.CredentialAgent})
	orders := projectAssigneeClaimability(ctx, []core.WorkOrder{{ID: "queued", State: core.WorkOrderQueued, Claimable: true}})
	if !orders[0].Claimable || orders[0].ClaimRefusalReason != "" {
		t.Fatalf("projection=%+v", orders[0])
	}
}
