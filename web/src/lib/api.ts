import type {
  ActivityItem,
  ActivityPage,
  ActivitySummary,
  Artifact,
  BlueprintView,
  InterventionAction,
  LineageGraph,
  LineageNodeType,
  MonitorDriftOutcome,
  MonitorStatus,
  PendingProposalsResponse,
  PlanningBundle,
  PlanningMessage,
  PlanningMessagePart,
  PlanningSession,
  PlanningSessionGoal,
  RepositoryDrift,
  RequirementDerivation,
  RequirementVersion,
  RequirementView,
  Task,
  TaskOperationsItem,
  TaskOperationsPage,
  VersionedWorkspaceConfig,
  WorkerList,
  WorkOrder,
  WorkspaceConfigDocument,
  WorkspaceConfigReceipt,
  WorkspaceInfo,
  WorkspaceRecord,
} from './types'

function workspaceURL(path: string) {
  const workspace = localStorage.getItem('conveyor-workspace') ?? ''
  if (!workspace) return path
  const separator = path.includes('?') ? '&' : '?'
  return `${path}${separator}workspace_id=${encodeURIComponent(workspace)}`
}

async function getJSON<T>(url: string): Promise<T> {
  const response = await fetch(url)
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<T>
}

// The Board sends the shared Tasks/Board filter family to the same store
// predicate the Tasks list uses (AC-2.4), so the two surfaces cannot narrow
// differently and neither one narrows a fully-loaded workspace in the browser.
async function fetchActivityPage(
  path: string,
  input: {
    limit: number
    offset: number
    filter?: Record<string, string | string[] | undefined>
    cursor?: string
    etag?: string
    previous?: ActivityPage
  },
) {
  const query = new URLSearchParams({ limit: String(input.limit), offset: String(input.offset) })
  if (input.cursor) query.set('since', input.cursor)
  for (const [key, value] of Object.entries(input.filter ?? {})) {
    for (const entry of Array.isArray(value) ? value : value ? [value] : []) query.append(key, entry)
  }
  const headers = new Headers()
  if (input.etag) headers.set('If-None-Match', input.etag)
  const response = await fetch(workspaceURL(`${path}?${query}`), { headers })
  if (response.status === 304 && input.previous) return input.previous
  if (response.status === 400 && input.cursor) {
    return fetchActivityPage(path, { ...input, cursor: undefined, etag: undefined, previous: undefined })
  }
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  const incoming = (await response.json()) as ActivitySummary[]
  // A representation can change through a read-time projection even when no
  // task event advances its marker. The ETag detects that case; retrying cold
  // prevents an empty delta from blessing stale summary data.
  if (input.cursor && input.previous && incoming.length === 0 && response.headers.get('ETag') !== input.etag) {
    return fetchActivityPage(path, { ...input, cursor: undefined, etag: undefined, previous: undefined })
  }
  const items =
    input.cursor && input.previous ? mergeActivity(input.previous.items, incoming).slice(0, input.limit) : incoming
  return {
    items,
    total: Number(response.headers.get('X-Conveyor-Total') ?? items.length),
    limit: Number(response.headers.get('X-Conveyor-Limit') ?? input.limit),
    offset: Number(response.headers.get('X-Conveyor-Offset') ?? input.offset),
    cursor: response.headers.get('X-Conveyor-Cursor') ?? input.cursor,
    etag: response.headers.get('ETag') ?? undefined,
  } satisfies ActivityPage
}

function mergeActivity(current: ActivitySummary[], incoming: ActivitySummary[]): ActivitySummary[] {
  const byID = new Map(current.map((item) => [item.task.id, item]))
  for (const item of incoming) byID.set(item.task.id, item)
  return [...byID.values()].sort((a, b) => {
    const created = new Date(b.task.created_at).getTime() - new Date(a.task.created_at).getTime()
    return created || a.task.id.localeCompare(b.task.id)
  })
}

export function fetchActivity(input: {
  limit: number
  offset: number
  filter?: Record<string, string | string[] | undefined>
  cursor?: string
  etag?: string
  previous?: ActivityPage
}) {
  return fetchActivityPage('/v1/activity', input)
}

export function fetchCallerAttentionTasks(input: { limit: number; offset: number }) {
  return fetchActivityPage('/v1/attention/tasks', input)
}

export function fetchPendingProposals() {
  return getJSON<PendingProposalsResponse>(workspaceURL('/v1/pending-proposals'))
}

export function fetchTaskActivity(taskId: string) {
  return getJSON<ActivityItem>(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/activity`))
}

export function fetchWorkspace() {
  return getJSON<WorkspaceInfo>(workspaceURL('/v1/workspace'))
}

export async function fetchWorkspaces() {
  const response = await fetch('/v1/workspaces')
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<WorkspaceRecord[]>
}

export async function redeemSignInLink(token: string) {
  const response = await fetch('/v1/sign-in/redeem', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{
    user: import('./types').CallerIdentity
    expires_at: string
    onboarding_required: true
  }>
}

export async function signInWithPassword(email: string, password: string) {
  const response = await fetch('/v1/sign-in/password', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Conveyor-CSRF': '1' },
    body: JSON.stringify({ email, password }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{ user: import('./types').CallerIdentity; expires_at: string }>
}

export async function updateOwnPassword(currentPassword: string, newPassword: string) {
  const response = await fetch('/v1/password', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Conveyor-CSRF': '1' },
    body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}

export async function fetchOwnProfile() {
  const response = await fetch('/v1/me')
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return (await response.json()) as import('./types').CallerIdentity
}

export async function updateOwnDisplayName(displayName: string) {
  const response = await fetch('/v1/me', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json', 'X-Conveyor-CSRF': '1' },
    body: JSON.stringify({ display_name: displayName }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return (await response.json()) as import('./types').CallerIdentity
}

export async function signOutDashboardSession() {
  const response = await fetch('/v1/sign-out', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Conveyor-CSRF': '1' },
  })
  if (!response.ok && response.status !== 400 && response.status !== 401) {
    throw new Error(apiErrorMessage(await response.text(), response.statusText))
  }
}

export interface CreateWorkspaceInput {
  id: string
  name: string
  document?: Partial<WorkspaceConfigDocument>
}

export async function createWorkspace(input: CreateWorkspaceInput) {
  const response = await fetch('/v1/workspaces', {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify(input),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<WorkspaceRecord>
}

// Membership routes carry the workspace in the path. resolveWorkspaceContext
// rejects a request whose path and query name different workspaces, so these
// URLs are built explicitly instead of going through workspaceURL().
function membersURL(workspace: string, suffix = '') {
  return `/v1/workspaces/${encodeURIComponent(workspace)}/members${suffix}`
}
function invitationsURL(workspace: string, suffix = '') {
  return `/v1/workspaces/${encodeURIComponent(workspace)}/invitations${suffix}`
}

export async function fetchWorkspaceMembers(workspace: string) {
  const response = await fetch(membersURL(workspace), { headers: mutationHeaders() })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return ((await response.json()) as import('./types').WorkspaceMembership[]) ?? []
}

// A caller without the membership-management capability is answered exactly
// like one addressing a workspace that does not exist. That is the deliberate
// server contract, so the distinct error type lets the surface fall back to a
// read-only member list instead of reporting a failure.
export class WorkspaceNotVisibleError extends Error {}

export async function fetchWorkspaceInvitations(workspace: string) {
  const response = await fetch(invitationsURL(workspace), { headers: mutationHeaders() })
  if (!response.ok) {
    const message = apiErrorMessage(await response.text(), response.statusText)
    if (response.status === 404) throw new WorkspaceNotVisibleError(message)
    throw new Error(message)
  }
  return ((await response.json()) as import('./types').WorkspaceInvitation[]) ?? []
}

// The workspace refuses to lose its last operator. That conflict is the one
// membership failure a person can act on, so it gets its own error type.
export class LastWorkspaceOperatorError extends Error {}

export async function inviteWorkspaceMember(
  workspace: string,
  input: { email: string; role: import('./types').WorkspaceRole },
) {
  const response = await fetch(membersURL(workspace), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify(input),
  })
  if (!response.ok) {
    const body = await response.text()
    const message = apiErrorMessage(body, response.statusText)
    if (response.status === 409 && membershipErrorCode(body) === 'last_workspace_operator') {
      throw new LastWorkspaceOperatorError(message)
    }
    throw new Error(message)
  }
  return response.json() as Promise<import('./types').MembershipGrant>
}

export async function revokeWorkspaceMember(workspace: string, userID: string) {
  const response = await fetch(membersURL(workspace, `/${encodeURIComponent(userID)}`), {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) {
    const message = apiErrorMessage(await response.text(), response.statusText)
    if (response.status === 409) throw new LastWorkspaceOperatorError(message)
    throw new Error(message)
  }
}

export async function revokeWorkspaceInvitation(workspace: string, email: string) {
  const response = await fetch(invitationsURL(workspace, `/${encodeURIComponent(email)}`), {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}

export async function resendWorkspaceInvitation(workspace: string, email: string) {
  const response = await fetch(invitationsURL(workspace, `/${encodeURIComponent(email)}/resend`), {
    method: 'POST',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').MembershipGrant>
}

// API failures arrive either as a JSON envelope or as plain text. Keeping the
// decoder here prevents mutation surfaces from rendering raw JSON to people.
function apiErrorMessage(body: string, fallback: string) {
  const text = body.trim()
  try {
    const parsed = JSON.parse(text) as { message?: string; fields?: Array<{ message?: string }> }
    return parsed.message || parsed.fields?.[0]?.message || text || fallback
  } catch {
    return text || fallback
  }
}

function membershipErrorCode(body: string) {
  try {
    return (JSON.parse(body) as { error?: string }).error
  } catch {
    return undefined
  }
}

// Who the caller is. The workspace travels with the request so the response
// carries the caller's role in it — the one surface that answers "may I do
// operator things here?" without the browser guessing (REQ-2, DEC-19).
export async function fetchCallerIdentity() {
  const response = await fetch(workspaceURL('/v1/me'), { headers: mutationHeaders() })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return (await response.json()) as import('./types').CallerIdentity
}

export async function fetchPersonalAccessTokens() {
  const response = await fetch('/v1/tokens', { headers: mutationHeaders() })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return ((await response.json()) as import('./types').PersonalAccessToken[]) ?? []
}

// The response of this call is the only place the token value ever exists.
export async function issuePersonalAccessToken(label: string) {
  const response = await fetch('/v1/tokens', {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ label }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').IssuedPersonalAccessToken>
}

export async function revokePersonalAccessToken(id: string) {
  const response = await fetch(`/v1/tokens/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}

export async function fetchForgeToken() {
  const response = await fetch('/v1/forge-token', { headers: mutationHeaders() })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').ForgeTokenStatus>
}

// The credential is accepted only as a write body. The returned type cannot
// represent it, preserving the write-only browser contract from AC-1.2.
export async function storeForgeToken(forgeToken: string) {
  const response = await fetch('/v1/forge-token', {
    method: 'PUT',
    headers: mutationHeaders(),
    body: JSON.stringify({ token: forgeToken }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').ForgeTokenStatus>
}

export async function deleteForgeToken() {
  const response = await fetch('/v1/forge-token', {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}

export async function fetchWorkspaceForgeToken(workspace: string) {
  const response = await fetch(`/v1/workspaces/${encodeURIComponent(workspace)}/forge-token`, {
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').WorkspaceForgeTokenStatus>
}

// The workspace credential is write-only in browser code. The response carries
// the same metadata-only shape used by the personal token surface.
export async function storeWorkspaceForgeToken(workspace: string, forgeToken: string) {
  const response = await fetch(`/v1/workspaces/${encodeURIComponent(workspace)}/forge-token`, {
    method: 'PUT',
    headers: mutationHeaders(),
    body: JSON.stringify({ token: forgeToken }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').WorkspaceForgeTokenStatus>
}

export async function deleteWorkspaceForgeToken(workspace: string) {
  const response = await fetch(`/v1/workspaces/${encodeURIComponent(workspace)}/forge-token`, {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}

export function fetchBlueprints() {
  return getJSON<BlueprintView[]>(workspaceURL('/v1/blueprints'))
}
export function fetchRequirements() {
  return getJSON<import('./types').RequirementSummary[]>(workspaceURL('/v1/requirements'))
}
export function fetchRequirement(requirementId: string) {
  return getJSON<RequirementView>(workspaceURL(`/v1/requirements/${encodeURIComponent(requirementId)}`))
}
export function fetchRequirementVersions(requirementId: string) {
  return getJSON<RequirementVersion[]>(workspaceURL(`/v1/requirements/${encodeURIComponent(requirementId)}/versions`))
}
export function fetchCheckpointContextCandidates(requirementId: string) {
  return getJSON<import('./types').CheckpointContextCandidate[]>(
    workspaceURL(`/v1/requirements/${encodeURIComponent(requirementId)}/checkpoint-context-candidates`),
  )
}
export async function confirmRequirementVersion(requirementId: string, version: number, expectedVersion: number) {
  const response = await fetch(
    workspaceURL(`/v1/requirements/${encodeURIComponent(requirementId)}/versions/${version}/confirm`),
    {
      method: 'POST',
      headers: { ...mutationHeaders(), 'If-Match': `"${expectedVersion}"` },
    },
  )
  if (!response.ok) {
    const body = await response.text()
    let message = body.trim() || response.statusText
    let code = ''
    try {
      const parsed = JSON.parse(body) as { error?: string; message?: string }
      code = parsed.error ?? ''
      message = parsed.message ?? message
    } catch {
      /* plain-text API error */
    }
    if (response.status === 409 && code === 'requirement_version_superseded')
      message = 'This requirement version was superseded by a newer confirmed version and can no longer be confirmed.'
    else if (response.status === 409)
      message = 'This requirement changed while you were reviewing it. Refresh and choose the version again.'
    throw new Error(message)
  }
  return response.json() as Promise<{ requirement: RequirementView['requirement']; version: RequirementVersion }>
}

export async function acknowledgeRequirementStaleness(requirementId: string, signalId: string) {
  const response = await fetch(
    workspaceURL(
      `/v1/requirements/${encodeURIComponent(requirementId)}/staleness/${encodeURIComponent(signalId)}/acknowledge`,
    ),
    { method: 'POST', headers: mutationHeaders() },
  )
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json()
}

export async function createRequirementStalenessFollowUp(requirementId: string, signalId: string) {
  const response = await fetch(
    workspaceURL(
      `/v1/requirements/${encodeURIComponent(requirementId)}/staleness/${encodeURIComponent(signalId)}/follow-up`,
    ),
    { method: 'POST', headers: mutationHeaders() },
  )
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{ task: Task; created: boolean }>
}
export function fetchPlanningSessions() {
  return getJSON<PlanningSession[]>(workspaceURL('/v1/planning-sessions'))
}
export function fetchPlanningBundles() {
  return getJSON<PlanningBundle[]>(workspaceURL('/v1/planning-bundles'))
}
export async function decidePlanningBundle(id: string, decision: 'approve' | 'reject') {
  const response = await fetch(workspaceURL(`/v1/planning-bundles/${encodeURIComponent(id)}/${decision}`), {
    method: 'POST',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<PlanningBundle>
}
export function fetchPlanningSession(sessionId: string) {
  return getJSON<PlanningSession>(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}`))
}
export function fetchPlanningMessages(sessionId: string) {
  return getJSON<PlanningMessage[]>(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}/messages`))
}
export async function createPlanningSession(input: {
  requirement_context_id?: string
  system_design_context_id?: string
  goal?: PlanningSessionGoal
  promotion?: RequirementDerivation
}) {
  const response = await fetch(workspaceURL('/v1/planning-sessions'), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify(input),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<PlanningSession>
}
export async function abandonPlanningSession(sessionId: string, reason?: string) {
  const response = await fetch(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}/abandon`), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ reason: reason?.trim() || undefined }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<PlanningSession>
}
export async function streamPlanningMessage(
  sessionId: string,
  content: string,
  onPart: (part: PlanningMessagePart) => void,
  options: { signal?: AbortSignal; attachments?: Artifact[] } = {},
) {
  const response = await fetch(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}/messages`), {
    method: 'POST',
    headers: mutationHeaders(),
    signal: options.signal,
    body: JSON.stringify({
      message: {
        role: 'user',
        content,
        parts: [
          { type: 'text', text: content },
          ...(options.attachments ?? []).map((artifact) => ({
            type: 'file',
            artifactId: artifact.id,
            filename: artifact.name,
            mediaType: artifact.content_type,
            size: artifact.size_bytes,
          })),
        ],
      },
    }),
  })
  if (!response.ok) {
    const message = (await response.text()).trim()
    if (response.status === 409) throw new Error('A reply is already in progress for this session.')
    throw new Error(message || response.statusText)
  }
  if (!response.body) throw new Error('Planning response did not include a stream.')
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  const processFrame = (frame: string) => {
    const data = frame
      .split(/\r?\n/)
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trim())
      .join('\n')
    if (!data || data === '[DONE]') return
    let part: PlanningMessagePart
    try {
      part = JSON.parse(data) as PlanningMessagePart
    } catch {
      return
    }
    onPart(part)
    if (part.type === 'error')
      throw new Error(part.errorText || 'Planning stopped before the reply finished. You can retry.')
  }
  try {
    for (;;) {
      const { value, done } = await reader.read()
      buffer += decoder.decode(value, { stream: !done })
      const frames = buffer.split(/\r?\n\r?\n/)
      buffer = frames.pop() ?? ''
      for (const frame of frames) processFrame(frame)
      if (done) {
        if (buffer.trim()) processFrame(buffer)
        break
      }
    }
  } finally {
    try {
      await reader.cancel()
    } catch {
      /* the stream may already be closed */
    }
  }
}
export function fetchLifecycleDiagram() {
  return getJSON<{ mermaid: string }>(workspaceURL('/v1/lifecycle-diagram'))
}
// The canonical bounded lineage walk. It is the only source the
// related-records panel reads (AC-3.2): the panel groups what this returns and
// never derives a relationship of its own, so it inherits the server's
// traversal budget and its truncation report unchanged.
export function fetchLineage(type: LineageNodeType, id: string) {
  return getJSON<LineageGraph>(workspaceURL(`/v1/lineage/${encodeURIComponent(type)}/${encodeURIComponent(id)}`))
}
export function fetchMonitorStatus() {
  return getJSON<MonitorStatus>(workspaceURL('/v1/monitor'))
}
export async function resolveMonitorDrift(driftId: string, outcome: MonitorDriftOutcome, requirementId?: string) {
  const response = await fetch(workspaceURL(`/v1/monitor/drift/${encodeURIComponent(driftId)}/resolve`), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ outcome, ...(requirementId ? { requirement_id: requirementId } : {}) }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<RepositoryDrift>
}
export function fetchTasks() {
  return getJSON<Task[]>(workspaceURL('/v1/tasks'))
}
export async function updateTaskContext(
  taskId: string,
  change: { add: { requirement_ids?: string[]; system_design_ids?: string[] }; remove: Record<string, never> },
) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/context`), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify(change),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').TaskContext>
}
export async function resolveTaskContextProposal(
  taskId: string,
  targetKind: 'requirement' | 'system_design',
  targetId: string,
  action: 'confirm' | 'dismiss',
) {
  const response = await fetch(
    workspaceURL(
      `/v1/tasks/${encodeURIComponent(taskId)}/context/proposals/${encodeURIComponent(targetKind)}/${encodeURIComponent(targetId)}/${action}`,
    ),
    { method: 'POST', headers: mutationHeaders() },
  )
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').TaskContextProposal>
}
// The Tasks view's read-only projection: task state, relations,
// attached context, and plan status from one durable source.
// Every member of the shared filter family travels to the server (AC-2.3): the
// browser asks for one page of what already matches rather than for the
// workspace it then narrows.
export async function fetchTaskOperations(input: {
  limit: number
  offset: number
  filter?: Record<string, string | string[] | undefined>
}) {
  const query = new URLSearchParams({ limit: String(input.limit), offset: String(input.offset) })
  for (const [key, value] of Object.entries(input.filter ?? {})) {
    for (const entry of Array.isArray(value) ? value : value ? [value] : []) query.append(key, entry)
  }
  const response = await fetch(workspaceURL(`/v1/task-operations?${query}`))
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  const items = (await response.json()) as TaskOperationsItem[]
  return {
    items,
    total: Number(response.headers.get('X-Conveyor-Total') ?? items.length),
    limit: Number(response.headers.get('X-Conveyor-Limit') ?? input.limit),
    offset: Number(response.headers.get('X-Conveyor-Offset') ?? input.offset),
  } satisfies TaskOperationsPage
}
export async function fetchArtifacts() {
  const response = await fetch(workspaceURL('/v1/artifacts'))
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Artifact[]>
}
export async function uploadArtifact(
  file: File,
  taskId?: string,
  requirementId?: string,
  role?: Artifact['role'],
  planningSessionId?: string,
) {
  const body = new FormData()
  body.set('file', file)
  if (taskId) body.set('task_id', taskId)
  if (requirementId) body.set('requirement_id', requirementId)
  if (planningSessionId) body.set('planning_session_id', planningSessionId)
  if (role) body.set('role', role)
  const response = await fetch(workspaceURL('/v1/artifacts'), {
    method: 'POST',
    headers: mutationAuthHeaders(),
    body,
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Artifact>
}

export function fetchReferenceDocuments() {
  return getJSON<import('./types').ReferenceDocument[]>(workspaceURL('/v1/reference-documents'))
}
export function fetchSystemDesigns() {
  return getJSON<import('./types').SystemDesignSummary[]>(workspaceURL('/v1/system-designs'))
}
export function fetchSystemDesign(id: string) {
  return getJSON<import('./types').SystemDesignView>(workspaceURL(`/v1/system-designs/${encodeURIComponent(id)}`))
}
export function fetchSystemDesignVersions(id: string) {
  return getJSON<import('./types').SystemDesignVersion[]>(
    workspaceURL(`/v1/system-designs/${encodeURIComponent(id)}/versions`),
  )
}
export async function confirmSystemDesignVersion(id: string, version: number, expected: number) {
  const response = await fetch(
    workspaceURL(`/v1/system-designs/${encodeURIComponent(id)}/versions/${version}/confirm`),
    { method: 'POST', headers: { ...mutationHeaders(), 'If-Match': `"${expected}"` } },
  )
  if (!response.ok) {
    const body = await response.text()
    let message = body.trim() || response.statusText
    try {
      const parsed = JSON.parse(body) as { message?: string }
      message = parsed.message ?? message
    } catch {
      /* plain-text API error */
    }
    if (response.status === 409)
      throw new SystemDesignConflictError(
        'This design changed while you were reviewing it. The latest versions are loading; review them and try again.',
      )
    throw new Error(message)
  }
  return response.json()
}
export class SystemDesignConflictError extends Error {}
export function fetchDecisions() {
  return getJSON<import('./types').Decision[]>(workspaceURL('/v1/decisions'))
}
export async function resolveDecision(id: string, action: 'confirm' | 'dismiss') {
  const response = await fetch(workspaceURL(`/v1/decisions/${encodeURIComponent(id)}/${action}`), {
    method: 'POST',
    headers: mutationHeaders(),
  })
  if (!response.ok) {
    const message = apiErrorMessage(await response.text(), response.statusText)
    if (response.status === 409) throw new DecisionConflictError(message)
    throw new Error(message)
  }
  return response.json() as Promise<import('./types').Decision>
}
export class DecisionConflictError extends Error {}
export function fetchReferenceDocumentVersions(id: string) {
  return getJSON<import('./types').ReferenceDocumentVersion[]>(
    workspaceURL(`/v1/reference-documents/${encodeURIComponent(id)}/versions`),
  )
}
export async function uploadReferenceDocument(file: File, id?: string) {
  const body = new FormData()
  body.set('file', file)
  if (!id) body.set('name', file.name.replace(/\.(md|markdown)$/i, ''))
  const path = id ? `/v1/reference-documents/${encodeURIComponent(id)}/versions` : '/v1/reference-documents'
  const response = await fetch(workspaceURL(path), { method: 'POST', headers: mutationHeaders(), body })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json()
}
export async function deleteReferenceDocument(id: string) {
  const response = await fetch(workspaceURL(`/v1/reference-documents/${encodeURIComponent(id)}`), {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}
// Fetch an attachment's bytes as an object URL for inline preview. The caller
// revokes the returned URL when the preview unmounts.
export async function fetchArtifactObjectURL(artifact: Artifact) {
  const response = await fetch(
    workspaceURL(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`),
  )
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return URL.createObjectURL(await response.blob())
}
export async function downloadArtifact(artifact: Artifact) {
  const response = await fetch(
    workspaceURL(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`),
  )
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  const blob = await response.blob()
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = artifact.name
  anchor.click()
  URL.revokeObjectURL(url)
}

export function fetchWorkspaceConfig() {
  return fetch(workspaceURL('/v1/workspace/config'), { headers: mutationHeaders() }).then(async (response) => {
    if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
    const result = (await response.json()) as VersionedWorkspaceConfig
    return {
      ...result,
      document: {
        ...result.document,
        execution: {
          ...result.document.execution,
          require_verification_evidence: result.document.execution?.require_verification_evidence ?? false,
        },
        repos: result.document.repos ?? [],
        monitor: result.document.monitor ?? {
          enabled: false,
          repositories: [],
          poll_interval: '1m',
          startup_window: '24h',
        },
        review: { seats: result.document.review?.seats ?? [{}] },
        stage_timeouts: {
          spec: result.document.stage_timeouts?.spec ?? '30m',
          implement: result.document.stage_timeouts?.implement ?? '4h',
          review: result.document.stage_timeouts?.review ?? '1h',
        },
      },
    }
  })
}

export class ConfigValidationError extends Error {
  fields: Array<{ field: string; message: string }>

  constructor(message: string, fields: Array<{ field: string; message: string }> = []) {
    super(message)
    this.fields = fields
  }
}

export async function updateWorkspaceConfig(document: WorkspaceConfigDocument, version: number) {
  const response = await fetch(workspaceURL('/v1/workspace/config'), {
    method: 'PUT',
    headers: { ...mutationHeaders(), 'If-Match': String(version) },
    body: JSON.stringify({ document }),
  })
  if (!response.ok) {
    const body = (await response.json().catch(() => null)) as {
      message?: string
      fields?: Array<{ field: string; message: string }>
    } | null
    throw new ConfigValidationError(body?.message ?? body?.fields?.[0]?.message ?? response.statusText, body?.fields)
  }
  return response.json() as Promise<WorkspaceConfigReceipt>
}

function mutationHeaders(_legacyToken?: string) {
  return {
    'Content-Type': 'application/json',
    ...mutationAuthHeaders(),
  }
}

function mutationAuthHeaders(_legacyToken?: string) {
  return { 'X-Conveyor-CSRF': '1' }
}

export interface CreateTaskInput {
  body: string
  repo: string
  base_branch?: string
  hold?: boolean
  spec_approval?: boolean
  merge_approval?: boolean
  depends_on?: string[]
  requirement_ids?: string[]
  system_design_ids?: string[]
}

export class TaskIntakeError extends Error {
  code: 'invalid_dependencies' | 'request_failed'

  constructor(message: string, code: TaskIntakeError['code']) {
    super(message)
    this.code = code
  }
}

export async function fetchWorkers() {
  const response = await fetch(workspaceURL('/v1/workers'), { headers: mutationHeaders() })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  const result = (await response.json()) as WorkerList
  return { ...result, workers: (result.workers ?? []).map((worker) => ({ ...worker, probes: worker.probes ?? [] })) }
}
export async function issueWorkerPairing() {
  const response = await fetch(workspaceURL('/v1/workers/pairings'), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ ttl_seconds: 600 }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{ pairing_token: string; expires_at: string }>
}
export async function revokeWorker(id: string) {
  const response = await fetch(workspaceURL(`/v1/workers/${encodeURIComponent(id)}`), {
    method: 'DELETE',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
}

export async function createTask(input: CreateTaskInput, attachments: File[] = [], idempotencyKey = '') {
  const body = new FormData()
  body.set('task', JSON.stringify({ ...input, source: 'dashboard' }))
  if (idempotencyKey) body.set('idempotency_key', idempotencyKey)
  for (const file of attachments) body.append('attachments', file)
  const response = await fetch(workspaceURL('/v1/tasks'), {
    method: 'POST',
    headers: mutationAuthHeaders(),
    body,
  })
  if (!response.ok) {
    const raw = await response.text()
    const message = apiErrorMessage(raw, response.statusText)
    const code = membershipErrorCode(raw) === 'invalid_dependencies' ? 'invalid_dependencies' : 'request_failed'
    throw new TaskIntakeError(message, code)
  }
  return response.json() as Promise<Task>
}

export async function removeTaskDependency(taskId: string, dependencyId: string, reason: string, requestId: string) {
  const response = await fetch(
    workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/dependencies/${encodeURIComponent(dependencyId)}`),
    {
      method: 'DELETE',
      headers: mutationHeaders(),
      body: JSON.stringify({ reason, request_id: requestId }),
    },
  )
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{ task: Task; request_id: string; removed: boolean }>
}

export async function redispatchTask(taskId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/redispatch`), {
    method: 'POST',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Task>
}

export async function cancelTask(taskId: string, reason: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/close`), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ reason }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Task>
}

// Toggle the per-task hold: while held, workers never claim
// the task's work orders; operator-attached agents may claim explicitly.
export async function setTaskHold(taskId: string, hold: boolean) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/hold`), {
    method: 'PUT',
    headers: mutationHeaders(),
    body: JSON.stringify({ hold }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Task>
}

export async function setTaskAssignee(taskId: string, assigneeUserId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/assignee`), {
    method: 'PUT',
    headers: mutationHeaders(),
    body: JSON.stringify({ assignee_user_id: assigneeUserId }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Task>
}

export async function recoverWorkOrder(workOrderId: string, requestId: string, direction?: string) {
  const response = await fetch(workspaceURL(`/v1/work-orders/${encodeURIComponent(workOrderId)}/recover`), {
    method: 'POST',
    headers: { ...mutationHeaders(), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ request_id: requestId, direction }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<WorkOrder>
}

export async function preemptWorkOrder(workOrderId: string, reason: string, requestId: string) {
  const response = await fetch(workspaceURL(`/v1/work-orders/${encodeURIComponent(workOrderId)}/preempt`), {
    method: 'POST',
    headers: { ...mutationHeaders(), 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ reason, request_id: requestId }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{
    request_id: string
    work_order: WorkOrder
    revoked_attempt_id: string
    revoked_session_id: string
    revoked_worker_id: string
    grace_bound: string
  }>
}

export async function retryReviewRound(taskId: string, requestId: string, reason: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/review-round/retry`), {
    method: 'POST',
    headers: { ...mutationHeaders(), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ request_id: requestId, reason }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').ReviewRoundRetryResult>
}

export async function recoverInterruptedReviewRound(taskId: string, requestId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/review-round/recover`), {
    method: 'POST',
    headers: { ...mutationHeaders(), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ request_id: requestId }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').InterruptedReviewRecoveryResult>
}

export interface ReviewInput {
  action: InterventionAction
  reasonCode: string
  comment: string
}

export async function reviewTask(taskId: string, input: ReviewInput) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/review`), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ action: input.action, reason_code: input.reasonCode, comment: input.comment }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{
    task: Task
    checkout_command?: string
    checkout_available: boolean
    checkout_guidance: string
  }>
}

export async function requestTaskChanges(taskId: string, feedback: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/request-changes`), {
    method: 'POST',
    headers: mutationHeaders(),
    body: JSON.stringify({ feedback }),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<{ task: Task; feedback: string }>
}

export async function mergeTask(taskId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/merge`), {
    method: 'POST',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<Task>
}

export async function fixMergeConflict(taskId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/merge-conflict-fix`), {
    method: 'POST',
    headers: mutationHeaders(),
  })
  if (!response.ok) throw new Error(apiErrorMessage(await response.text(), response.statusText))
  return response.json() as Promise<import('./types').WorkOrder>
}
