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
  | 'running'
  | 'done'
  | 'failed'

export type EscalationLevel = 'L0' | 'L1' | 'L2' | 'L3'

export interface GitHubLifecycle {
  task_id: string
  repository: string
  spec_version: number
  source: string
  source_issue_number?: number
  issue_number?: number
  issue_url?: string
  outcome?: 'created' | 'reused'
  state: 'queued' | 'retrying' | 'published' | 'failed'
  create_state: 'not_started' | 'reconciling' | 'confirmed'
  create_attempts: number
  reconcile_misses: number
  attempts: number
  last_error?: string
  created_at: string
  updated_at: string
}

export interface Task {
  id: string
  workspace: string
  source: string
  title: string
  body: string
  class: string
  level: EscalationLevel | ''
  // Legacy historical record (spec §21.31); behavior is governed by hold.
  mode?: string
  hold?: boolean
  spec_approval: boolean
  merge_approval: boolean
  policy_version: number
  setup: string
  setup_contract: ExecutionSetup
  reviewed_head_sha?: string
  approved_head_sha?: string
  approval_stale?: boolean
  refresh_baseline_sha?: string
  refresh_head_sha?: string
  refresh_review_scope?: 'delta' | 'full' | 'none'
  repo: string
  base_branch: string
  branch: string
  state: TaskState
  next_stage?: Stage
  recovery_stage?: Stage
  parent_task_id?: string
  feature_id?: string
  github?: GitHubLifecycle
  created_at: string
}

export interface Job {
  id: string
  task_id: string
  stage: Stage
  harness: string
  model_tier: string
  auth_mode?: string
  runner: string
  pack_version?: string
  confinement: string
  cost_usd?: number | null
  tokens_in: number
  tokens_out: number
  state: JobState
  started_at?: string
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

export type InterventionAction = 'approve' | 'reject' | 'redirect' | 'pull_to_local' | 'cancel'

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
  review_diagnostics?: ReviewVerdictDiagnostic[]
  review_recovery?: ReviewRecoveryState
  interrupted_review_recovery?: InterruptedReviewRecoveryState
	stalled?: StalledState
}

export interface StalledState {
	needed: boolean
	reason: string
	work_order: WorkOrder
	last_failure?: string
}

export interface ReviewRecoveryState {
  needed: boolean
  prior_round: number
  reason: string
  timed_out_orders: WorkOrder[]
}

export interface ReviewRoundRetryResult {
  request_id: string
  task_id: string
  prior_round: number
  new_round: number
  pr_head: string
  work_orders: WorkOrder[]
}

export interface InterruptedReviewRecoveryState {
  needed: boolean
  review_round: number
  reason: string
  eligible_orders: WorkOrder[]
  retained_orders: WorkOrder[]
}

export interface InterruptedReviewRecoveryResult {
  request_id: string
  task_id: string
  review_round: number
  recovered_orders: WorkOrder[]
  retained_orders: WorkOrder[]
}

export interface ReviewVerdictDiagnostic {
  status: 'claimed_without_verdict' | 'expired_without_verdict'
  work_order_id: string
  review_round?: number
  review_seat?: number
  claimed_at?: string
  lease_expires_at?: string
  reason: string
}

// Unauthenticated display snapshot (GET /v1/workspace). The authenticated
// config document below is the mutable Postgres-backed source of truth.
export interface WorkspaceRepo {
  name: string
  url: string
  github?: string
  base: string
}

export interface WorkspaceRoute {
  stage: string
  model: string
  timeout: string
  execution: 'in_process' | 'mcp'
}

export interface WorkspaceInfo {
  workspace: string
  max_bounces: number
  database: string
  repos: WorkspaceRepo[] | null
  routing: WorkspaceRoute[] | null
  setups: ExecutionSetup[] | null
  default_setup: string
}

export interface WorkspaceRecord {
  id: string
  name: string
  config_version: number
  created_at: string
}

export interface WorkspaceConfigRepo {
  name: string
  url: string
  github?: string
  base: string
}

export interface WorkspaceConfigRoute {
  model: string
  model_policy?: 'explicit' | 'harness_default'
  effort?: 'low' | 'medium' | 'high'
  timeout: string
  execution: 'in_process' | 'mcp'
  harness?: string
}

export interface WorkspaceHarness {
  name: string
  mcp_transport: 'json_file' | 'toml_override' | 'environment'
  mcp_attachment?: string
  command: string[]
  model_args?: string[]
  default_model_sentinels?: string[]
  effort_args?: Partial<Record<'low' | 'medium' | 'high', string[]>>
  probe_command: string[]
  probe_timeout: string
}

export interface HarnessTemplate {
  id: string
  label: string
  description: string
  harness: WorkspaceHarness
}

export interface WorkspaceReviewSeat {
  model: string
  harness?: string
  effort?: 'low' | 'medium' | 'high'
}

export interface ExecutionPolicy {
  spec_approval: boolean
  merge_approval: boolean
  implement_concurrency: number
  review_concurrency: number
}

export interface WorkspaceExecutionSettings {
    control_plane: {
      triage: { model: string; effort?: 'minimal' | 'low' | 'medium' | 'high'; timeout: string }
    }
    spec: {
      harness: string
      model?: string
      model_policy: 'explicit' | 'harness_default'
      effort?: 'low' | 'medium' | 'high'
      timeout: string
    }
    implementation: {
      harness: string
      model?: string
      model_policy: 'explicit' | 'harness_default'
	  effort?: 'low' | 'medium' | 'high'
      timeout: string
    }
    review: {
      execution: 'in_process' | 'mcp'
      timeout: string
      fallback_model?: string
      fallback_harness?: string
    }
}

export interface ExecutionSetup {
  name: string
  execution_settings: WorkspaceExecutionSettings
  review: { seats: WorkspaceReviewSeat[] }
  refresh_review: 'delta' | 'full' | 'none'
}

export interface WorkspaceConfigDocument {
  workspace: string
  max_bounces: number
  work_order_queue_timeout: string
  execution_settings: WorkspaceExecutionSettings
  routing: {
    stages: Record<string, WorkspaceConfigRoute>
  }
  harnesses: WorkspaceHarness[]
  review: { seats: WorkspaceReviewSeat[] }
  setups: ExecutionSetup[]
  default_setup: string
  execution: ExecutionPolicy
  repos: WorkspaceConfigRepo[]
}

export interface HarnessProbe { harness: string; healthy: boolean; message?: string; checked_at: string }
export interface Worker { id: string; workspace: string; name: string; lease_expires_at?: string; last_seen_at?: string; revoked_at?: string; probes: HarnessProbe[]; created_at: string }
export interface HarnessModelFailure { harness: string; model: string; detail: string; work_order_id: string; observed_at: string }
export interface SetupServiceability { auto_available: boolean; auto_unavailable_reason?: string; model_failures?: HarnessModelFailure[] }
export interface WorkerList { workers: Worker[]; auto_available: boolean; auto_unavailable_reason?: string; setup_serviceability?: Record<string, SetupServiceability> }
export interface TaskWorkerStatus { available: boolean; required_harnesses: string[]; reason: string; last_heartbeat_at?: string; last_heartbeat_age?: string; queue_context: 'never_started' | 'interrupted' }

export interface VersionedWorkspaceConfig {
  document: WorkspaceConfigDocument
  version: number
}

export interface WorkspaceConfigReceipt extends VersionedWorkspaceConfig {
  event_id: number
  actor_id: string
  sections: string[]
}

export interface ActivityItem {
  task: Task
  jobs: Job[]
  events: TaskEvent[]
  interventions: Intervention[]
  checkout_command?: string
  checkout_available: boolean
  checkout_guidance: string
  needs_attention: boolean
  spec?: SpecVersion
  attachments?: Artifact[]
  work_orders: WorkOrder[]
  review_diagnostics?: ReviewVerdictDiagnostic[]
  review_recovery?: ReviewRecoveryState
  interrupted_review_recovery?: InterruptedReviewRecoveryState
	stalled?: StalledState
  worker_status?: TaskWorkerStatus
  merge_readiness?: { state: 'MERGEABLE' | 'UNKNOWN' | 'CONFLICTING' | 'STALE'; head_sha?: string; url?: string; number?: number }
}

export interface WorkOrder {
  id: string
  task_id: string
  job_id: string
  stage: 'spec' | 'implement' | 'review'
  state: 'queued' | 'claimed' | 'submitted' | 'completed' | 'cancelled' | 'stale' | 'timed_out'
  claimable: boolean
  claimed_by?: string
  session_id?: string
  agent?: string
  model?: string
  worker_id?: string
  review_round?: number
  review_seat?: number
  reason_code?: string
  review_kind?: 'refresh'
  review_scope?: 'delta' | 'full'
  baseline_sha?: string
  head_sha?: string
  required_model?: string
  required_harness?: string
  required_effort?: 'low' | 'medium' | 'high'
  required_harness_config?: WorkspaceHarness
  execution_timeout?: string
  model_enforcement?: 'worker-pinned' | 'self-reported'
  lease_expires_at?: string
  queue_entered_at: string
  queue_deadline: string
  execution_started_at?: string
  execution_deadline?: string
  last_attempt_outcome?: 'child_failure' | 'released' | 'cancelled' | 'expired'
  last_failure_message?: string
  last_failure_detail?: string
  last_failure_exit_status?: number
  last_failure_at?: string
  automatic_retry_count?: number
  next_retry_at?: string
  retry_suppressed?: boolean
  retry_suppression_reason?: string
  redispatch_count: number
  progress?: string
  cost_usd: number
  tokens_in: number
  tokens_out: number
  self_reported: boolean
}

export interface Feature { id: string; workspace: string; parent_id?: string; name: string; description?: string; created_at: string }
export interface Artifact { id: string; workspace: string; name: string; content_type: string; size_bytes: number; task_id?: string; feature_id?: string; download_url?: string; created_at: string }
export interface RequirementNode { feature: Feature; tasks: Task[] | null; approved_specs: SpecVersion[] | null; events: TaskEvent[] | null }
