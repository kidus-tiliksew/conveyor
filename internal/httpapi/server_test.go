package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestCreateTaskRequiresBearerToken(t *testing.T) {
	st := store.NewMemory()
	created := make(chan string, 1)
	s := NewServer(st)
	s.Repos = []string{"api"}
	s.Workspace = "test"
	s.BearerToken = "secret-token"
	s.OnCreate = func(id string) { created <- id }
	h := s.Handler()
	body := []byte(`{"title":"fix it","repo":"api"}`)

	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "wrong", header: "Bearer wrong-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
			req.Header.Set("Authorization", tc.header)
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusUnauthorized)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/tasks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret-token")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", res.Code, http.StatusCreated, res.Body.String())
	}
	select {
	case <-created:
	default:
		t.Fatal("authorized task was not dispatched")
	}

	tasks, err := st.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(tasks))
	}
}

func TestReadEndpointsDoNotRequireToken(t *testing.T) {
	s := NewServer(store.NewMemory())
	s.BearerToken = "secret-token"
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/tasks", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
}
