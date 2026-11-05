package httpapi

import "github.com/kidus-tiliksew/conveyor/internal/core"

// retiredAgentKind is the kind a stored row of the retired agent class carries.
// No store verification returns it now (DEC-116(1), (3)). A test presents it
// directly to prove a user-only guard still fails closed.
const retiredAgentKind = core.CredentialKind("agent")
