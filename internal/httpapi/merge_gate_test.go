package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Approval and merge are separate operator acts: approve never reaches the
// merge hook, and approving an approved task names the merge command
// (req-review-gates-evidence AC-1.1; component-runtime).
func TestReviewApproveOnApprovedTaskNamesMergeCommand(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	ctx := context.Background()
	for _, task := range []core.Task{
		{ID: "approved", State: core.TaskApproved, MergeApproval: true, CreatedAt: time.Now()},
		{ID: "awaiting", State: core.TaskAwaiting, MergeApproval: true, CreatedAt: time.Now()},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(st)
	s.BearerToken = "token"
	mergeCalls := 0
	s.OnMerge = func(context.Context, core.Task) error {
		mergeCalls++
		return nil
	}
	approve := func(id string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+id+"/review", strings.NewReader(`{"action":"approve","reason_code":"approved"}`))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, request)
		return response
	}

	refused := approve("approved")
	if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), "approved tasks must use the merge operation; run `conveyor task merge approved`") {
		t.Fatalf("status=%d body=%s", refused.Code, refused.Body.String())
	}
	accepted := approve("awaiting")
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("awaiting approve status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if mergeCalls != 0 {
		t.Fatalf("approve reached the merge hook %d times", mergeCalls)
	}
}
