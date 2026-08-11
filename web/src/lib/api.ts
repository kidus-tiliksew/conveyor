import type {
  ActivityItem,
  ActivitySummary,
  Artifact,
  BlueprintView,
  HarnessTemplate,
  InterventionAction,
  LineageGraph,
  LineageNodeType,
  MonitorDriftOutcome,
  MonitorStatus,
  PlanningBundle,
  PlanningMessage,
  PlanningMessagePart,
  PlanningSession,
  PlanningSessionGoal,
  PendingProposalsResponse,
  RequirementDerivation,
  RequirementVersion,
  RequirementView,
  RepositoryDrift,
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

function authHeaders(): HeadersInit {
  const token = sessionStorage.getItem('conveyor-token') ?? ''
  return token ? { Authorization: `Bearer ${token}` } : {}
}

async function getJSON<T>(url: string): Promise<T> {
  const response = await fetch(url, { headers: authHeaders() })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<T>
}

// The Board sends the shared Tasks/Board filter family to the same store
// predicate the Tasks list uses (AC-2.4), so the two surfaces cannot narrow
// differently and neither one narrows a fully-loaded workspace in the browser.
export function fetchActivity(filter?: Record<string, string | string[] | undefined>) {
  const query = filterQuery(filter)
  return getJSON<ActivitySummary[]>(workspaceURL(`/v1/activity${query ? `?${query}` : ''}`))
}

export function fetchPendingProposals() {
  return getJSON<PendingProposalsResponse>(workspaceURL('/v1/pending-proposals'))
}

// List members repeat their parameter (`state=a&state=b`), the spelling the
// server's parseTaskFilter reads as a disjunction.
function filterQuery(filter?: Record<string, string | string[] | undefined>) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(filter ?? {})) {
    for (const entry of Array.isArray(value) ? value : value ? [value] : []) params.append(key, entry)
  }
  return params.toString()
}

export function fetchTaskActivity(taskId: string) {
  return getJSON<ActivityItem>(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/activity`))
}

export function fetchWorkspace() {
  return getJSON<WorkspaceInfo>(workspaceURL('/v1/workspace'))
}

export async function fetchWorkspaces(token: string) {
  if (!token) return [] as WorkspaceRecord[]
  const response = await fetch('/v1/workspaces', { headers: { Authorization: `Bearer ${token}` } })
  if (!response.ok) throw new Error(await response.text())
  return response.json() as Promise<WorkspaceRecord[]>
}

export interface CreateWorkspaceInput {
  id: string
  name: string
  document?: Partial<WorkspaceConfigDocument>
}

export async function createWorkspace(token: string, input: CreateWorkspaceInput) {
  const response = await fetch('/v1/workspaces', {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify(input),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<WorkspaceRecord>
}

export function fetchBlueprints() {
  return getJSON<BlueprintView[]>(workspaceURL('/v1/blueprints'))
}
export function fetchRequirements() {
  return getJSON<RequirementView[]>(workspaceURL('/v1/requirements'))
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
export async function confirmRequirementVersion(
  token: string,
  requirementId: string,
  version: number,
  expectedVersion: number,
) {
  const response = await fetch(
    workspaceURL(`/v1/requirements/${encodeURIComponent(requirementId)}/versions/${version}/confirm`),
    {
      method: 'POST',
      headers: { ...mutationHeaders(token), 'If-Match': `"${expectedVersion}"` },
    },
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
      message = 'This requirement changed while you were reviewing it. Refresh and choose the version again.'
    throw new Error(message)
  }
  return response.json() as Promise<{ requirement: RequirementView['requirement']; version: RequirementVersion }>
}

export async function acknowledgeRequirementStaleness(token: string, requirementId: string, signalId: string) {
  const response = await fetch(
    workspaceURL(
      `/v1/requirements/${encodeURIComponent(requirementId)}/staleness/${encodeURIComponent(signalId)}/acknowledge`,
    ),
    { method: 'POST', headers: mutationHeaders(token) },
  )
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json()
}

export async function createRequirementStalenessFollowUp(token: string, requirementId: string, signalId: string) {
  const response = await fetch(
    workspaceURL(
      `/v1/requirements/${encodeURIComponent(requirementId)}/staleness/${encodeURIComponent(signalId)}/follow-up`,
    ),
    { method: 'POST', headers: mutationHeaders(token) },
  )
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{ task: Task; created: boolean }>
}
export function fetchPlanningSessions() {
  return getJSON<PlanningSession[]>(workspaceURL('/v1/planning-sessions'))
}
export function fetchPlanningBundles() {
  return getJSON<PlanningBundle[]>(workspaceURL('/v1/planning-bundles'))
}
export async function decidePlanningBundle(token: string, id: string, decision: 'approve' | 'reject') {
  const response = await fetch(workspaceURL(`/v1/planning-bundles/${encodeURIComponent(id)}/${decision}`), {
    method: 'POST',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<PlanningBundle>
}
export function fetchPlanningSession(sessionId: string) {
  return getJSON<PlanningSession>(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}`))
}
export function fetchPlanningMessages(sessionId: string) {
  return getJSON<PlanningMessage[]>(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}/messages`))
}
export async function createPlanningSession(
  token: string,
  input: {
    requirement_context_id?: string
    system_design_context_id?: string
    model?: string
    goal?: PlanningSessionGoal
    promotion?: RequirementDerivation
  },
) {
  const response = await fetch(workspaceURL('/v1/planning-sessions'), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify(input),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<PlanningSession>
}
export async function abandonPlanningSession(token: string, sessionId: string, reason?: string) {
  const response = await fetch(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}/abandon`), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify({ reason: reason?.trim() || undefined }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<PlanningSession>
}
export async function streamPlanningMessage(
  token: string,
  sessionId: string,
  content: string,
  onPart: (part: PlanningMessagePart) => void,
  options: { signal?: AbortSignal; attachments?: Artifact[] } = {},
) {
  const response = await fetch(workspaceURL(`/v1/planning-sessions/${encodeURIComponent(sessionId)}/messages`), {
    method: 'POST',
    headers: mutationHeaders(token),
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
export async function resolveMonitorDrift(
  token: string,
  driftId: string,
  outcome: MonitorDriftOutcome,
  requirementId?: string,
) {
  const response = await fetch(workspaceURL(`/v1/monitor/drift/${encodeURIComponent(driftId)}/resolve`), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify({ outcome, ...(requirementId ? { requirement_id: requirementId } : {}) }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<RepositoryDrift>
}
export function fetchTasks() {
  return getJSON<Task[]>(workspaceURL('/v1/tasks'))
}
export async function updateTaskContext(
  token: string,
  taskId: string,
  change: { add: { requirement_ids?: string[]; system_design_ids?: string[] }; remove: Record<string, never> },
) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/context`), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify(change),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<import('./types').TaskContext>
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
  const response = await fetch(workspaceURL(`/v1/task-operations?${query}`), { headers: authHeaders() })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  const items = (await response.json()) as TaskOperationsItem[]
  return {
    items,
    total: Number(response.headers.get('X-Conveyor-Total') ?? items.length),
    limit: Number(response.headers.get('X-Conveyor-Limit') ?? input.limit),
    offset: Number(response.headers.get('X-Conveyor-Offset') ?? input.offset),
  } satisfies TaskOperationsPage
}
export async function fetchArtifacts(token: string) {
  const response = await fetch(workspaceURL('/v1/artifacts'), { headers: { Authorization: `Bearer ${token}` } })
  if (!response.ok) throw new Error(await response.text())
  return response.json() as Promise<Artifact[]>
}
export async function uploadArtifact(
  token: string,
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
    headers: { Authorization: `Bearer ${token}` },
    body,
  })
  if (!response.ok) throw new Error(await response.text())
  return response.json() as Promise<Artifact>
}

export function fetchReferenceDocuments() {
  return getJSON<import('./types').ReferenceDocument[]>(workspaceURL('/v1/reference-documents'))
}
export function fetchSystemDesigns() {
  return getJSON<import('./types').SystemDesignView[]>(workspaceURL('/v1/system-designs'))
}
export function fetchSystemDesign(id: string) {
  return getJSON<import('./types').SystemDesignView>(workspaceURL(`/v1/system-designs/${encodeURIComponent(id)}`))
}
export function fetchSystemDesignVersions(id: string) {
  return getJSON<import('./types').SystemDesignVersion[]>(
    workspaceURL(`/v1/system-designs/${encodeURIComponent(id)}/versions`),
  )
}
export async function confirmSystemDesignVersion(token: string, id: string, version: number, expected: number) {
  const response = await fetch(
    workspaceURL(`/v1/system-designs/${encodeURIComponent(id)}/versions/${version}/confirm`),
    { method: 'POST', headers: { ...mutationHeaders(token), 'If-Match': `"${expected}"` } },
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
export async function resolveDecision(token: string, id: string, action: 'confirm' | 'dismiss') {
  const response = await fetch(workspaceURL(`/v1/decisions/${encodeURIComponent(id)}/${action}`), {
    method: 'POST',
    headers: mutationHeaders(token),
  })
  if (!response.ok) {
    const message = (await response.text()).trim() || response.statusText
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
export async function uploadReferenceDocument(token: string, file: File, id?: string) {
  const body = new FormData()
  body.set('file', file)
  if (!id) body.set('name', file.name.replace(/\.(md|markdown)$/i, ''))
  const path = id ? `/v1/reference-documents/${encodeURIComponent(id)}/versions` : '/v1/reference-documents'
  const response = await fetch(workspaceURL(path), { method: 'POST', headers: mutationHeaders(token), body })
  if (!response.ok) throw new Error(await response.text())
  return response.json()
}
export async function deleteReferenceDocument(token: string, id: string) {
  const response = await fetch(workspaceURL(`/v1/reference-documents/${encodeURIComponent(id)}`), {
    method: 'DELETE',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error(await response.text())
}
// Fetch an attachment's bytes as an object URL for inline preview. The
// download route requires the operator token and forces attachment
// disposition, so an <img src> cannot load it directly — the caller revokes
// the returned URL when the preview unmounts.
export async function fetchArtifactObjectURL(token: string, artifact: Artifact) {
  const response = await fetch(
    workspaceURL(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`),
    { headers: token ? { Authorization: `Bearer ${token}` } : {} },
  )
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return URL.createObjectURL(await response.blob())
}
export async function downloadArtifact(token: string, artifact: Artifact) {
  const response = await fetch(
    workspaceURL(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`),
    { headers: { Authorization: `Bearer ${token}` } },
  )
  if (!response.ok) throw new Error(await response.text())
  const blob = await response.blob()
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = artifact.name
  anchor.click()
  URL.revokeObjectURL(url)
}

export function fetchWorkspaceConfig(token: string) {
  return fetch(workspaceURL('/v1/workspace/config'), { headers: mutationHeaders(token) }).then(async (response) => {
    if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
    const result = (await response.json()) as VersionedWorkspaceConfig
    const fallbackReview = {
      seats: [
        {
          model: result.document.routing.stages.review?.model ?? '',
          harness: result.document.routing.stages.review?.harness,
        },
      ],
    }
    const review = { ...result.document.review, seats: result.document.review?.seats ?? fallbackReview.seats }
    const setups = (
      result.document.setups?.length
        ? result.document.setups
        : [
            {
              name: 'default',
              execution_settings: result.document.execution_settings,
              review,
              refresh_review: 'delta' as const,
            },
          ]
    ).map((setup) => ({
      ...setup,
      review: { ...setup.review, seats: setup.review?.seats ?? review.seats },
      refresh_review: setup.refresh_review || ('delta' as const),
    }))
    const planningModels = [
      ...new Set((result.document.planning_models ?? []).map((model) => model.trim()).filter(Boolean)),
    ]
    return {
      ...result,
      document: {
        ...result.document,
        execution: {
          ...result.document.execution,
          require_verification_evidence: result.document.execution?.require_verification_evidence ?? false,
        },
        harnesses: result.document.harnesses ?? [],
        repos: result.document.repos ?? [],
        monitor: result.document.monitor ?? {
          enabled: false,
          repositories: [],
          poll_interval: '1m',
          startup_window: '24h',
        },
        review,
        setups,
        default_setup: result.document.default_setup || setups[0].name,
        planning_models: planningModels.length ? planningModels : [],
      },
    }
  })
}

export async function getHarnessTemplates(token: string) {
  const response = await fetch('/v1/harness-templates', { headers: mutationHeaders(token) })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{ templates: HarnessTemplate[] }>
}

export class ConfigValidationError extends Error {
  fields: Array<{ field: string; message: string }>

  constructor(message: string, fields: Array<{ field: string; message: string }> = []) {
    super(message)
    this.fields = fields
  }
}

export async function updateWorkspaceConfig(token: string, document: WorkspaceConfigDocument, version: number) {
  const response = await fetch(workspaceURL('/v1/workspace/config'), {
    method: 'PUT',
    headers: { ...mutationHeaders(token), 'If-Match': String(version) },
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

function mutationHeaders(token: string) {
  return {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${token}`,
  }
}

export interface CreateTaskInput {
  body: string
  repo: string
  base_branch?: string
  hold?: boolean
  spec_approval?: boolean
  merge_approval?: boolean
  setup?: string
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

export async function fetchWorkers(token: string) {
  const response = await fetch(workspaceURL('/v1/workers'), { headers: mutationHeaders(token) })
  if (!response.ok) throw new Error(await response.text())
  const result = (await response.json()) as WorkerList
  return { ...result, workers: (result.workers ?? []).map((worker) => ({ ...worker, probes: worker.probes ?? [] })) }
}
export async function issueWorkerPairing(token: string) {
  const response = await fetch(workspaceURL('/v1/workers/pairings'), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify({ ttl_seconds: 600 }),
  })
  if (!response.ok) throw new Error(await response.text())
  return response.json() as Promise<{ pairing_token: string; expires_at: string }>
}
export async function revokeWorker(token: string, id: string) {
  const response = await fetch(workspaceURL(`/v1/workers/${encodeURIComponent(id)}`), {
    method: 'DELETE',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error(await response.text())
}

export async function createTask(token: string, input: CreateTaskInput, attachments: File[] = [], idempotencyKey = '') {
  const body = new FormData()
  body.set('task', JSON.stringify({ ...input, source: 'dashboard' }))
  if (idempotencyKey) body.set('idempotency_key', idempotencyKey)
  for (const file of attachments) body.append('attachments', file)
  const response = await fetch(workspaceURL('/v1/tasks'), {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}` },
    body,
  })
  if (!response.ok) {
    const contentType = response.headers.get('Content-Type') ?? ''
    const raw = await response.text()
    let payload: { error?: string; message?: string } | undefined
    if (contentType.includes('application/json')) {
      try {
        payload = JSON.parse(raw) as { error?: string; message?: string }
      } catch {
        // Preserve the response body as the fallback for malformed envelopes.
      }
    }
    const message = payload?.message?.trim() || raw.trim() || response.statusText
    const code = payload?.error === 'invalid_dependencies' ? 'invalid_dependencies' : 'request_failed'
    throw new TaskIntakeError(message, code)
  }
  return response.json() as Promise<Task>
}

export async function removeTaskDependency(
  taskId: string,
  dependencyId: string,
  token: string,
  reason: string,
  requestId: string,
) {
  const response = await fetch(
    workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/dependencies/${encodeURIComponent(dependencyId)}`),
    {
      method: 'DELETE',
      headers: mutationHeaders(token),
      body: JSON.stringify({ reason, request_id: requestId }),
    },
  )
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{ task: Task; request_id: string; removed: boolean }>
}

export async function redispatchTask(taskId: string, token: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/redispatch`), {
    method: 'POST',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

export async function cancelTask(taskId: string, token: string, reason: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/close`), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify({ reason }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

// Toggle the per-task hold: while held, workers never claim
// the task's work orders; operator-attached agents may claim explicitly.
export async function setTaskHold(taskId: string, token: string, hold: boolean) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/hold`), {
    method: 'PUT',
    headers: mutationHeaders(token),
    body: JSON.stringify({ hold }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

export async function setTaskAssignee(taskId: string, token: string, assigneeUserId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/assignee`), {
    method: 'PUT',
    headers: mutationHeaders(token),
    body: JSON.stringify({ assignee_user_id: assigneeUserId }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

export async function changeTaskSetup(
  taskId: string,
  token: string,
  input: { setup?: string; apply_latest?: boolean; reason?: string; request_id: string },
) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/setup`), {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify(input),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{ task: Task; review_transition: string }>
}

export async function recoverWorkOrder(workOrderId: string, token: string, requestId: string, direction?: string) {
  const response = await fetch(workspaceURL(`/v1/work-orders/${encodeURIComponent(workOrderId)}/recover`), {
    method: 'POST',
    headers: { ...mutationHeaders(token), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ request_id: requestId, direction }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<WorkOrder>
}

export async function preemptWorkOrder(workOrderId: string, token: string, reason: string, requestId: string) {
  const response = await fetch(workspaceURL(`/v1/work-orders/${encodeURIComponent(workOrderId)}/preempt`), {
    method: 'POST',
    headers: { ...mutationHeaders(token), 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ reason, request_id: requestId }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{
    request_id: string
    work_order: WorkOrder
    revoked_attempt_id: string
    revoked_session_id: string
    revoked_worker_id: string
    grace_bound: string
  }>
}

export async function retryReviewRound(taskId: string, token: string, requestId: string, reason: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/review-round/retry`), {
    method: 'POST',
    headers: { ...mutationHeaders(token), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ request_id: requestId, reason }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<import('./types').ReviewRoundRetryResult>
}

export async function recoverInterruptedReviewRound(taskId: string, token: string, requestId: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/review-round/recover`), {
    method: 'POST',
    headers: { ...mutationHeaders(token), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
    body: JSON.stringify({ request_id: requestId }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<import('./types').InterruptedReviewRecoveryResult>
}

export interface ReviewInput {
  action: InterventionAction
  reasonCode: string
  comment: string
}

export async function reviewTask(taskId: string, token: string, input: ReviewInput) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/review`), {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ action: input.action, reason_code: input.reasonCode, comment: input.comment }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{
    task: Task
    checkout_command?: string
    checkout_available: boolean
    checkout_guidance: string
  }>
}

export async function mergeTask(taskId: string, token: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/merge`), {
    method: 'POST',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

export async function fixMergeConflict(taskId: string, token: string) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/merge-conflict-fix`), {
    method: 'POST',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<import('./types').WorkOrder>
}
