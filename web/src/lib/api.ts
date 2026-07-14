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
