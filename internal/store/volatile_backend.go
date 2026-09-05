package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
)

// volatileMemory adds deployment capabilities only for tests which explicitly
// request a Backend. NewMemory retains its existing Store and fixture behavior.
type volatileMemory struct {
	*memory
	memberships          map[memoryScopedKey]workspaceBinding
	invitations          map[memoryScopedKey]workspaceInvitation
	workspaces           map[string]workspaceRecord
	users                map[string]identityUser
	credentials          map[string]identityCredential
	signInLinks          map[string]signInLink
	sessions             map[string]dashboardSession
	forgeTokens          map[string]forgeTokenRecord
	workspaceForgeTokens map[string]forgeTokenRecord
	orgName              string
	forgeTokenKey        []byte
	log                  eventlog.Store
}

var _ Backend = (*volatileMemory)(nil)

// NewVolatileBackend constructs the test backend. Deployment selection must
// use backend.Open, which requires an explicit AllowVolatile option.
func NewVolatileBackend() Backend {
	return &volatileMemory{
		memory:               NewMemory().(*memory),
		memberships:          map[memoryScopedKey]workspaceBinding{},
		invitations:          map[memoryScopedKey]workspaceInvitation{},
		workspaces:           map[string]workspaceRecord{},
		users:                map[string]identityUser{},
		credentials:          map[string]identityCredential{},
		signInLinks:          map[string]signInLink{},
		sessions:             map[string]dashboardSession{},
		forgeTokens:          map[string]forgeTokenRecord{},
		workspaceForgeTokens: map[string]forgeTokenRecord{},
		orgName:              "Conveyor",
		log:                  memlog.New(),
	}
}

func (m *volatileMemory) Close()              {}
func (m *volatileMemory) Log() eventlog.Store { return m.log }
func (m *volatileMemory) lock()               { m.mu.Lock() }

func (m *volatileMemory) unlock() {
	// The existing task store reads these fixture maps for assignment and
	// worker eligibility. Update them under the same mutex as membership.
	clear(m.workspaceMembers)
	clear(m.workspaceMemberRoles)
	for key, binding := range m.memberships {
		m.workspaceMembers[key] = m.userActive(key.id)
		m.workspaceMemberRoles[key] = binding.Role
	}
	m.mu.Unlock()
}

func (m *volatileMemory) recordEventLocked(workspace string, event core.Event) core.Event {
	m.appendEventLocked(WithWorkspace(context.Background(), workspace), event)
	event.ID = m.nextEventID
	return event
}
