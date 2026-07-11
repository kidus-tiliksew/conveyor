import type { ActivityItem, ActivitySummary } from './lib/types'
import type { InterventionAction } from './lib/contracts'

export async function fetchActivity(): Promise<ActivitySummary[]> {
  const response = await fetch('/v1/activity')
  if (!response.ok) throw new Error(await response.text())
  return response.json() as Promise<ActivitySummary[]>
}

export async function fetchTaskActivity(taskId: string): Promise<ActivityItem> {
  const response = await fetch(`/v1/tasks/${encodeURIComponent(taskId)}/activity`)
  if (!response.ok) throw new Error(await response.text())
  return response.json() as Promise<ActivityItem>
}

export async function reviewTask(taskId: string, token: string, action: InterventionAction, reasonCode: string, comment: string) {
  const response = await fetch(`/v1/tasks/${encodeURIComponent(taskId)}/review`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
      'X-Conveyor-Actor': 'dashboard-operator',
    },
    body: JSON.stringify({ action, reason_code: reasonCode, comment }),
  })
  if (!response.ok) throw new Error((await response.text()).trim())
  return response.json()
}
