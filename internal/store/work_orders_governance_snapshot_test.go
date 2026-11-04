package store

import (
	"encoding/json"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestEncodeGovernanceSnapshotStableAndEmpty(t *testing.T) {
	if digest, content := EncodeGovernanceSnapshot(nil); digest != "" || content != nil {
		t.Fatalf("nil snapshot digest=%q content=%q", digest, content)
	}
	snapshot := &core.GovernanceSnapshot{
		Designs: []core.GovernanceDesignContext{{ID: "component-persistence", Title: "Persistence", Category: "component", Version: 1, Content: "body"}},
	}
	digest, content := EncodeGovernanceSnapshot(snapshot)
	if digest == "" || len(digest) != 64 {
		t.Fatalf("digest=%q", digest)
	}
	again, againContent := EncodeGovernanceSnapshot(snapshot)
	if digest != again || string(content) != string(againContent) {
		t.Fatal("identical snapshots must share one digest")
	}
	var decoded core.GovernanceSnapshot
	if err := json.Unmarshal(content, &decoded); err != nil || len(decoded.Designs) != 1 || decoded.Designs[0].Content != "body" {
		t.Fatalf("content=%s err=%v", content, err)
	}
}

func TestPinClaimGovernanceFreezesAuthority(t *testing.T) {
	order := core.WorkOrder{GovernanceSnapshot: &core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "pinned", Version: 1, Content: "frozen"}},
		PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "old"}},
		ResolutionNotes:        []string{"old"},
	}}
	PinClaimGovernance(&order, &core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "live", Version: 2, Content: "new"}},
		PendingDesignProposals: []core.PendingSystemDesignProposal{{DocumentID: "fresh"}},
		ResolutionNotes:        []string{"fresh"},
	}, false)
	// A frozen pin stays exactly as recorded, including an older pin's retired
	// evidence, so its digest never moves (DEC-102(6)).
	if order.GovernanceSnapshot.Designs[0].ID != "pinned" || order.GovernanceSnapshot.PendingDesignProposals[0].DocumentID != "old" || order.GovernanceSnapshot.ResolutionNotes[0] != "old" {
		t.Fatalf("frozen pin changed=%+v", order.GovernanceSnapshot)
	}
	PinClaimGovernance(&order, &core.GovernanceSnapshot{
		Designs: []core.GovernanceDesignContext{{ID: "live", Version: 2, Content: "new"}},
	}, true)
	if order.GovernanceSnapshot.Designs[0].ID != "live" {
		t.Fatalf("repin=%+v", order.GovernanceSnapshot)
	}
}

func TestWorkOrderEventPayloadReferencesDigestOnly(t *testing.T) {
	order := core.WorkOrder{ID: "wo-1", GovernanceSnapshot: &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "component-persistence", Content: "secret-body"}}}}
	digest, _ := EncodeGovernanceSnapshot(order.GovernanceSnapshot)
	payload := WorkOrderEventPayload(order)
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["governance_snapshot"]; ok {
		t.Fatalf("event embedded snapshot: %s", payload)
	}
	if _, ok := decoded["served_requirement_snapshot"]; ok {
		t.Fatalf("event embedded requirements: %s", payload)
	}
	if got, _ := decoded["governance_snapshot_digest"].(string); got != digest {
		t.Fatalf("digest=%q want %q payload=%s", got, digest, payload)
	}
}
