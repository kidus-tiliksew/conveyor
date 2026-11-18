package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// TestRenewWorkOrderReadsNoSnapshotThroughMCP renews a claimed verify order
// and a claimed review seat through MCP renew_work_order. The claim-bound
// authorization, the renewal command, and DeliverOrder all read through a
// WorkOrderReadRecorder, which must see zero snapshot bytes, no full order
// read, no task read, and no timeline read, while the renewal lists exactly
// the documents the claim response listed from the order's own pin
// (component-work-orders invariants 20, 23, and 24; req-design-delivery
// AC-1.4).
func TestRenewWorkOrderReadsNoSnapshotThroughMCP(t *testing.T) {
	t.Parallel()
	for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()
			f := newDeliveryFixture(t, "renewal-reads-"+string(stage), func(task *core.Task) {
				if stage == core.StageVerify {
					task.SetupContract.VerifyStage = true
					task.NextStage = core.StageVerify
					task.ReviewedHeadSHA = strings.Repeat("b", 40)
				}
			})
			order := f.order(t, stage)
			session := "renewal-reads-" + string(stage) + "-session"
			_, claimText := f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": session + "-token", "agent": "codex", "model": "gpt-6.1-sol", "lease_seconds": 3600}))
			var claimed struct {
				References *core.GovernanceReferences `json:"governance_references"`
			}
			if err := json.Unmarshal([]byte(claimText), &claimed); err != nil {
				t.Fatal(err)
			}
			if claimed.References == nil || len(claimed.References.Designs) != deliveryDesignCount || claimed.References.Designs[0].ID != deliveryAttachedFirst {
				t.Fatalf("%s claim did not list its pinned documents", stage)
			}
			stored, err := f.store.GetWorkOrder(f.ctx, order)
			if err != nil {
				t.Fatal(err)
			}
			if stored.GovernanceSnapshot == nil || !strings.Contains(stored.GovernanceSnapshot.Designs[0].Content, deliveryBodySentinel) {
				t.Fatalf("%s claim pinned no design bodies", stage)
			}

			recorder := storetest.NewWorkOrderReadRecorder(f.store)
			f.server.Store = recorder
			f.server.WorkOrders.Store = recorder
			_, renewText := f.call(t, "renew_work_order", deliveryArgs(order, session, map[string]any{"lease_seconds": 3600}))
			requireNoGovernanceBodies(t, "renew_work_order", renewText)
			var renewed struct {
				State      core.WorkOrderState        `json:"state"`
				References *core.GovernanceReferences `json:"governance_references"`
				Error      string                     `json:"governance_references_error"`
			}
			if err = json.Unmarshal([]byte(renewText), &renewed); err != nil {
				t.Fatal(err)
			}
			if renewed.State != core.WorkOrderClaimed || renewed.Error != "" || renewed.References == nil {
				t.Fatalf("%s renewal = state %s, list error %q", stage, renewed.State, renewed.Error)
			}
			claimJSON, _ := json.Marshal(claimed.References)
			renewJSON, _ := json.Marshal(renewed.References)
			if string(claimJSON) != string(renewJSON) {
				t.Fatalf("%s renewal lists\n%s\nwant the claim's\n%s", stage, renewJSON, claimJSON)
			}
			if recorder.SnapshotBytes() != 0 {
				t.Fatalf("%s renewal through MCP decoded %d snapshot bytes, want 0", stage, recorder.SnapshotBytes())
			}
			for _, method := range []string{"GetWorkOrder", "GetTask", "ListCompactEvents", "ListEvents", "ListTaskContextEvents", "ListWorkOrders"} {
				if n := recorder.Calls(method); n != 0 {
					t.Fatalf("%s renewal through MCP called %s %d times, want none", stage, method, n)
				}
			}
			for _, method := range []string{"GetWorkOrderWithoutSnapshots", "RenewWorkerClaimCommand", "GetWorkOrderGovernancePin", "GetTaskRepository", "ListTaskDesignContextEvents"} {
				if recorder.Calls(method) == 0 {
					t.Fatalf("%s renewal through MCP made no %s read", stage, method)
				}
			}
			after, err := f.store.GetWorkOrder(f.ctx, order)
			if err != nil {
				t.Fatal(err)
			}
			if after.GovernanceSnapshotDigest != stored.GovernanceSnapshotDigest || !strings.Contains(after.GovernanceSnapshot.Designs[0].Content, deliveryBodySentinel) {
				t.Fatalf("%s renewal changed the stored pin", stage)
			}
		})
	}
}

// failingPinStore fails only the pinned reference read.
type failingPinStore struct{ store.Store }

func (failingPinStore) GetWorkOrderGovernancePin(context.Context, string) (*core.GovernanceSnapshot, error) {
	return nil, errors.New("pin read failed")
}

// TestRenewWorkOrderReportsAFailedPinReadThroughMCP keeps the renewal's
// failure rule: the renewal already succeeded, so a failed document-list read
// answers the renewed order with governance_references_error instead of an
// error (component-work-orders invariant 24).
func TestRenewWorkOrderReportsAFailedPinReadThroughMCP(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t, "renewal-pin-failure", func(task *core.Task) {
		task.SetupContract.VerifyStage = true
		task.NextStage = core.StageVerify
		task.ReviewedHeadSHA = strings.Repeat("b", 40)
	})
	order := f.order(t, core.StageVerify)
	const session = "renewal-pin-failure-session"
	f.call(t, "claim_work_order", deliveryArgs(order, session, map[string]any{"client_token": "renewal-pin-failure-token", "agent": "codex", "model": "gpt-6.1-sol", "lease_seconds": 600}))
	f.server.WorkOrders.Store = failingPinStore{f.store}
	_, renewText := f.call(t, "renew_work_order", deliveryArgs(order, session, map[string]any{"lease_seconds": 3600}))
	var renewed struct {
		State          core.WorkOrderState        `json:"state"`
		SessionID      string                     `json:"session_id"`
		LeaseExpiresAt string                     `json:"lease_expires_at"`
		References     *core.GovernanceReferences `json:"governance_references"`
		Error          string                     `json:"governance_references_error"`
	}
	if err := json.Unmarshal([]byte(renewText), &renewed); err != nil {
		t.Fatal(err)
	}
	if renewed.State != core.WorkOrderClaimed || renewed.SessionID != session || renewed.References != nil || renewed.Error != governanceReferencesUnavailableForTest {
		t.Fatalf("renewal after a failed pin read = state %s session %q list %v error %q", renewed.State, renewed.SessionID, renewed.References != nil, renewed.Error)
	}
	stored, err := f.store.GetWorkOrder(f.ctx, order)
	if err != nil {
		t.Fatal(err)
	}
	// The claim held a ten-minute lease; the renewal asked for an hour.
	if !stored.LeaseExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("the renewal did not extend the ten-minute lease: %s", stored.LeaseExpiresAt)
	}
}

// governanceReferencesUnavailableForTest is the message DeliverOrder answers
// when the document list cannot be read.
const governanceReferencesUnavailableForTest = "the document list could not be read; get_work_order delivers it"
