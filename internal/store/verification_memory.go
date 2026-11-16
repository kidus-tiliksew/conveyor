package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type verificationSecrets []string

func (s verificationSecrets) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s, nil
}

func (m *volatileMemory) ApplyVerification(ctx context.Context, c VerificationCommand) (VerificationReceipt, error) {
	if _, err := RequireActor(ctx); err != nil {
		return VerificationReceipt{}, err
	}
	return taskops.ExecuteVerification(ctx, m, c.Access.TaskID, c.Kind, func(lease taskops.TaskLease) (VerificationReceipt, error) { return m.applyVerification(ctx, lease, c) })
}

func (m *volatileMemory) applyVerification(ctx context.Context, lease taskops.TaskLease, c VerificationCommand) (VerificationReceipt, error) {
	if !lease.ValidForCommand(c.Access.TaskID, "verification."+c.Kind) {
		return VerificationReceipt{}, ErrVerificationAccess
	}
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
		if err = VerifyVerificationClaim(ctx, c.Access, m.tasks[c.Access.TaskID], m.workOrders[c.Access.WorkOrderID], c.Kind != VerificationSeal, now); err != nil {
			// component-verification-service: only the exact retained checkpoint claim may replay.
			ws, _ := WorkspaceFromContext(ctx)
			if c.Kind != VerificationSeal || VerifyVerificationCheckpointReplay(ctx, c.Access, m.tasks[c.Access.TaskID], m.workOrders[c.Access.WorkOrderID], m.verificationRowsLocked(ws, c.Access.TaskID, VerificationReplayTables), c.ContextID, now) != nil {
				return VerificationReceipt{}, err
			}
		}
	}
	if err = BindVerificationPermissionOrder(ctx, &c, m.tasks[c.Access.TaskID], m.workOrders[c.Access.WorkOrderID], now); err != nil {
		return VerificationReceipt{}, err
	}
	if err = VerifyVerificationAuthority(c, authority, m.workOrders[c.Access.WorkOrderID]); err != nil {
		return VerificationReceipt{}, err
	}
	BindVerificationAuthority(&c, authority, m.workOrders[c.Access.WorkOrderID])
	ws, _ := WorkspaceFromContext(ctx)
	rows := m.verificationRowsLocked(ws, c.Access.TaskID, VerificationCommandTables(c))
	c.SubmittedHeadSHA = m.workOrders[c.Access.WorkOrderID].HeadSHA
	mutation, err := PrepareVerificationMutation(ctx, verificationSecrets(secrets), memoryVerificationScope{m: m.memory, task: c.Access.TaskID}, c, rows, now)
	if err != nil {
		return VerificationReceipt{}, err
	}
	if len(mutation.Rows) == 0 && len(mutation.DeleteChunks) == 0 {
		return mutation.Receipt, nil
	}
	var completion *VerificationCompletion
	var jobIndex int
	if c.Kind == VerificationSeal {
		c = VerificationSealedCommand(c, mutation)
		job, index, ok := m.findJobLocked(m.workOrders[c.Access.WorkOrderID].JobID)
		if !ok {
			return VerificationReceipt{}, ErrVerificationState
		}
		v, e := PrepareVerificationCompletion(ctx, m.tasks[c.Access.TaskID], m.workOrders[c.Access.WorkOrderID], job, rows, m.events[c.Access.TaskID], c, now)
		if e != nil {
			return VerificationReceipt{}, e
		}
		completion, jobIndex = &v, index
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

	var deliveryUpdates []core.VerificationDelivery
	if c.Kind == VerificationWriteEvidence || mutation.Publication != nil {
		sourceID := ""
		if mutation.Publication != nil {
			sourceID = mutation.Publication.ID
		}
		added, updates, e := m.deliveryIntentLocked(ctx, c.Access.TaskID, append(append([]VerificationRow(nil), rows...), mutation.Rows...), m.events[c.Access.TaskID], sourceID)
		if e != nil {
			return VerificationReceipt{}, e
		}
		mutation.Rows = append(mutation.Rows, added...)
		deliveryUpdates = updates
		if e = m.enqueueDeliveryLocked(ctx, updates); e != nil {
			return VerificationReceipt{}, e
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
	if completion != nil {
		m.tasks[completion.Task.ID] = completion.Task
		m.workOrders[completion.Order.ID] = completion.Order
		m.jobs[completion.Job.TaskID][jobIndex] = completion.Job
		if completion.Intervention != nil {
			m.interventions[completion.Task.ID] = append(m.interventions[completion.Task.ID], *completion.Intervention)
		}
		for _, e := range completion.Events {
			m.appendEventLocked(ctx, e)
		}
	}
	m.putDeliveriesLocked(deliveryUpdates)
	m.appendEventLocked(ctx, mutation.Event)
	committed = true
	return mutation.Receipt, nil
}
func (m *memory) verificationRowsLocked(ws, task string, tables VerificationTableSet) []VerificationRow {
	var rows []VerificationRow
	wanted := tables.index()
	for key, row := range m.verificationRows {
		if len(key) > len(ws) && key[:len(ws)+1] == ws+"\x00" && row.TaskID == task && wanted[row.Table] {
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

// verificationOperationByKeyLocked returns the operation that owns a logical
// key anywhere in the workspace, so a registration refuses another task's key
// without reading every task's operation rows (component-verification-service).
func (m *memory) verificationOperationByKeyLocked(ws, key string) (VerificationRow, bool) {
	for k, row := range m.verificationRows {
		if row.Table == "verification_operations" && row.LogicalKey == key && len(k) > len(ws) && k[:len(ws)+1] == ws+"\x00" {
			row.Body = append([]byte(nil), row.Body...)
			return row, true
		}
	}
	return VerificationRow{}, false
}

// unresolvedVerificationOperationsLocked returns another task's unresolved
// operations for one subject, step, and target: changing the source cannot hide
// an unresolved external action (component-verification-service).
func (m *memory) unresolvedVerificationOperationsLocked(ws, task, subjectKey, stepID, target string) []VerificationRow {
	var rows []VerificationRow
	for k, row := range m.verificationRows {
		if row.Table != "verification_operations" || row.TaskID == task || verificationOperationResolved(row.State) || len(k) <= len(ws) || k[:len(ws)+1] != ws+"\x00" {
			continue
		}
		op := verificationDecode[VerificationOperation](row)
		if op.SubjectKey != subjectKey || op.StepID != stepID || op.Target != target {
			continue
		}
		row.Body = append([]byte(nil), row.Body...)
		rows = append(rows, row)
	}
	return rows
}

// memoryVerificationScope serves the operation lookups a registration needs
// from the in-memory image the caller already holds locked.
type memoryVerificationScope struct {
	m    *memory
	task string
}

func (s memoryVerificationScope) VerificationOperationByKey(ctx context.Context, key string) (VerificationRow, bool, error) {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return VerificationRow{}, false, ErrVerificationAccess
	}
	row, found := s.m.verificationOperationByKeyLocked(ws, key)
	return row, found, nil
}

func (s memoryVerificationScope) UnresolvedVerificationOperations(ctx context.Context, subjectKey, stepID, target string) ([]VerificationRow, error) {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return nil, ErrVerificationAccess
	}
	return s.m.unresolvedVerificationOperationsLocked(ws, s.task, subjectKey, stepID, target), nil
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
	rows := m.verificationRowsLocked(ws, a.TaskID, VerificationSnapshotTables)
	noteVerificationReadTask(ctx, VerificationReadRows, a.TaskID, len(rows))
	snapshot, err := VerificationReadSnapshot(rows, a, id, m.workOrders[a.WorkOrderID].Stage)
	if err != nil {
		return VerificationSnapshot{}, err
	}
	var ds []core.VerificationDelivery
	for _, d := range m.verificationDeliveries {
		if d.WorkspaceID == ws && d.TaskID == a.TaskID {
			ds = append(ds, d)
		}
	}
	AttachVerificationDeliverySnapshot(&snapshot, ds)
	if len(snapshot.Publications) > 0 {
		// One batched metadata pass over the task's deliveries, so the read's
		// statement count does not grow with one lookup per publication
		// (component-persistence, Database read accounting).
		attached := 0
		for i := range snapshot.Publications {
			if snapshot.Publications[i].Delivery != nil {
				attached++
			}
		}
		noteVerificationReadTask(ctx, VerificationReadMetadata, a.TaskID, attached)
	}
	return snapshot, nil
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
	rows := m.verificationRowsLocked(ws, a.TaskID, VerificationArtifactTables)
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
	if _, err := RequireActor(ctx); err != nil {
		return 0, err
	}
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return 0, ErrVerificationAccess
	}
	m.mu.RLock()
	var rows []VerificationRow
	for _, task := range m.tasks {
		if task.Workspace == ws {
			rows = append(rows, m.verificationRowsLocked(ws, task.ID, VerificationClaimTables)...)
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

func (m *volatileMemory) RecoverWorkOrderCommand(ctx context.Context, lease taskops.TaskLease, id, requestID, direction string, timeout time.Duration, refreeze ...*RecoveryRefreeze) (core.WorkOrder, error) {
	if _, err := RequireActor(ctx); err != nil {
		return core.WorkOrder{}, err
	}
	clean, err := SanitizeVerificationRecovery(ctx, m)
	if err != nil {
		return core.WorkOrder{}, err
	}
	return m.memory.RecoverWorkOrderCommand(clean, lease, id, requestID, direction, timeout, refreeze...)
}

func (m *volatileMemory) deliveryIntentLocked(ctx context.Context, task string, rows []VerificationRow, events []core.Event, sourceID string) ([]VerificationRow, []core.VerificationDelivery, error) {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || m.tasks[task].Workspace != ws {
		return nil, nil, ErrVerificationAccess
	}
	pr, found := VerificationPRFromEvents(events)
	if !found {
		return nil, nil, nil
	}
	var prior []core.VerificationDelivery
	for _, d := range m.verificationDeliveries {
		if d.WorkspaceID == ws && d.Repository == pr.Repository && d.PullRequestNumber == pr.Number {
			prior = append(prior, d)
		}
	}
	return PrepareVerificationDelivery(ws, task, pr, rows, prior, sourceID, time.Now().UTC())
}
func (m *volatileMemory) enqueueDeliveryLocked(ctx context.Context, updates []core.VerificationDelivery) error {
	for _, d := range updates {
		if d.State == "pending" {
			a := VerificationDeliveryArgs(d)
			if _, err := logqueue.Enqueue(ctx, m.log, d.WorkspaceID, a.Kind(), a.UniqueKey(), a, 5, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}
func (m *volatileMemory) putDeliveriesLocked(updates []core.VerificationDelivery) {
	if m.verificationDeliveries == nil {
		m.verificationDeliveries = map[string]core.VerificationDelivery{}
	}
	for _, d := range updates {
		m.verificationDeliveries[d.WorkspaceID+"\x00"+d.ID] = d
	}
}
func (m *volatileMemory) RecordVerificationPullRequest(ctx context.Context, e core.Event) error {
	if _, err := RequireActor(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || m.tasks[e.TaskID].Workspace != ws || e.Kind != "pull_request.opened" {
		return ErrVerificationAccess
	}
	if e.JobID != "" {
		j, _, exists := m.findJobLocked(e.JobID)
		if !exists || j.TaskID != e.TaskID {
			return ErrVerificationAccess
		}
	}
	events := append(append([]core.Event(nil), m.events[e.TaskID]...), e)
	rows, updates, err := m.deliveryIntentLocked(ctx, e.TaskID, m.verificationRowsLocked(ws, e.TaskID, VerificationDeliveryTables), events, "")
	if err != nil {
		return err
	}
	if err = VerificationFault(ctx, "queue"); err != nil {
		return err
	}
	if err = m.enqueueDeliveryLocked(ctx, updates); err != nil {
		return err
	}
	if m.verificationRows == nil {
		m.verificationRows = map[string]VerificationRow{}
	}
	for _, row := range rows {
		m.verificationRows[ws+"\x00"+row.Table+"\x00"+row.ID] = row
	}
	m.putDeliveriesLocked(updates)
	m.appendEventLocked(ctx, e)
	return nil
}
func (m *volatileMemory) TranslateVerificationPublication(ctx context.Context, p VerificationPublication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, _ := WorkspaceFromContext(ctx)
	rows, updates, err := m.deliveryIntentLocked(ctx, p.TaskID, m.verificationRowsLocked(ws, p.TaskID, VerificationDeliveryTables), m.events[p.TaskID], p.ID)
	if err != nil {
		return err
	}
	if len(rows) != 0 {
		return ErrVerificationInvalid
	}
	if err = m.enqueueDeliveryLocked(ctx, updates); err != nil {
		return err
	}
	m.putDeliveriesLocked(updates)
	return nil
}
func (m *volatileMemory) RunVerificationDelivery(ctx context.Context, a queue.VerificationPublicationArgs, fn func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error) error {
	ws, _ := WorkspaceFromContext(ctx)
	if !a.ValidWorkspace(ws) {
		return ErrVerificationAccess
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var ds []core.VerificationDelivery
	for _, d := range m.verificationDeliveries {
		if d.WorkspaceID == ws && d.Repository == a.Repository && d.PullRequestNumber == a.PullRequestNumber {
			ds = append(ds, d)
		}
	}
	current, ok := VerificationDeliveryLatest(ds)
	if !ok {
		return nil
	}
	noteVerificationReadTask(ctx, VerificationReadDelivery, current.TaskID, 1)
	view := current
	save := func(command string, next core.VerificationDelivery) error {
		if command == "check" {
			if m.verificationDeliveries[ws+"\x00"+current.ID].Generation != current.Generation {
				return ErrVerificationConflict
			}
			return nil
		}
		updated, err := AdvanceVerificationDelivery(current, command, next, time.Now().UTC())
		if err != nil {
			return err
		}

		current = updated
		view = updated
		if command == "attempt" {
			m.putDeliveriesLocked([]core.VerificationDelivery{updated})
		}
		return nil
	}
	if err := fn(&view, save); err != nil {
		return err
	}
	m.putDeliveriesLocked([]core.VerificationDelivery{current})
	return nil
}
func (m *volatileMemory) ReconcileVerificationDeliveries(ctx context.Context) error {
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return ErrVerificationAccess
	}
	m.mu.Lock()
	var repaired []string
	defer func() {
		m.mu.Unlock()
		// A pending or retrying delivery whose job was enqueued is drift,
		// reported once the lock is released (component-persistence, Drift
		// repair reports).
		for _, taskID := range repaired {
			ReportDriftRepair(ctx, DriftVerificationDeliveryWithoutJob.For(taskID))
		}
	}()
	now := time.Now().UTC()
	// The pass acts on the newest due generation of each pull-request key, in
	// the same bounded, due-time ordered window the SQL backends select
	// (component-verification-publication; component-persistence, Database read
	// accounting).
	var due []core.VerificationDelivery
	for _, d := range m.verificationDeliveries {
		if d.WorkspaceID == ws && (d.State == "pending" || d.State == "retrying" || d.State == "failed" && d.NextAttemptAt != nil && !d.NextAttemptAt.After(now)) {
			due = append(due, d)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		left, right := dueAt(due[i]), dueAt(due[j])
		if !left.Equal(right) {
			return left.Before(right)
		}
		leftKey, rightKey := VerificationDeliveryKey(due[i].Repository, due[i].PullRequestNumber), VerificationDeliveryKey(due[j].Repository, due[j].PullRequestNumber)
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		return due[i].Generation < due[j].Generation
	})
	newest := map[string]int{}
	var window []core.VerificationDelivery
	for _, d := range due {
		key := VerificationDeliveryKey(d.Repository, d.PullRequestNumber)
		if generation, found := newest[key]; found && generation >= d.Generation {
			continue
		}
		newest[key] = d.Generation
		window = append(window, d)
	}
	noteVerificationReadTask(ctx, VerificationReadDue, "", len(due))
	if len(window) > VerificationDeliveryReconcileWindow {
		window = window[:VerificationDeliveryReconcileWindow]
	}
	// Only the acted rows' bodies are decoded, so a superseded generation's
	// retained payload is never read by a pass (component-persistence, Database
	// read accounting).
	noteVerificationReadTask(ctx, VerificationReadBodies, "", len(window))
	for _, d := range window {
		{
			due := d.State == "failed"
			if due {
				next, err := AdvanceVerificationDelivery(d, "retry", d, time.Now().UTC())
				if err != nil {
					return err
				}
				d = next
			}
			a := VerificationDeliveryArgs(d)
			inserted, err := logqueue.Enqueue(ctx, m.log, ws, a.Kind(), a.UniqueKey(), a, 5, time.Now().UTC())
			if err != nil {
				return err
			}
			if inserted && !due {
				repaired = append(repaired, d.TaskID)
			}
			m.putDeliveriesLocked([]core.VerificationDelivery{d})
		}
	}
	return nil
}

// dueAt is the reconcile order key: a row that has never been attempted sorts
// first, matching the SQL backends' COALESCE(next_attempt_at, epoch).
func dueAt(d core.VerificationDelivery) time.Time {
	if d.NextAttemptAt == nil {
		return time.Unix(0, 0).UTC()
	}
	return d.NextAttemptAt.UTC()
}
