package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
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

func TestMCPInitializeAndVersionEndpointReportSameRelease(t *testing.T) {
	server := NewServer(store.NewMemory())
	versionRequest := httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	versionResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(versionResponse, versionRequest)

	initializeRequest := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	initializeResponse := httptest.NewRecorder()
	server.handleMCP(initializeResponse, initializeRequest)

	var versionBody struct {
		Version string `json:"version"`
	}
	var initializeBody struct {
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(versionResponse.Body.Bytes(), &versionBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(initializeResponse.Body.Bytes(), &initializeBody); err != nil {
		t.Fatal(err)
	}
	if initializeResponse.Code != http.StatusOK || versionResponse.Code != http.StatusOK ||
		initializeBody.Result.ServerInfo.Version != versionBody.Version || versionBody.Version != releaseinfo.Version {
		t.Fatalf("initialize status=%d version=%q; HTTP status=%d version=%q; release=%q",
			initializeResponse.Code, initializeBody.Result.ServerInfo.Version,
			versionResponse.Code, versionBody.Version, releaseinfo.Version)
	}
}
