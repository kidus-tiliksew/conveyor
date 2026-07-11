import type { InterventionAction } from './contracts'

export type TaskState = 'claiming' | 'queued' | 'running' | 'awaiting_human' | 'approved' | 'merged' | 'closed' | 'parked'

export interface Task {
  id: string
  workspace: string
  source: string
  title: string
  body: string
  class: string
  level: string
  repo: string
  base_branch: string
  branch: string
  state: TaskState
  created_at: string
}

export interface Job {
  id: string
  task_id: string
  stage: string
  harness: string
  model_tier: string
  credential_id?: string
  auth_mode?: string
  runner: string
  confinement: string
  budget_usd: number
  cost_usd: number
  tokens_in: number
  tokens_out: number
  state: string
  started_at: string
  ended_at?: string
}

export interface Event {
  id: number
  task_id: string
  job_id?: string
  kind: string
  actor_id: string
  actor_role: string
  payload: Record<string, unknown>
  at: string
}

export interface Intervention {
  id: number
  task_id: string
  job_id?: string
  actor_id: string
  action: InterventionAction
  reason_code: string
  comment: string
  at: string
}

export interface ActivityItem {
  task: Task
  jobs: Job[]
  events: Event[]
  interventions: Intervention[]
  checkout_command: string
  needs_attention: boolean
}

export interface ActivitySummary {
  task: Task
  latest_stage?: string
  last_event_at: string
  needs_attention: boolean
}
