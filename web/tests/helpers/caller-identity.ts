// Mocked GET /v1/me projections in the shape the server returns for an
// authorized workspace read: the role, the role's capability list, and the
// ascending role chain (component-identity-membership). These are test
// fixtures only. The dashboard itself holds no role-to-capability table and
// renders whatever list the server serves; the Go HTTP tests in
// internal/httpapi/identity_me_capabilities_test.go prove the served lists
// against internal/core/authorization.go.

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

// Adds the served capability list and role chain to a scoped identity
// fixture, unless the fixture states its own lists explicitly.
export function callerIdentity<T extends { role?: string; capabilities?: string[]; roles?: string[] }>(identity: T) {
  return {
    ...identity,
    capabilities: identity.capabilities ?? (identity.role ? capabilitiesForRole(identity.role) : undefined),
    roles: identity.roles ?? (identity.role ? [...servedRoleChain] : undefined),
  }
}
