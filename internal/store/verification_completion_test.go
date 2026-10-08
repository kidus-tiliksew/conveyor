package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// component-verification-service: the work-order ground reference
// is bounded and reports any omission.
func TestVerificationGroundReferenceBounds(t *testing.T) {
	limit := core.WorkOrderVerificationGroundListLimit
	g := VerificationCheckpointGround{Kind: VerificationGroundAttemptBlocked, AttemptID: "run", Explanation: "short", ServerVerified: true}
	for i := 0; i < limit; i++ {
		g.EvidenceIDs = append(g.EvidenceIDs, fmt.Sprintf("evidence-%d", i))
		g.Permissions = append(g.Permissions, verification.Permission{Kind: "network", TargetBinding: fmt.Sprintf("b%d", i)})
	}
	within := verificationGroundReference(g)
	if within.Truncated || len(within.EvidenceIDs) != limit || len(within.Permissions) != limit || within.Explanation != "short" || within.AttemptID != "run" || !within.ServerVerified {
		t.Fatalf("bounded ground = %+v", within)
	}
	for name, mutate := range map[string]func(*VerificationCheckpointGround){
		"evidence": func(g *VerificationCheckpointGround) { g.EvidenceIDs = append(g.EvidenceIDs, "extra") },
		"permissions": func(g *VerificationCheckpointGround) {
			g.Permissions = append(g.Permissions, verification.Permission{Kind: "network"})
		},
		"operations": func(g *VerificationCheckpointGround) {
			g.OperationIDs = make([]string, limit+1)
		},
		"explanation": func(g *VerificationCheckpointGround) {
			g.Explanation = strings.Repeat("é", core.WorkOrderVerificationGroundTextLimit+1)
		},
	} {
		over := g
		over.EvidenceIDs = append([]string{}, g.EvidenceIDs...)
		over.Permissions = append([]verification.Permission{}, g.Permissions...)
		mutate(&over)
		ref := verificationGroundReference(over)
		if !ref.Truncated || len(ref.EvidenceIDs) > limit || len(ref.Permissions) > limit || len(ref.OperationIDs) > limit || len([]rune(ref.Explanation)) > core.WorkOrderVerificationGroundTextLimit {
			t.Fatalf("%s: unbounded ground %+v", name, ref)
		}
	}
}
