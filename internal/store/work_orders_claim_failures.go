package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// ValidateWorkOrderClaimFailure checks the arguments every backend's
// RecordWorkOrderClaimFailure accepts (component-persistence, Schema domains).
func ValidateWorkOrderClaimFailure(workOrderID, errorClass string, at time.Time) error {
	if strings.TrimSpace(workOrderID) == "" {
		return fmt.Errorf("claim-failure record requires a work order id")
	}
	if errorClass != core.WorkOrderClaimFailureClassBackendOperation {
		return fmt.Errorf("claim-failure record error class %q is not %q", errorClass, core.WorkOrderClaimFailureClassBackendOperation)
	}
	if at.IsZero() {
		return fmt.Errorf("claim-failure record requires a failure time")
	}
	return nil
}

// SortWorkOrderClaimFailures orders records by work order ID, the order every
// backend returns.
func SortWorkOrderClaimFailures(records []core.WorkOrderClaimFailure) {
	sort.Slice(records, func(i, j int) bool { return records[i].WorkOrderID < records[j].WorkOrderID })
}

func (m *memory) RecordWorkOrderClaimFailure(_ context.Context, workOrderID, errorClass string, at time.Time) (core.WorkOrderClaimFailure, error) {
	if err := ValidateWorkOrderClaimFailure(workOrderID, errorClass, at); err != nil {
		return core.WorkOrderClaimFailure{}, err
	}
	at = at.UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.workOrders[workOrderID]; !ok {
		return core.WorkOrderClaimFailure{}, fmt.Errorf("%w: work order %s", ErrNotFound, workOrderID)
	}
	record, ok := m.claimFailures[workOrderID]
	if !ok {
		record = core.WorkOrderClaimFailure{WorkOrderID: workOrderID, FirstFailedAt: at}
	}
	record.ConsecutiveFailures = core.NextWorkOrderClaimFailureCount(record.ConsecutiveFailures)
	record.LastFailedAt = at
	record.ErrorClass = errorClass
	m.claimFailures[workOrderID] = record
	return record, nil
}

func (m *memory) ListWorkOrderClaimFailures(_ context.Context, workOrderIDs []string) ([]core.WorkOrderClaimFailure, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []core.WorkOrderClaimFailure{}
	seen := map[string]bool{}
	for _, id := range workOrderIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if record, ok := m.claimFailures[id]; ok {
			out = append(out, record)
		}
	}
	SortWorkOrderClaimFailures(out)
	return out, nil
}
