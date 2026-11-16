package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestBoardRESTAndMCP(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	token := f.user("usr_board", core.WorkspaceRoleContributor)
	for i := range 3 {
		if err := f.store.CreateTask(f.ctx, core.Task{ID: fmt.Sprintf("board-%d", i), Workspace: governanceWorkspace, Title: "Board task", State: core.TaskRunning, CreatedAt: time.Now().Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path, tag, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if tag != "" {
			req.Header.Set("If-None-Match", tag)
		}
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, req)
		return w
	}
	path := "/v1/board?workspace_id=" + governanceWorkspace + "&limit=2&offset=0"
	first := get(path, "", token)
	if first.Code != 200 {
		t.Fatalf("board: %d %s", first.Code, first.Body.String())
	}
	var page boardResponse
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || len(page.Items) != 2 || page.Limit != 2 || first.Header().Get("X-Conveyor-Total") != "3" {
		t.Fatalf("page: %+v headers %v", page, first.Header())
	}
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("missing ETag")
	}
	cached := get(path, tag, token)
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatalf("conditional: %d %s", cached.Code, cached.Body.String())
	}
	capped := get("/v1/board?workspace_id="+governanceWorkspace+"&limit=200", "", token)
	if capped.Code != 200 || capped.Header().Get("X-Conveyor-Limit") != "100" {
		t.Fatalf("capped page: %d headers %v", capped.Code, capped.Header())
	}
	if err := f.store.CreateTask(f.ctx, core.Task{ID: "board-new", Workspace: governanceWorkspace, Title: "New task", State: core.TaskRunning, CreatedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	changed := get(path, tag, token)
	if changed.Code != 200 || changed.Header().Get("ETag") == tag {
		t.Fatalf("changed board: %d tag %s", changed.Code, changed.Header().Get("ETag"))
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=1&offset=2147483648", "offset=1"} {
		if got := get("/v1/board?workspace_id="+governanceWorkspace+"&"+query, "", token); got.Code != 400 {
			t.Fatalf("%s: %d", query, got.Code)
		}
	}
	if got := get(path, "", ""); got.Code == 200 {
		t.Fatal("anonymous board accepted")
	}
	text, isErr := f.call(token, "get_board", map[string]any{"workspace_id": governanceWorkspace, "limit": 2, "offset": 2})
	if isErr {
		t.Fatal(text)
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 || len(page.Items) != 2 || page.Offset != 2 {
		t.Fatalf("MCP page: %s", text)
	}
	restPage := get("/v1/board?workspace_id="+governanceWorkspace+"&limit=2&offset=2", "", token)
	var restResult boardResponse
	if err := json.Unmarshal(restPage.Body.Bytes(), &restResult); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page, restResult) {
		t.Fatalf("MCP page differs from REST: %+v %+v", page, restResult)
	}
	_, isErr = f.call(token, "get_board", map[string]any{"workspace_id": governanceWorkspace, "snapshot": "0123456789abcdef0123456789abcdef"})
	if !isErr {
		t.Fatal("snapshot accepted")
	}
	_, isErr = f.call(token, "get_board", map[string]any{"workspace_id": governanceWorkspace, "offset": 2147483648})
	if !isErr {
		t.Fatal("out-of-range offset accepted")
	}
}

func TestBoardShortPageContinuation(t *testing.T) {
	f := newGovernanceAuthFixture(t)
	token := f.user("usr_board_large", core.WorkspaceRoleContributor)
	for i := range 110 {
		id := fmt.Sprintf("%03d-%s", i, strings.Repeat("x", 220))
		if err := f.store.CreateTask(f.ctx, core.Task{ID: id, Workspace: governanceWorkspace, Title: strings.Repeat("t", 80), State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	offset := 0
	short := false
	for offset < 110 {
		path := fmt.Sprintf("/v1/board?workspace_id=%s&limit=100&offset=%d", governanceWorkspace, offset)
		body := f.rest(token, path)
		if len(body) >= 32768 {
			t.Fatalf("page exceeds budget: %d", len(body))
		}
		var page boardResponse
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) == 0 {
			t.Fatalf("empty page at %d", offset)
		}
		if offset == 0 && len(page.Items) < page.Limit {
			short = true
		}
		offset += len(page.Items)
	}
	if !short || offset != 110 {
		t.Fatalf("short=%t count=%d", short, offset)
	}
}
