package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// OAuthDeploymentEventsForTest returns the volatile deployment events whose
// kind starts with identity.oauth_, in append order. The memory conformance
// entry point supplies it as Fixture.OAuthDeploymentEvents.
func OAuthDeploymentEventsForTest(t *testing.T, st Backend) []core.Event {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []core.Event
	for _, event := range m.events[""] {
		if strings.HasPrefix(event.Kind, "identity.oauth_") {
			out = append(out, event)
		}
	}
	return out
}

// AgeOAuthRecordForTest moves every stored time of one OAuth row back by
// by. table is one of the four deployment OAuth table names.
func AgeOAuthRecordForTest(t *testing.T, st Backend, table, id string, by time.Duration) {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.Lock()
	defer m.mu.Unlock()
	back := func(at *time.Time) *time.Time {
		if at == nil {
			return nil
		}
		return timePtr(at.Add(-by))
	}
	switch table {
	case "oauth_clients":
		client, ok := m.oauth.clients[id]
		if !ok {
			t.Fatalf("OAuth client %q is missing", id)
		}
		client.CreatedAt, client.UnusedExpiresAt, client.FirstUsedAt = client.CreatedAt.Add(-by), client.UnusedExpiresAt.Add(-by), back(client.FirstUsedAt)
		m.oauth.clients[id] = client
	case "oauth_authorization_codes":
		code, ok := m.oauth.codes[id]
		if !ok {
			t.Fatalf("OAuth code %q is missing", id)
		}
		code.CreatedAt, code.ExpiresAt, code.ConsumedAt = code.CreatedAt.Add(-by), code.ExpiresAt.Add(-by), back(code.ConsumedAt)
		m.oauth.codes[id] = code
	case "oauth_grants":
		grant, ok := m.oauth.grants[id]
		if !ok {
			t.Fatalf("OAuth grant %q is missing", id)
		}
		grant.CreatedAt, grant.LastUsedAt, grant.RevokedAt = grant.CreatedAt.Add(-by), back(grant.LastUsedAt), back(grant.RevokedAt)
		m.oauth.grants[id] = grant
	case "oauth_tokens":
		token, ok := m.oauth.tokens[id]
		if !ok {
			t.Fatalf("OAuth token %q is missing", id)
		}
		token.CreatedAt, token.ExpiresAt, token.RotatedAt = token.CreatedAt.Add(-by), token.ExpiresAt.Add(-by), back(token.RotatedAt)
		m.oauth.tokens[id] = token
	default:
		t.Fatalf("unknown OAuth table %q", table)
	}
}

// OAuthStoredRowForTest is one stored volatile OAuth row: its table, ID,
// digest, and every other field rendered as text.
type OAuthStoredRowForTest struct {
	Table, ID, Text string
	Digest          []byte
}

// OAuthStoredRowsForTest returns every row of the volatile OAuth tables.
func OAuthStoredRowsForTest(_ context.Context, t *testing.T, st Backend) []OAuthStoredRowForTest {
	t.Helper()
	m := st.(*volatileMemory)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var rows []OAuthStoredRowForTest
	for id, client := range m.oauth.clients {
		rows = append(rows, OAuthStoredRowForTest{Table: "oauth_clients", ID: id, Text: fmt.Sprintf("%+v", client)})
	}
	for id, code := range m.oauth.codes {
		rows = append(rows, OAuthStoredRowForTest{Table: "oauth_authorization_codes", ID: id, Digest: code.CodeHash, Text: fmt.Sprintf("%+v %v", code.OAuthAuthorizationCode, code.ConsumedAt)})
	}
	for id, grant := range m.oauth.grants {
		rows = append(rows, OAuthStoredRowForTest{Table: "oauth_grants", ID: id, Text: fmt.Sprintf("%+v %s", grant.OAuthGrant, grant.CurrentRefreshID)})
	}
	for id, token := range m.oauth.tokens {
		rows = append(rows, OAuthStoredRowForTest{Table: "oauth_tokens", ID: id, Digest: token.TokenHash, Text: fmt.Sprintf("%s %s %s %s %s %v", token.ID, token.GrantID, token.Kind, token.CreatedAt, token.ExpiresAt, token.RotatedAt)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Table+rows[i].ID < rows[j].Table+rows[j].ID })
	return rows
}
