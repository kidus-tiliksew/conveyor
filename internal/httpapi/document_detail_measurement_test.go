package httpapi

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// Reproducible handler measurement: two serving tasks with 600 2-KiB events.
func TestDocumentDetailMeasurement(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	req, design, _ := storetest.SeedDocumentEventMeasurement(t, st, ctx)
	server := NewServer(st)
	server.Workspace = "demo"
	handler := server.Handler()
	for _, path := range []string{"/v1/requirements/" + req, "/v1/system-designs/" + design} {
		response := httptest.NewRecorder()
		start := time.Now()
		handler.ServeHTTP(response, authenticatedMemoryRead(server, httptest.NewRequest("GET", path+"?workspace_id=demo", nil)))
		elapsed := time.Since(start)
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		t.Logf("MEASUREMENT %s bytes=%d server_time=%s", path, response.Body.Len(), elapsed)
	}
}
