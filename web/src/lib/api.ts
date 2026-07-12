import type { ActivityItem, ActivitySummary, EscalationLevel, InterventionAction, Task, WorkspaceInfo } from './types'

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
