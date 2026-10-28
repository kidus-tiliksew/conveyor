package store

import (
	"testing"
	"time"
)

// SeedDashboardSessionTimesForTest sets one dashboard session's stored last
// use and expiry relative to now. No public writer can; the memory
// conformance entry point supplies it as Fixture.SeedDashboardSessionTimes
// (component-identity-membership; component-verification-strategy).
func SeedDashboardSessionTimesForTest(t *testing.T, st Backend, sessionID string, lastUsedAgo, expiresIn time.Duration) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		t.Fatalf("seed requires an existing dashboard session %q", sessionID)
	}
	now := time.Now().UTC()
	session.LastUsedAt, session.ExpiresAt = now.Add(-lastUsedAgo), now.Add(expiresIn)
	m.sessions[sessionID] = session
}

// DashboardSessionTimesForTest returns one dashboard session's stored last
// use and expiry; no public read exposes them.
func DashboardSessionTimesForTest(t *testing.T, st Backend, sessionID string) (time.Time, time.Time) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[sessionID]
	if !ok {
		t.Fatalf("dashboard session %q is missing", sessionID)
	}
	return session.LastUsedAt, session.ExpiresAt
}
