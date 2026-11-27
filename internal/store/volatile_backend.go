package store

import (
	"context"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/memlog"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// volatileMemory adds deployment capabilities only for tests which explicitly
// request a Backend. NewMemory retains its existing Store and fixture behavior.
type volatileMemory struct {
	*memory
	memberships             map[memoryScopedKey]workspaceBinding
	invitations             map[memoryScopedKey]workspaceInvitation
	workspaces              map[string]workspaceRecord
	users                   map[string]identityUser
	credentials             map[string]identityCredential
	signInLinks             map[string]signInLink
	sessions                map[string]dashboardSession
	workspaceGitHubApps     map[string]workspaceAppRecord
	gitHubWebhookDeliveries map[string]map[string]time.Time
	stagePreferences        map[string]StagePreferences
	// oauth holds the deployment OAuth tables (component-persistence
	// invariant 21) under the memory mutex.
	oauth oauthState
	// environmentState holds the environment tables under the memory mutex
	// (component-environments).
	environmentState environmentState
	// feedbackState holds the feedback tables under the memory mutex
	// (component-production-feedback).
	feedbackState feedbackState
	// feedbackReportState holds the report table under the memory mutex
	// (component-feedback-reports).
	feedbackReportState    feedbackReportState
	orgName                string
	gitHubAppEncryptionKey []byte
	log                    eventlog.Store
}

var _ Backend = (*volatileMemory)(nil)

// NewVolatileBackend constructs the test backend. Deployment selection must
// use backend.Open, which requires an explicit AllowVolatile option.
func NewVolatileBackend() Backend {
	mem := NewMemory().(*memory)
	backend := &volatileMemory{
		memory:      mem,
		memberships: map[memoryScopedKey]workspaceBinding{},
		invitations: map[memoryScopedKey]workspaceInvitation{},
		workspaces:  map[string]workspaceRecord{},
		users:       map[string]identityUser{},
		credentials: map[string]identityCredential{},
		signInLinks: map[string]signInLink{},
		sessions:    map[string]dashboardSession{},
		// stagePreferences is keyed by user ID (component-stage-preferences).
		stagePreferences: map[string]StagePreferences{},
		oauth:            newOAuthState(),
		environmentState: newEnvironmentState(),
		feedbackState:    newFeedbackState(),
		// feedbackReportState is keyed by workspace and environment ID.
		feedbackReportState: newFeedbackReportState(),
		orgName:             "Conveyor",
		log:                 logqueue.Notify(memlog.New(), mem.changes),
	}
	backend.readClockPolicy = func(workspace string) (config.RecoveryPolicy, time.Duration, error) {
		record, ok := backend.workspaces[workspace]
		if !ok || strings.TrimSpace(record.ConfigYAML) == "" {
			return config.DefaultRecoveryPolicy(), config.DefaultWorkOrderQueueTimeout, nil
		}
		return config.ReadClockPolicy([]byte(record.ConfigYAML))
	}
	// The acceptance edge reads the live trust list and each author's role
	// under the memory lock it already holds, so these read the maps directly
	// and never call a reader that takes the lock (component-risk-review
	// invariant 19).
	backend.readTrustedAuthors = func(workspace, repo string) ([]string, error) {
		return StoredTrustedAuthors([]byte(backend.workspaces[workspace].ConfigYAML), repo)
	}
	backend.operateGatesLocked = func(workspace, userID string) bool {
		binding, ok := backend.memberships[memoryScopedKey{workspace: workspace, id: userID}]
		return ok && core.RoleAllows(binding.Role, core.CapabilityOperateGates)
	}
	return backend
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

// recordEventLocked records an event in a fresh workspace context. The caller
// passes the actor the operation derived, so the fresh context never relies on
// an implicit default.
func (m *volatileMemory) recordEventLocked(actor Actor, workspace string, event core.Event) core.Event {
	m.appendEventLocked(WithActor(WithWorkspace(context.Background(), workspace), actor), event)
	event.ID = m.nextEventID
	return event
}
