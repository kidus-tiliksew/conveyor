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

// SeedCredentialLastUsedForTest sets one bearer credential's stored last use
// relative to now. No public writer can; the memory conformance entry point
// supplies it as Fixture.SeedCredentialLastUsed
// (component-identity-membership; component-verification-strategy).
func SeedCredentialLastUsedForTest(t *testing.T, st Backend, credentialID string, lastUsedAgo time.Duration) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.Lock()
	defer m.mu.Unlock()
	credential, ok := m.credentials[credentialID]
	if !ok {
		t.Fatalf("seed requires an existing credential %q", credentialID)
	}
	credential.LastUsedAt = timePtr(time.Now().UTC().Add(-lastUsedAgo))
	m.credentials[credentialID] = credential
}

// CredentialLastUsedForTest returns one bearer credential's stored last use,
// zero when unset; no public read exposes an agent credential's.
func CredentialLastUsedForTest(t *testing.T, st Backend, credentialID string) time.Time {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.RLock()
	defer m.mu.RUnlock()
	credential, ok := m.credentials[credentialID]
	if !ok {
		t.Fatalf("credential %q is missing", credentialID)
	}
	if credential.LastUsedAt == nil {
		return time.Time{}
	}
	return *credential.LastUsedAt
}
