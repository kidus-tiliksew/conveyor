package postgres

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestDocumentDetailMeasurementIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	defer st.Close()
	req, design, _ := storetest.SeedDocumentEventMeasurement(t, st, ctx)
	server := httpapi.NewServer(st)
	server.Credentials = nil
	server.Workspace = workspace
	server.BearerToken = "token"
	handler := server.Handler()
	for _, path := range []string{"/v1/requirements/" + req, "/v1/system-designs/" + design} {
		request := httptest.NewRequest("GET", path+"?workspace_id="+workspace, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(response, request)
		elapsed := time.Since(start)
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		t.Logf("MEASUREMENT postgres %s bytes=%d server_time=%s", path, response.Body.Len(), elapsed)
	}
}
