package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ClaimedVerificationEvidenceRequest carries only claim identity and file
// metadata. Task, workspace, and artifact role are always derived by the
// store from the matching live implement order.
type ClaimedVerificationEvidenceRequest struct {
	WorkOrderID string
	WorkerID    string
	SessionID   string
	ClientToken string
	Name        string
	ContentType string
}

type memoryArtifact struct {
	meta    core.Artifact
	content []byte
	links   []core.Artifact
}

type memoryArtifactKey struct {
	workspace string
	id        string
}

func (m *memory) CreateArtifact(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	if artifact.Role == core.ArtifactRoleTypedVerificationEvidence {
		return core.Artifact{}, ErrVerificationAccess
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createArtifactLocked(ctx, artifact, content)
}

func (m *memory) createArtifactLocked(ctx context.Context, artifact core.Artifact, content []byte) (core.Artifact, error) {
	media, err := core.ValidateArtifactMedia(artifact.ContentType, content)
	if err != nil {
		return core.Artifact{}, err
	}
	artifact.ContentType = media
	workspace := workspaceOrDefault(ctx, artifact.Workspace)
	if artifact.Workspace != "" && artifact.Workspace != workspace {
		return core.Artifact{}, fmt.Errorf("artifact workspace mismatch")
	}
	artifact.Workspace = workspace
	if artifact.Role == "" {
		artifact.Role = core.ArtifactRoleTaskContext
	}
	if !artifact.Role.Valid() {
		return core.Artifact{}, fmt.Errorf("invalid artifact role %q", artifact.Role)
	}
	artifact.ID = fmt.Sprintf("%x", sha256.Sum256(content))
	artifact.SizeBytes = int64(len(content))
	if err := artifact.ValidateAttachmentTarget(); err != nil {
		return core.Artifact{}, err
	}
	if artifact.RequirementID != "" {
		if _, ok := m.requirements[memoryScopedKey{workspace: artifact.Workspace, id: artifact.RequirementID}]; !ok {
			return core.Artifact{}, fmt.Errorf("artifact attachment does not belong to workspace %s", artifact.Workspace)
		}
	}
	if artifact.PlanningSessionID != "" {
		if _, ok := m.planningSessions[memoryScopedKey{workspace: artifact.Workspace, id: artifact.PlanningSessionID}]; !ok {
			return core.Artifact{}, fmt.Errorf("artifact attachment does not belong to workspace %s", artifact.Workspace)
		}
	}
	if artifact.Role == core.ArtifactRoleTypedVerificationEvidence {
		if artifact.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("typed evidence requires a task")
		}
		if _, err := core.ValidateTypedVerificationArtifact(artifact.ContentType, content); err != nil {
			return core.Artifact{}, err
		}
	}
	if artifact.Role == core.ArtifactRoleVerificationEvidence {
		if artifact.TaskID == "" {
			return core.Artifact{}, fmt.Errorf("verification evidence must be attached directly to one task")
		}
		task, ok := m.tasks[artifact.TaskID]
		if !ok || task.Workspace != artifact.Workspace {
			return core.Artifact{}, fmt.Errorf("verification evidence task does not belong to workspace %s", artifact.Workspace)
		}
		normalized, err := core.ValidateVerificationEvidenceArtifact(artifact.ContentType, content)
		if err != nil {
			return core.Artifact{}, err
		}
		artifact.ContentType = normalized
	}
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	key := memoryArtifactKey{workspace: artifact.Workspace, id: artifact.ID}
	if existing, ok := m.artifacts[key]; ok {
		artifact.Name, artifact.ContentType, artifact.SizeBytes, artifact.CreatedAt = existing.meta.Name, existing.meta.ContentType, existing.meta.SizeBytes, existing.meta.CreatedAt
		for _, link := range existing.links {
			if link.Workspace == artifact.Workspace && link.TaskID == artifact.TaskID && link.RequirementID == artifact.RequirementID && link.PlanningSessionID == artifact.PlanningSessionID && link.Role == artifact.Role {
				return link, nil
			}
		}
		existing.links = append(existing.links, artifact)
		m.artifacts[key] = existing
		return artifact, nil
	}
	m.artifacts[key] = memoryArtifact{meta: artifact, content: append([]byte(nil), content...), links: []core.Artifact{artifact}}
	return artifact, nil
}

func (m *memory) CreateClaimedVerificationEvidence(ctx context.Context, request ClaimedVerificationEvidenceRequest, content []byte) (core.Artifact, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.Artifact{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspace := workspaceOrDefault(ctx, "")
	order, ok := m.workOrders[request.WorkOrderID]
	if !ok {
		return core.Artifact{}, ErrVerificationEvidenceClaimConflict
	}
	now := time.Now().UTC()
	order = m.refreshWorkOrderLocked(ctx, order, now)
	if order.State != core.WorkOrderClaimed || order.Stage != core.StageImplement ||
		order.WorkerID == "" || order.WorkerID != request.WorkerID ||
		request.SessionID == "" || order.SessionID != request.SessionID ||
		request.ClientToken == "" || order.ClientTokenHash != tokenHash(request.ClientToken) ||
		!order.LeaseExpiresAt.After(now) ||
		(!order.ExecutionDeadline.IsZero() && !order.ExecutionDeadline.After(now)) {
		return core.Artifact{}, ErrVerificationEvidenceClaimConflict
	}
	task, ok := m.tasks[order.TaskID]
	if !ok || task.Workspace != workspace {
		return core.Artifact{}, ErrVerificationEvidenceClaimConflict
	}
	return m.createArtifactLocked(ctx, core.Artifact{
		Workspace: workspace, Name: request.Name, ContentType: request.ContentType,
		Role: core.ArtifactRoleVerificationEvidence, TaskID: order.TaskID, CreatedAt: now,
	}, content)
}

func (m *memory) artifactForRead(ctx context.Context, id string) (memoryArtifact, bool) {
	if workspace, scoped := WorkspaceFromContext(ctx); scoped {
		artifact, ok := m.artifacts[memoryArtifactKey{workspace: workspace, id: id}]
		return artifact, ok
	}
	var match memoryArtifact
	found := false
	for key, artifact := range m.artifacts {
		if key.id != id {
			continue
		}
		if found {
			return memoryArtifact{}, false
		}
		match = artifact
		found = true
	}
	return match, found
}

func (m *memory) GetArtifact(ctx context.Context, id string) (core.Artifact, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifact, ok := m.artifactForRead(ctx, id)
	if !ok {
		return core.Artifact{}, nil, fmt.Errorf("%w: artifact %s", ErrNotFound, id)
	}
	// Content-addressed bytes may have several roles. Any typed evidence link
	// requires the provenance-scoped reader, regardless of insertion order.
	if artifact.meta.Role == core.ArtifactRoleTypedVerificationEvidence {
		return core.Artifact{}, nil, ErrVerificationAccess
	}
	for _, link := range artifact.links {
		if link.Role == core.ArtifactRoleTypedVerificationEvidence {
			return core.Artifact{}, nil, ErrVerificationAccess
		}
	}
	return artifact.meta, append([]byte(nil), artifact.content...), nil
}

func (m *memory) GetArtifactForPlanningSession(ctx context.Context, id, sessionID string) (core.Artifact, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	artifact, ok := m.artifactForRead(ctx, id)
	if !ok {
		return core.Artifact{}, nil, fmt.Errorf("artifact %s not found", id)
	}
	for _, link := range artifact.links {
		if sessionID != "" && link.PlanningSessionID == sessionID {
			return link, append([]byte(nil), artifact.content...), nil
		}
	}
	return core.Artifact{}, nil, fmt.Errorf("artifact %s not found", id)
}

func (m *memory) ListArtifacts(ctx context.Context) ([]core.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []core.Artifact
	workspace, scoped := WorkspaceFromContext(ctx)
	for key, artifact := range m.artifacts {
		if scoped && workspace != "" && key.workspace != workspace {
			continue
		}
		out = append(out, artifact.links...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *memory) ListArtifactsForLineage(ctx context.Context, nodes []core.LineageNode) ([]core.Artifact, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	wanted := make(map[core.LineageNode]bool, len(nodes))
	rank := make(map[core.LineageNode]int, len(nodes))
	for index, node := range nodes {
		if !node.Valid() {
			continue
		}
		wanted[node] = true
		if _, exists := rank[node]; !exists {
			rank[node] = index
		}
	}
	workspace, scoped := WorkspaceFromContext(ctx)
	var out []core.Artifact
	artifactRanks := map[string]int{}
	for key, artifact := range m.artifacts {
		if scoped && workspace != "" && key.workspace != workspace {
			continue
		}
		for _, link := range artifact.links {
			matchedRank := len(nodes)
			matched := false
			for _, node := range []core.LineageNode{{Type: core.LineageTask, ID: link.TaskID}, {Type: core.LineageRequirement, ID: link.RequirementID}, {Type: core.LineagePlanningSession, ID: link.PlanningSessionID}, {Type: core.LineageEvidence, ID: link.ID}} {
				if node.Type == core.LineageEvidence && !link.EligibleVerificationEvidence() {
					continue
				}
				if wanted[node] && rank[node] < matchedRank {
					matched, matchedRank = true, rank[node]
				}
			}
			if matched {
				out = append(out, link)
				key := artifactLineageLinkKey(link)
				if prior, exists := artifactRanks[key]; !exists || matchedRank < prior {
					artifactRanks[key] = matchedRank
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		leftRank := artifactRanks[artifactLineageLinkKey(out[i])]
		rightRank := artifactRanks[artifactLineageLinkKey(out[j])]
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return artifactLineageLinkKey(out[i]) < artifactLineageLinkKey(out[j])
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func artifactLineageLinkKey(artifact core.Artifact) string {
	return strings.Join([]string{artifact.ID, string(artifact.Role), artifact.TaskID, artifact.RequirementID, artifact.PlanningSessionID}, "\x00")
}
