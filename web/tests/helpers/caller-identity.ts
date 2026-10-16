// Mocked GET /v1/me projections in the shape the server returns
// (component-identity-membership). Every read carries an explicit
// can_create_workspaces boolean, the caller's own workspace-creation right,
// which defaults to false and is never derived from a role or capability.
// Only a read that names workspace context (the workspace_id query or the
// X-Workspace-ID header) adds the role, the role's capability list, and the
// ascending role chain; an unscoped read carries none of them. These are test
// fixtures only. The dashboard itself holds no role-to-capability table and
// renders whatever the server serves; the Go HTTP tests in
// internal/httpapi/identity_me_capabilities_test.go prove the served lists and
// the creation right against internal/core/authorization.go.

export const servedRoleChain = ['viewer', 'executor', 'contributor', 'maintainer', 'operator'] as const

const viewer = ['view_workspace']
const executor = [...viewer, 'claim_work', 'request_changes']
const contributor = [...executor, 'propose_documents']
const maintainer = [
  ...contributor,
  'create_tasks',
  'set_assignee',
  'operate_gates',
  'recover_work',
  'manage_reference_documents',
]
const operator = [...maintainer, 'confirm_documents', 'manage_membership', 'manage_workspace']

const servedCapabilities: Record<string, readonly string[]> = {
  viewer,
  executor,
  contributor,
  maintainer,
  operator,
}

// Example capability list the server serves for a governed role.
export function capabilitiesForRole(role: string): string[] {
  return [...(servedCapabilities[role] ?? [])].sort()
}

// The parts of a Playwright Request the projection depends on.
export type IdentityRequest = { url(): string; headers(): Record<string, string> }

// Whether a mocked identity read names workspace context the way the server
// reads it: a non-blank workspace_id query or X-Workspace-ID header.
export function isScopedIdentityRequest(request: IdentityRequest) {
  const query = new URL(request.url()).searchParams.get('workspace_id')?.trim()
  const header = request.headers()['x-workspace-id']?.trim()
  return Boolean(query || header)
}

// Serves an identity fixture for one mocked request. A scoped read gets the
// served capability list and role chain unless the fixture states its own
// lists explicitly; an unscoped read drops the role and both lists. The
// creation right is always explicit.
export function callerIdentity<
  T extends { role?: string; capabilities?: string[]; roles?: string[]; can_create_workspaces?: boolean },
>(identity: T, request: IdentityRequest) {
  const { role, capabilities, roles, ...rest } = identity
  const base = { ...rest, can_create_workspaces: identity.can_create_workspaces ?? false }
  if (!isScopedIdentityRequest(request)) return base
  return {
    ...base,
    role,
    capabilities: capabilities ?? (role ? capabilitiesForRole(role) : undefined),
    roles: roles ?? (role ? [...servedRoleChain] : undefined),
  }
}
