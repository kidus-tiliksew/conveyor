package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

var ErrArtifactRepairConflict = errors.New("artifact metadata repair conflict")
var ErrArtifactRepairInvalid = errors.New("invalid artifact metadata repair")

type ArtifactRepairRequest struct {
	ArtifactID             string `json:"-"`
	ExpectedOldContentType string `json:"expected_old_content_type"`
	NewContentType         string `json:"new_content_type"`
	RequestID              string `json:"request_id"`
	DryRun                 bool   `json:"dry_run"`
}
type ArtifactRepairResult struct {
	ArtifactID          string `json:"artifact_id"`
	CurrentContentType  string `json:"current_content_type"`
	DetectedContentType string `json:"detected_content_type"`
	ProposedContentType string `json:"proposed_content_type"`
	WouldChange         bool   `json:"would_change"`
	ValidationStatus    string `json:"validation_status"`
	RequestID           string `json:"request_id"`
}
type ArtifactRepairReceipt struct {
	Request ArtifactRepairRequest `json:"request"`
	Result  ArtifactRepairResult  `json:"result"`
}

// NormalizeArtifactRepair requires trusted workspace/actor context; the HTTP
// capability boundary is ART-HTTP-3 (req-security-boundaries REQ-1).
func NormalizeArtifactRepair(ctx context.Context, r ArtifactRepairRequest) (ArtifactRepairRequest, error) {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || strings.TrimSpace(ws) == "" || ActorFromContext(ctx).Role != core.ActorUser {
		return r, fmt.Errorf("%w: explicit workspace and user actor required", ErrArtifactRepairInvalid)
	}
	r.ExpectedOldContentType = strings.TrimSpace(r.ExpectedOldContentType)
	r.RequestID = strings.TrimSpace(r.RequestID)
	if len(r.RequestID) > 255 {
		return r, fmt.Errorf("%w: request_id must be at most 255 bytes", ErrArtifactRepairInvalid)
	}
	if r.ArtifactID == "" || r.ExpectedOldContentType == "" || r.RequestID == "" || !core.SupportedArtifactImage(r.NewContentType) {
		return r, fmt.Errorf("%w: artifact, expected old type, canonical supported image type and request_id required", ErrArtifactRepairInvalid)
	}
	return r, nil
}
func SameArtifactRepair(a, b ArtifactRepairRequest) bool {
	return a.ArtifactID == b.ArtifactID && a.RequestID == b.RequestID && a.ExpectedOldContentType == b.ExpectedOldContentType && a.NewContentType == b.NewContentType
}
func ValidateArtifactRepair(r ArtifactRepairRequest, a core.Artifact, content []byte) (ArtifactRepairResult, error) {
	result := ArtifactRepairResult{}
	if fmt.Sprintf("%x", sha256.Sum256(content)) != a.ID || int64(len(content)) != a.SizeBytes {
		return result, fmt.Errorf("%w: stored hash or size does not match bytes", ErrArtifactRepairInvalid)
	}
	detected, err := core.ValidateArtifactMedia("", content)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrArtifactRepairInvalid, err)
	}
	if !core.SupportedArtifactImage(detected) || r.NewContentType != detected {
		return result, fmt.Errorf("%w: proposed type %q must equal detected supported image type %q", ErrArtifactRepairInvalid, r.NewContentType, detected)
	}
	if a.ContentType != r.ExpectedOldContentType {
		return result, fmt.Errorf("%w: expected old type %q, current type %q", ErrArtifactRepairConflict, r.ExpectedOldContentType, a.ContentType)
	}
	return ArtifactRepairResult{ArtifactID: a.ID, CurrentContentType: a.ContentType, DetectedContentType: detected, ProposedContentType: r.NewContentType, WouldChange: a.ContentType != r.NewContentType, ValidationStatus: "valid", RequestID: r.RequestID}, nil
}
func ArtifactRepairEvent(ctx context.Context, r ArtifactRepairRequest, a core.Artifact, result ArtifactRepairResult) core.Event {
	ws, _ := WorkspaceFromContext(ctx)
	actor := ActorFromContext(ctx)
	return core.Event{Kind: "artifact.metadata_repaired", ActorID: actor.ID, ActorRole: actor.Role, Payload: core.JSONPayload(map[string]any{"artifact_id": a.ID, "actor": actor.ID, "workspace": ws, "request_id": r.RequestID, "hash": a.ID, "old_content_type": a.ContentType, "detected_content_type": result.DetectedContentType, "new_content_type": r.NewContentType, "size_bytes": a.SizeBytes})}
}

func (m *memory) RepairArtifactMetadata(ctx context.Context, r ArtifactRepairRequest) (ArtifactRepairResult, error) {
	r, err := NormalizeArtifactRepair(ctx, r)
	if err != nil {
		return ArtifactRepairResult{}, err
	}
	ws, _ := WorkspaceFromContext(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memoryScopedKey{workspace: ws, id: r.RequestID}
	if receipt, ok := m.artifactRepairs[key]; ok {
		if !SameArtifactRepair(receipt.Request, r) {
			return ArtifactRepairResult{}, ErrArtifactRepairConflict
		}
		if !r.DryRun {
			return receipt.Result, nil
		}
	}
	a, ok := m.artifacts[memoryArtifactKey{workspace: ws, id: r.ArtifactID}]
	if !ok {
		return ArtifactRepairResult{}, ErrNotFound
	}
	result, err := ValidateArtifactRepair(r, a.meta, a.content)
	if err != nil || r.DryRun {
		return result, err
	}
	if strings.ContainsRune(ActorFromContext(ctx).ID, 0) {
		return ArtifactRepairResult{}, fmt.Errorf("audit actor contains NUL")
	}
	if result.WouldChange {
		event := ArtifactRepairEvent(ctx, r, a.meta, result)
		a.meta.ContentType = r.NewContentType
		for i := range a.links {
			a.links[i].ContentType = r.NewContentType
		}
		m.artifacts[memoryArtifactKey{workspace: ws, id: r.ArtifactID}] = a
		m.appendEventLocked(ctx, event)
	}
	if m.artifactRepairs == nil {
		m.artifactRepairs = map[memoryScopedKey]ArtifactRepairReceipt{}
	}
	m.artifactRepairs[key] = ArtifactRepairReceipt{Request: r, Result: result}
	return result, nil
}
