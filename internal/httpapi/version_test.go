package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestVersionEndpointReportsReleaseWithoutAuthentication(t *testing.T) {
	server := NewServer(store.NewMemory())
	server.Release = "v9.2.1"
	request := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"version":"v9.2.1"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
