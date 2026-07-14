import type {
  ActivityItem,
  ActivitySummary,
  EscalationLevel,
  InterventionAction,
  Task,
  VersionedWorkspaceConfig,
  WorkspaceConfigDocument,
  WorkspaceConfigReceipt,
  WorkspaceInfo,
  RequirementNode,
  Artifact,
} from './types'

async function getJSON<T>(url: string): Promise<T> {
  const response = await fetch(url)
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<T>
}

export function fetchActivity() {
  return getJSON<ActivitySummary[]>('/v1/activity')
}

export function fetchTaskActivity(taskId: string) {
  return getJSON<ActivityItem>(`/v1/tasks/${encodeURIComponent(taskId)}/activity`)
}

export function fetchWorkspace() {
  return getJSON<WorkspaceInfo>('/v1/workspace')
}

export function fetchRequirements() { return getJSON<RequirementNode[]>('/v1/requirements') }
export function fetchTasks() { return getJSON<Task[]>('/v1/tasks') }
export async function fetchArtifacts(token: string) { const response = await fetch('/v1/artifacts', { headers: { Authorization: `Bearer ${token}` } }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<Artifact[]> }
export async function uploadArtifact(token: string, file: File, taskId?: string, featureId?: string) { const body = new FormData(); body.set('file', file); if (taskId) body.set('task_id', taskId); if (featureId) body.set('feature_id', featureId); const response = await fetch('/v1/artifacts', { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'X-Conveyor-Actor': 'dashboard-operator' }, body }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<Artifact> }
export async function createFeature(token: string, input: { name: string; description: string; parent_id?: string }) { const response = await fetch('/v1/features', { method: 'POST', headers: mutationHeaders(token), body: JSON.stringify(input) }); if (!response.ok) throw new Error(await response.text()); return response.json() }
export async function assignTaskFeature(token: string, taskId: string, featureId: string) { const response = await fetch(`/v1/tasks/${encodeURIComponent(taskId)}/feature`, { method: 'PUT', headers: mutationHeaders(token), body: JSON.stringify({ feature_id: featureId }) }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<Task> }
export async function downloadArtifact(token: string, artifact: Artifact) { const response = await fetch(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`, { headers: { Authorization: `Bearer ${token}` } }); if (!response.ok) throw new Error(await response.text()); const blob = await response.blob(); const url = URL.createObjectURL(blob); const anchor = document.createElement('a'); anchor.href = url; anchor.download = artifact.name; anchor.click(); URL.revokeObjectURL(url) }

export function fetchWorkspaceConfig(token: string) {
  return fetch('/v1/workspace/config', { headers: mutationHeaders(token) }).then(async (response) => {
    if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
    return response.json() as Promise<VersionedWorkspaceConfig>
  })
}

export class ConfigValidationError extends Error {
  fields: Array<{ field: string; message: string }>

  constructor(message: string, fields: Array<{ field: string; message: string }> = []) {
    super(message)
    this.fields = fields
  }
}

export async function updateWorkspaceConfig(token: string, document: WorkspaceConfigDocument, version: number) {
  const response = await fetch('/v1/workspace/config', {
    method: 'PUT',
    headers: { ...mutationHeaders(token), 'If-Match': String(version) },
    body: JSON.stringify({ document }),
  })
  if (!response.ok) {
    const body = await response.json().catch(() => null) as { message?: string; fields?: Array<{ field: string; message: string }> } | null
    throw new ConfigValidationError(body?.message ?? body?.fields?.[0]?.message ?? response.statusText, body?.fields)
  }
  return response.json() as Promise<WorkspaceConfigReceipt>
}

function mutationHeaders(token: string) {
  return {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${token}`,
    'X-Conveyor-Actor': 'dashboard-operator',
  }
}

export interface CreateTaskInput {
  title: string
  body: string
  repo: string
  base_branch?: string
  level: EscalationLevel
}

export async function createTask(token: string, input: CreateTaskInput) {
  const response = await fetch('/v1/tasks', {
    method: 'POST',
    headers: mutationHeaders(token),
    body: JSON.stringify({ ...input, source: 'dashboard' }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

export async function redispatchTask(taskId: string, token: string) {
  const response = await fetch(`/v1/tasks/${encodeURIComponent(taskId)}/redispatch`, {
    method: 'POST',
    headers: mutationHeaders(token),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
}

export interface ReviewInput {
  action: InterventionAction
  reasonCode: string
  comment: string
}

export async function reviewTask(taskId: string, token: string, input: ReviewInput) {
  const response = await fetch(`/v1/tasks/${encodeURIComponent(taskId)}/review`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
      'X-Conveyor-Actor': 'dashboard-operator',
    },
    body: JSON.stringify({ action: input.action, reason_code: input.reasonCode, comment: input.comment }),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{ task: Task; checkout_command: string }>
}
