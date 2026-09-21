package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

type verificationSecrets []string

func (s verificationSecrets) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s, nil
}

func (m *volatileMemory) ApplyVerification(ctx context.Context, c VerificationCommand) (VerificationReceipt, error) {
	if err := BindVerificationEvidenceAuthority(ctx, m, &c); err != nil {
		return VerificationReceipt{}, err
	}
	authority, err := LoadVerificationAuthority(ctx, m, c)
	if err != nil {
		return VerificationReceipt{}, err
	}
	secrets, err := m.ListGitHubAppKeysForRedaction(ctx)
	if err != nil {
		return VerificationReceipt{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	if c.Kind == VerificationReconcileClaimLoss {
		if err = VerifyVerificationClaimLoss(ctx, c.Access, m.tasks[c.Access.TaskID], m.workOrders[c.Access.WorkOrderID], now); err != nil {
			return VerificationReceipt{}, err
		}
	} else if c.Access.UserID == "" {
		if err = VerifyVerificationClaim(ctx, c.Access, m.tasks[c.Access.TaskID], m.workOrders[c.Access.WorkOrderID], true, now); err != nil {
			return VerificationReceipt{}, err
		}
	}
	if err = VerifyVerificationAuthority(c, authority, m.workOrders[c.Access.WorkOrderID]); err != nil {
		return VerificationReceipt{}, err
	}
	ws, _ := WorkspaceFromContext(ctx)
	rows := m.verificationRowsLocked(ws, c.Access.TaskID)
	mutation, err := PrepareVerificationMutation(ctx, verificationSecrets(secrets), c, rows, now)
	if err != nil {
		return VerificationReceipt{}, err
	}
	if len(mutation.Rows) == 0 && len(mutation.DeleteChunks) == 0 {
		return mutation.Receipt, nil
	}
	// Artifact changes are staged against a copied map and rolled back before
	// releasing the mutex on every failure. All fallible checks precede the one
	// atomic queue append; no failure point exists after that final commit step.
	original := m.artifacts
	staged := make(map[memoryArtifactKey]memoryArtifact, len(original))
	for k, v := range original {
		v.links = append([]core.Artifact(nil), v.links...)
		staged[k] = v
	}
	m.artifacts = staged
	committed := false
	defer func() {
		if !committed {
			m.artifacts = original
		}
	}()
	for _, a := range mutation.Artifacts {
		if _, err = m.createArtifactLocked(ctx, a.Artifact, a.Content); err != nil {
			return VerificationReceipt{}, err
		}
	}
	for _, ref := range mutation.ArtifactReferences {
		stored, ok := m.artifacts[memoryArtifactKey{workspace: ws, id: ref.ArtifactID}]
		if !ok {
			return VerificationReceipt{}, ErrVerificationAccess
		}
		if err = VerifyRetainedVerificationArtifact(ref, stored.content); err != nil {
			return VerificationReceipt{}, err
		}
	}
	for _, step := range []string{"artifact", "evidence", "event", "queue"} {
		if err = VerificationFault(ctx, step); err != nil {
			return VerificationReceipt{}, err
		}
	}
	if mutation.Publication != nil {
		p := mutation.Publication
		if _, err = logqueue.Enqueue(ctx, m.log, ws, "verification_publication", p.ID, p, 5, now); err != nil {
			return VerificationReceipt{}, err
		}
	}
	if m.verificationRows == nil {
		m.verificationRows = map[string]VerificationRow{}
	}
	for _, row := range mutation.Rows {
		m.verificationRows[ws+"\x00"+row.Table+"\x00"+row.ID] = row
	}
	for _, id := range mutation.DeleteChunks {
		delete(m.verificationRows, ws+"\x00verification_upload_chunks\x00"+id)
	}
	m.appendEventLocked(ctx, mutation.Event)
	committed = true
	return mutation.Receipt, nil
}
func (m *volatileMemory) verificationRowsLocked(ws, task string) []VerificationRow {
	var rows []VerificationRow
	for key, row := range m.verificationRows {
		if len(key) > len(ws) && key[:len(ws)+1] == ws+"\x00" && (row.TaskID == task || row.Table == "verification_operations") {
			row.Body = append([]byte(nil), row.Body...)
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Table != rows[j].Table {
			return rows[i].Table < rows[j].Table
		}
		return rows[i].ID < rows[j].ID
	})
	return rows
}
func (m *volatileMemory) ReadVerification(ctx context.Context, a VerificationAccess, id string) (VerificationSnapshot, error) {
	if err := AuthorizeVerificationUserRead(ctx, m, a); err != nil {
		return VerificationSnapshot{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return VerificationSnapshot{}, ErrVerificationAccess
	}
	task := m.tasks[a.TaskID]
	if task.Workspace != ws {
		return VerificationSnapshot{}, ErrVerificationAccess
	}
	if a.UserID == "" {
		if err := VerifyVerificationClaim(ctx, a, task, m.workOrders[a.WorkOrderID], false, time.Now().UTC()); err != nil {
			return VerificationSnapshot{}, err
		}
	}
	rows := m.verificationRowsLocked(ws, a.TaskID)
	return VerificationReadSnapshot(rows, a, id, m.workOrders[a.WorkOrderID].Stage)
}
func (m *volatileMemory) ReadVerificationArtifact(ctx context.Context, a VerificationAccess, evidenceID, artifactID string) (core.Artifact, []byte, error) {
	if err := AuthorizeVerificationUserRead(ctx, m, a); err != nil {
		return core.Artifact{}, nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, _ := WorkspaceFromContext(ctx)
	task := m.tasks[a.TaskID]
	if task.Workspace != ws {
		return core.Artifact{}, nil, ErrVerificationAccess
	}
	if a.UserID == "" {
		if err := VerifyVerificationClaim(ctx, a, task, m.workOrders[a.WorkOrderID], false, time.Now().UTC()); err != nil {
			return core.Artifact{}, nil, err
		}
	}
	rows := m.verificationRowsLocked(ws, a.TaskID)
	if err := VerificationArtifactAccess(rows, a, evidenceID, artifactID, m.workOrders[a.WorkOrderID].Stage); err != nil {
		return core.Artifact{}, nil, err
	}
	artifact, ok := m.artifacts[memoryArtifactKey{workspace: ws, id: artifactID}]
	if !ok {
		return core.Artifact{}, nil, ErrVerificationAccess
	}
	if verificationHash(artifact.content) != artifactID {
		return core.Artifact{}, nil, fmt.Errorf("verification artifact integrity failure")
	}
	meta := artifact.meta
	meta.ContentType = VerificationArtifactMedia(rows, evidenceID, artifactID)
	meta.Role = core.ArtifactRoleTypedVerificationEvidence
	meta.TaskID = a.TaskID
	return meta, append([]byte(nil), artifact.content...), nil
}

func AuthorizeVerificationUserRead(ctx context.Context, b MembershipStore, a VerificationAccess) error {
	if a.UserID == "" {
		return nil
	}
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || ActorFromContext(ctx).ID != UserActorID(a.UserID) {
		return ErrVerificationAccess
	}
	allowed, err := b.AuthorizeWorkspace(ctx, a.UserID, ws, core.CapabilityViewWorkspace)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrVerificationAccess
	}
	return nil
}
func VerificationReadSnapshot(rows []VerificationRow, a VerificationAccess, id string, stage core.Stage) (VerificationSnapshot, error) {
	if strings.HasPrefix(id, "request:") && a.UserID == "" && stage == core.StageVerify {
		for _, r := range rows {
			if r.Table == "verification_contexts" && r.TaskID == a.TaskID && r.LogicalKey == a.WorkOrderID+":"+strings.TrimPrefix(id, "request:") {
				id = r.ID
				break
			}
		}
	}
	row, ok := verificationFind(rows, "verification_contexts", id)
	if !ok || row.TaskID != a.TaskID {
		return VerificationSnapshot{}, ErrVerificationAccess
	}
	vc := verificationDecode[VerificationContext](row)
	if a.UserID == "" {
		if stage == core.StageReview {
			if vc.SealedAt == nil {
				return VerificationSnapshot{}, ErrVerificationAccess
			}
		} else if vc.WorkOrderID != a.WorkOrderID {
			return VerificationSnapshot{}, ErrVerificationAccess
		}
	}
	return VerificationSnapshotFromRows(rows, id)
}
func VerificationArtifactAccess(rows []VerificationRow, a VerificationAccess, evidenceID, artifactID string, stage core.Stage) error {
	row, ok := verificationFind(rows, "verification_evidence", evidenceID)
	if !ok || row.State != "evidence" || row.TaskID != a.TaskID {
		return ErrVerificationAccess
	}
	if _, err := VerificationReadSnapshot(rows, a, row.ContextID, stage); err != nil {
		return err
	}
	for _, ref := range verificationDecode[VerificationEvidenceRecord](row).Envelope.Artifacts {
		if ref.ArtifactID == artifactID && ref.SHA256 == artifactID {
			return nil
		}
	}
	return ErrVerificationAccess
}

func (m *volatileMemory) ReconcileVerificationClaims(ctx context.Context) (int, error) {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return 0, ErrVerificationAccess
	}
	m.mu.RLock()
	var rows []VerificationRow
	for _, task := range m.tasks {
		if task.Workspace == ws {
			rows = append(rows, m.verificationRowsLocked(ws, task.ID)...)
		}
	}
	commands, err := VerificationReconciliationCommands(ctx, rows)
	m.mu.RUnlock()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, c := range commands {
		if _, err = m.ApplyVerification(ctx, c); err != nil {
			if errors.Is(err, ErrVerificationState) {
				continue
			}
			return count, err
		}
		count++
	}
	return count, nil
}
