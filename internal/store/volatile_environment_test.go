package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// volatileEnvironmentKeyForTest reads one stored key and holds the memory
// mutex until the test hook calls unlock.
func volatileEnvironmentKeyForTest(t *testing.T, st Backend, ctx context.Context, keyID string) (m *volatileMemory, key memoryScopedKey, record environmentKeyRecord, unlock func()) {
	t.Helper()
	m = st.(*volatileMemory)
	m.mu.Lock()
	key = memoryScopedKey{workspace: workspaceOrDefault(ctx, ""), id: keyID}
	record, ok := m.environmentState.keys[key]
	if !ok {
		m.mu.Unlock()
		t.Fatalf("environment key %q is missing", keyID)
	}
	return m, key, record, m.mu.Unlock
}

// AgeEnvironmentKeyForTest moves one volatile environment key's last use back
// by by. The memory conformance entry point supplies it as
// Fixture.AgeEnvironmentKey.
func AgeEnvironmentKeyForTest(t *testing.T, st Backend, ctx context.Context, keyID string, by time.Duration) {
	t.Helper()
	m, key, record, unlock := volatileEnvironmentKeyForTest(t, st, ctx, keyID)
	defer unlock()
	if record.LastUsedAt == nil {
		t.Fatalf("environment key %q has no last use", keyID)
	}
	record.LastUsedAt = timePtr(record.LastUsedAt.Add(-by))
	m.environmentState.keys[key] = record
}

// StoredEnvironmentKeyForTest returns one volatile environment key's stored
// hash and its other fields as text.
func StoredEnvironmentKeyForTest(t *testing.T, st Backend, ctx context.Context, keyID string) ([]byte, string) {
	t.Helper()
	_, _, record, unlock := volatileEnvironmentKeyForTest(t, st, ctx, keyID)
	defer unlock()
	digest := record.Hash
	record.Hash = nil
	return digest, fmt.Sprintf("%+v", record)
}
