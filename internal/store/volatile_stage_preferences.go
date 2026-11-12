package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

var errStagePreferenceUser = errors.New("stage preferences require a user")

// GetStagePreferences implements StagePreferenceStore. A person with no saved
// set reads version 0 with every stage unset (req-stage-preferences AC-2.4).
func (m *volatileMemory) GetStagePreferences(_ context.Context, userID string) (StagePreferences, error) {
	if strings.TrimSpace(userID) == "" {
		return StagePreferences{}, errStagePreferenceUser
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneStagePreferences(m.stagePreferences[userID]), nil
}

// UpdateStagePreferences implements StagePreferenceStore. The store mutex
// serializes every writer, so apply always sees the latest set and a refused
// apply leaves the set unchanged (component-stage-preferences invariant 4).
// apply must not call back into the store.
func (m *volatileMemory) UpdateStagePreferences(_ context.Context, userID string, apply StagePreferenceUpdate) (StagePreferences, error) {
	if strings.TrimSpace(userID) == "" {
		return StagePreferences{}, errStagePreferenceUser
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := cloneStagePreferences(m.stagePreferences[userID])
	next, err := apply(current)
	if err != nil {
		return StagePreferences{}, err
	}
	// The stored copy goes through the durable backends' column encoding, so
	// the volatile backend returns what PostgreSQL and SingleStore return.
	columns, err := EncodeStagePreferenceColumns(next)
	if err != nil {
		return StagePreferences{}, err
	}
	stages, err := DecodeStagePreferenceColumns(columns)
	if err != nil {
		return StagePreferences{}, err
	}
	updated := StagePreferenceTime(time.Now())
	saved := StagePreferences{Version: current.Version + 1, UpdatedAt: &updated, Stages: stages}
	m.stagePreferences[userID] = saved
	return cloneStagePreferences(saved), nil
}

func cloneStagePreferences(value StagePreferences) StagePreferences {
	cloned := StagePreferences{Version: value.Version, Stages: CloneStagePreferenceStages(value.Stages)}
	if value.UpdatedAt != nil {
		at := *value.UpdatedAt
		cloned.UpdatedAt = &at
	}
	return cloned
}
