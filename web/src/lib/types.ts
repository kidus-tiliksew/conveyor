// Wire types mirroring internal/core/types.go and the httpapi payloads
// (spec §16, §17.3). Keep field names in lockstep with the Go JSON tags.

export type TaskState =
  | 'claiming'
  | 'queued'
  | 'running'
  | 'awaiting_human'
  | 'approved'
  | 'merged'
  | 'closed'
  | 'parked'

export type Stage =
  | 'triage'
  | 'spec'
  | 'implement'
  | 'review'
  | 'verify'
  | 'gate'
  | 'merge'
  | 'monitor'

export type JobState =
  | 'pending'
  | 'booting'
  | 'running'
  | 'paused'
  | 'done'
  | 'failed'
  | 'sandbox_boot_failed'

export type EscalationLevel = 'L0' | 'L1' | 'L2' | 'L3'

export interface Task {
  id: string
  workspace: string
  source: string
  title: string
  body: string
  class: string
  level: EscalationLevel | ''
  repo: string
  base_branch: string
  branch: string
  state: TaskState
  next_stage?: Stage
  recovery_stage?: Stage
  parent_task_id?: string
  created_at: string
}

export interface BootDiagnostics {
  image_build_log?: string
  validation_error?: string
  runtime_error?: string
  missing_env_vars?: string[]
}

export interface Job {
  id: string
  task_id: string
  stage: Stage
  harness: string
  model_tier: string
  credential_id?: string
  auth_mode?: string
  runner: string
  sandbox_ref?: string
  pack_version?: string
  confinement: string
  budget_usd: number
  cost_usd: number
  tokens_in: number
  tokens_out: number
  state: JobState
  boot_diagnostics?: BootDiagnostics
  started_at: string
  ended_at?: string
}

export interface TaskEvent {
  id: number
  task_id: string
  job_id?: string
  kind: string
  actor_id: string
  actor_role: 'system' | 'human' | 'agent' | 'runner'
  payload: Record<string, unknown> | null
  at: string
}

export type InterventionAction = 'approve' | 'reject' | 'redirect' | 'pull_to_local'

export interface Intervention {
  id: number
  task_id: string
  job_id?: string
  actor_id: string
  actor_role: 'system' | 'human' | 'agent' | 'runner'
  action: InterventionAction
  reason_code: string
  comment: string
  at: string
}

// §4.1 machine-owned blocks, validated by internal/pipeline/output.go.
export interface AcceptanceCriterion {
  id: string
  criterion: string
  verify: 'test' | 'playwright' | 'computer-use' | 'human'
  ref?: string
}

export interface DecompositionItem {
  id: string
  repo: string
  summary: string
  depends_on: string[]
}

export interface SpecVersion {
  task_id: string
  version: number
  content: string
  acceptance_count: number
  acceptance: AcceptanceCriterion[] | null
  decomposition: DecompositionItem[] | null
  approved: boolean
  created_at: string
  approved_at?: string
}

export interface ActivitySummary {
  task: Task
  latest_stage?: Stage
  last_event_at: string
  needs_attention: boolean
}

// Read-only workspace snapshot (GET /v1/workspace) — conveyor.yaml as the
// dashboard sees it. Mutation stays with the operator-owned file (spec §2.1).
export interface WorkspaceRepo {
  name: string
  url: string
  github?: string
  base: string
  image: string
  secret_ref_count: number
  allowed_commands?: string[][]
  denied_commands?: string[][]
}

export interface WorkspaceRoute {
  stage: string
  harnesses: string[]
  model_tier?: string
  budget_usd: number
  timeout: string
}

export interface WorkspaceCredential {
  id: string
  owner_id: string
  owner_kind: string
  kind: string
  vendor: string
  harness: string
}

export interface WorkspaceInfo {
  workspace: string
  image: string
  max_bounces: number
  database: string
  repos: WorkspaceRepo[] | null
  routing: WorkspaceRoute[] | null
  credentials: WorkspaceCredential[] | null
}

export interface ActivityItem {
  task: Task
  jobs: Job[]
  events: TaskEvent[]
  interventions: Intervention[]
  checkout_command: string
  needs_attention: boolean
  spec?: SpecVersion
}
