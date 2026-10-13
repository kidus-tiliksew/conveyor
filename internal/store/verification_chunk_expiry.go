package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Chunk expiry runs from the daemon's reconcile tick under the internal
// verification-reconciler actor, never from a route, MCP tool, or worker
// (req-verification-evidence REQ-2/AC-2.1, AC-2.4; req-security-boundaries
// REQ-1, REQ-5; component-verification-evidence).
const (
	// VerificationChunkExpiryLimit bounds the staging rows one workspace run
	// selects. At the 512 KiB chunk cap it bounds the staged bytes considered
	// for deletion to 50 MiB.
	VerificationChunkExpiryLimit = 100
	// VerificationChunkExpiryBudget bounds one workspace run of the tick.
	VerificationChunkExpiryBudget = 5 * time.Second
	verificationReconcilerActor   = "verification-reconciler"
)

// VerificationChunkCandidate is the scalar metadata of one staging row that the
// bounded candidate read returns. It never carries chunk content.
type VerificationChunkCandidate struct {
	ID, TaskID, UploadID string
	ExpiresAt            time.Time
}

// AuthorizeVerificationChunkExpiry admits only the internal reconciler in an
// explicit workspace and fixes the cutoff. The verification test clock, when
// present, supplies the cutoff so fixtures can expire staged uploads.
func AuthorizeVerificationChunkExpiry(ctx context.Context) (string, time.Time, error) {
	ws, ok := WorkspaceFromContext(ctx)
	actor := ActorFromContext(ctx)
	if !ok || ws == "" || actor.Role != core.ActorSystem || actor.ID != verificationReconcilerActor {
		return "", time.Time{}, ErrVerificationAccess
	}
	now := time.Now().UTC()
	if clock, ok := ctx.Value(verificationClockKey{}).(func() time.Time); ok {
		now = clock().UTC()
	}
	return ws, now, nil
}

// VerificationChunkExpiryLimitOrDefault keeps the per-run row bound in 1..100.
func VerificationChunkExpiryLimitOrDefault(limit int) int {
	if limit <= 0 || limit > VerificationChunkExpiryLimit {
		return VerificationChunkExpiryLimit
	}
	return limit
}

// GroupVerificationChunkCandidates orders candidates by expiry then ID, keeps
// at most limit rows, and groups them by task in first-expiry order. Each group
// becomes one locked transaction.
func GroupVerificationChunkCandidates(candidates []VerificationChunkCandidate, limit int) [][]VerificationChunkCandidate {
	sorted := append([]VerificationChunkCandidate(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].ExpiresAt.Equal(sorted[j].ExpiresAt) {
			return sorted[i].ExpiresAt.Before(sorted[j].ExpiresAt)
		}
		return sorted[i].ID < sorted[j].ID
	})
	if limit = VerificationChunkExpiryLimitOrDefault(limit); len(sorted) > limit {
		sorted = sorted[:limit]
	}
	var groups [][]VerificationChunkCandidate
	index := map[string]int{}
	for _, c := range sorted {
		i, ok := index[c.TaskID]
		if !ok {
			i = len(groups)
			index[c.TaskID] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], c)
	}
	return groups
}

// VerificationChunkExpiryEvent records one committed deletion. Its metadata is
// bounded by the per-run limit; a group that deletes nothing appends no event.
func VerificationChunkExpiryEvent(taskID string, cutoff time.Time, deleted []VerificationChunkCandidate) core.Event {
	chunks := make([]string, 0, len(deleted))
	var uploads []string
	seen := map[string]bool{}
	for _, c := range deleted {
		chunks = append(chunks, c.ID)
		if !seen[c.UploadID] {
			seen[c.UploadID] = true
			uploads = append(uploads, c.UploadID)
		}
	}
	sort.Strings(chunks)
	sort.Strings(uploads)
	return core.Event{TaskID: taskID, Kind: "verification." + VerificationExpireChunks, ActorID: verificationReconcilerActor, ActorRole: core.ActorSystem, Payload: core.JSONPayload(map[string]any{"cutoff": cutoff.UTC().Format(time.RFC3339Nano), "deleted_count": len(deleted), "chunk_ids": chunks, "upload_ids": uploads})}
}

type verificationChunkExpirySelectedKey struct{}

// WithVerificationChunkExpirySelectedForTest installs a conformance seam that
// every backend reaches after its bounded candidate read and before any locked
// recheck transaction. The callback receives the selected rows and may block,
// so a test can let finalization consume those rows before expiry rechecks
// them. Without this context value the seam does nothing.
func WithVerificationChunkExpirySelectedForTest(ctx context.Context, selected func([]VerificationChunkCandidate)) context.Context {
	return context.WithValue(ctx, verificationChunkExpirySelectedKey{}, selected)
}

// ExecuteVerificationChunkExpiry runs each task group through the taskops
// chunk.expire verification lease and sums the deleted rows. It stops at the
// first error or at context cancellation; committed groups stay committed and
// remaining rows wait for a later tick.
func ExecuteVerificationChunkExpiry(ctx context.Context, backend taskops.Backend, groups [][]VerificationChunkCandidate, expire func(taskops.TaskLease, []VerificationChunkCandidate) (int, error)) (int, error) {
	if selected, ok := ctx.Value(verificationChunkExpirySelectedKey{}).(func([]VerificationChunkCandidate)); ok {
		var rows []VerificationChunkCandidate
		for _, group := range groups {
			rows = append(rows, group...)
		}
		selected(rows)
	}
	total := 0
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := taskops.ExecuteVerification(ctx, backend, group[0].TaskID, VerificationExpireChunks, func(lease taskops.TaskLease) (int, error) {
			if !lease.ValidForCommand(group[0].TaskID, "verification."+VerificationExpireChunks) {
				return 0, ErrVerificationAccess
			}
			return expire(lease, group)
		})
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// VerificationChunkExpiryRecheck keeps the selected rows that still match under
// the transaction locks: same task, staging state, and expiry at or before the
// cutoff. A row consumed by finalization or deleted by an earlier run is gone.
func VerificationChunkExpiryRecheck(taskID string, cutoff time.Time, selected []VerificationChunkCandidate, current map[string]VerificationRow) []VerificationChunkCandidate {
	var keep []VerificationChunkCandidate
	for _, c := range selected {
		row, ok := current[c.ID]
		if !ok || row.Table != "verification_upload_chunks" || row.TaskID != taskID || row.State != "staging" || row.ExpiresAt == nil || row.ExpiresAt.After(cutoff) {
			continue
		}
		c.ExpiresAt, c.UploadID = row.ExpiresAt.UTC(), row.LogicalKey
		keep = append(keep, c)
	}
	return keep
}

// ExpireVerificationChunks implements the internal expiry for the volatile
// backend under its single mutex.
func (m *volatileMemory) ExpireVerificationChunks(ctx context.Context, limit int) (int, error) {
	if _, err := RequireActor(ctx); err != nil {
		return 0, err
	}
	ws, cutoff, err := AuthorizeVerificationChunkExpiry(ctx)
	if err != nil {
		return 0, err
	}
	prefix := ws + "\x00verification_upload_chunks\x00"
	m.mu.RLock()
	var candidates []VerificationChunkCandidate
	for key, row := range m.verificationRows {
		if strings.HasPrefix(key, prefix) && row.State == "staging" && row.ExpiresAt != nil && !row.ExpiresAt.After(cutoff) {
			candidates = append(candidates, VerificationChunkCandidate{ID: row.ID, TaskID: row.TaskID, UploadID: row.LogicalKey, ExpiresAt: row.ExpiresAt.UTC()})
		}
	}
	m.mu.RUnlock()
	return ExecuteVerificationChunkExpiry(ctx, m, GroupVerificationChunkCandidates(candidates, limit), func(_ taskops.TaskLease, group []VerificationChunkCandidate) (int, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		taskID := group[0].TaskID
		if task, ok := m.tasks[taskID]; !ok || task.Workspace != ws {
			return 0, ErrVerificationAccess
		}
		current := map[string]VerificationRow{}
		for _, c := range group {
			if row, ok := m.verificationRows[prefix+c.ID]; ok {
				current[c.ID] = row
			}
		}
		deleted := VerificationChunkExpiryRecheck(taskID, cutoff, group, current)
		if len(deleted) == 0 {
			return 0, nil
		}
		// Every fault point precedes the first mutation, so a refusal leaves
		// the staging rows and the ledger unchanged.
		for _, step := range []string{"evidence", "event"} {
			if err := VerificationFault(ctx, step); err != nil {
				return 0, err
			}
		}
		for _, c := range deleted {
			delete(m.verificationRows, prefix+c.ID)
		}
		m.appendEventLocked(ctx, VerificationChunkExpiryEvent(taskID, cutoff, deleted))
		return len(deleted), nil
	})
}
