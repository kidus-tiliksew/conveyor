// Wire types mirroring internal/core/types.go and the httpapi payloads
// Keep field names in lockstep with the Go JSON tags.

export type TaskState =
  | 'claiming'
  | 'queued'
  | 'running'
  | 'awaiting_human'
  | 'approved'
  | 'merged'
  | 'closed'
  | 'parked'

export type Stage = 'triage' | 'spec' | 'implement' | 'review' | 'verify' | 'gate' | 'merge' | 'monitor'

export type JobState = 'pending' | 'running' | 'done' | 'failed'

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
  // Legacy historical record; behavior is governed by hold.
  mode?: string
  hold?: boolean
  assignee?: TaskAssignee
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
  origin_spec_version?: number
  origin_sub_id?: string
  dependencies?: TaskRelation[]
  blocking_task_ids?: string[]
  children?: TaskRelation[]
  context?: TaskContext
  github?: GitHubLifecycle
  created_at: string
}

export interface TaskAssignee {
  user_id: string
  email?: string
  display_name?: string
}

export interface TaskContext {
  requirements?: Array<{ id: string; title: string; version: number }>
  designs?: Array<{ id: string; title: string; version: number }>
}

export interface CheckpointContextCandidate {
  id: string
  title: string
  state: TaskState
}

export interface TaskRelation {
  id: string
  title: string
  state: TaskState
  origin_spec_version?: number
  origin_sub_id?: string
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

export type LineageNodeType =
  | 'planning_session'
  | 'planning_bundle'
  | 'requirement'
  | 'requirement_version'
  | 'reference_document'
  | 'reference_document_version'
  | 'system_design'
  | 'system_design_version'
  | 'decision'
  | 'repository_path'
  | 'blueprint'
  | 'blueprint_version'
  | 'task'
  | 'work_order'
  | 'pull_request'
  | 'commit_range'
  | 'evidence'
  | 'verdict'

export interface LineageNode {
  type: LineageNodeType
  id: string
  label?: string
}
export interface LineageLink {
  workspace: string
  src_type: LineageNodeType
  src_id: string
  dst_type: LineageNodeType
  dst_id: string
  kind: string
  created_by_event_id: number
  created_at: string
}
export interface LineageGraph {
  roots: LineageNode[]
  nodes: LineageNode[]
  links: LineageLink[]
  truncated: boolean
  budget?: { max_depth: number; max_nodes: number; max_links?: number }
  omitted_nodes?: number
  omitted_links?: number
  exhaustion_reasons?: string[]
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
  materialized_children?: TaskRelation[]
  approved: boolean
  created_at: string
  approved_at?: string
}

export interface ActivitySummary {
  task: Task
  latest_stage?: Stage
  last_event_at: string
  needs_attention: boolean
  pending_authority: boolean
  forge_failure?: ForgeFailure
  review_diagnostics?: ReviewVerdictDiagnostic[]
  review_recovery?: ReviewRecoveryState
  interrupted_review_recovery?: InterruptedReviewRecoveryState
  stalled?: StalledState
}

// The four durable plan outcomes the Tasks view renders (AC-1.4). "none" is a
// task with no persisted plan version; the version is absent with it.
export type TaskPlanState = 'none' | 'pending_gate' | 'approved' | 'redirected'

export interface TaskPlanStatus {
  state: TaskPlanState
  version?: number
  legacy?: boolean
}

export interface TaskChildRollup {
  total: number
  merged: number
  closed: number
  open: number
}

// TaskOperationsItem is the list-first Tasks view's row.
// It carries no priority or declared-phase field. Assignment is part of the
// task projection because it constrains claim eligibility, never ordering.
export interface TaskOperationsItem {
  task: Task
  latest_stage?: Stage
  last_event_at: string
  stalled?: TaskStalledSummary
  needs_attention: boolean
  unsatisfiable_task_ids?: string[]
  child_rollup?: TaskChildRollup
  plan: TaskPlanStatus
}

export interface TaskOperationsPage {
  items: TaskOperationsItem[]
  total: number
  limit: number
  offset: number
}

// The list-scoped stalled projection: the reason a row cannot move, without
// the work order the detail surfaces render.
export interface TaskStalledSummary {
  needed: boolean
  reason: string
  last_failure?: string
}

export interface StalledState {
  needed: boolean
  reason: string
  work_order: WorkOrder
  last_failure?: string
  blocking_task_ids?: string[]
  unsatisfiable_edge?: boolean
}

export interface ForgeFailure {
  category: string
  detail: string
  surface: string
  at: string
}

export interface ReviewRecoveryState {
  needed: boolean
  prior_round: number
  reason: string
  timed_out_orders: WorkOrder[]
  inconsistent_orders: WorkOrder[]
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

export type WorkspaceRole = 'viewer' | 'executor' | 'contributor' | 'maintainer' | 'operator'

// The self-identity projection behind GET /v1/me: deliberately narrow, and
// carrying a role only when the request supplied authorized workspace context.
export interface CallerIdentity {
  id: string
  email: string
  display_name: string
  role?: WorkspaceRole
}

export interface WorkspaceMembership {
  workspace_id: string
  user_id: string
  email?: string
  display_name?: string
  role: WorkspaceRole
  created_at: string
}

export interface WorkspaceInvitation {
  workspace_id: string
  email: string
  role: WorkspaceRole
  invited_by: string
  invited_by_display_name?: string
  created_at: string
}

export interface MembershipGrant {
  email: string
  role: WorkspaceRole
  delivery?: 'sent' | 'fallback' | 'failed'
  sign_in_url?: string
}

export interface PersonalAccessToken {
  id: string
  user_id: string
  label: string
  last_used_at?: string
  revoked_at?: string
  created_at: string
}

// The value field belongs to this shape alone: issuance returns it once, and no
// listing response can be assigned to a type that carries it.
export interface IssuedPersonalAccessToken extends PersonalAccessToken {
  value: string
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
  stall_timeout?: string
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
  require_verification_evidence: boolean
  implement_concurrency: number
  review_concurrency: number
  first_activity_timeout: string
}

export interface WorkspaceExecutionSettings {
  control_plane: {
    triage: { model: string; effort?: 'minimal' | 'low' | 'medium' | 'high'; timeout: string }
    planning: {
      model: string
      effort?: 'minimal' | 'low' | 'medium' | 'high'
      timeout: string
      exploration_output_tokens: number
      context?: {
        depth: number
        nodes: number
        renderable_bytes: number
        artifact_refs?: number
        authority_nodes?: number
      }
    }
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
  planning_models: string[]
  monitor?: { enabled: boolean; repositories: string[]; poll_interval: string; startup_window: string }
}

export type MonitorSignalKind = 'post_merge_failure' | 'direct_push' | 'external_pr_merge' | 'revert'
export type MonitorDriftOutcome =
  | 'requirements_amended'
  | 'design_document_updated'
  | 'conflict_resolved'
  | 'change_reverted'
export interface MonitorObservation {
  workspace_id: string
  repository: string
  kind: MonitorSignalKind
  occurrence_id: string
  source_url: string
  commit_sha?: string
  task_id?: string
  task_outcome?: 'created' | 'reused'
  state: string
  deduplicated_count: number
  created_at: string
  updated_at: string
}
export interface RepositoryDrift {
  id: string
  workspace_id: string
  repository: string
  kind: MonitorSignalKind
  source_url: string
  commit_sha?: string
  requirement_id?: string
  task_id: string
  system_design_id?: string
  system_design_version?: number
  causal_event_id?: number
  matching_paths?: string[]
  detected_at: string
}
export interface MonitorStatus {
  workspace_id: string
  enabled: boolean
  last_successful_observation?: string
  current_error?: string
  forge_error_category?: string
  backoff_until?: string
  observations: MonitorObservation[]
  drift: RepositoryDrift[]
  drift_count: number
  oldest_drift_age: number
  activity: Array<{ id: number; workspace_id: string; kind: string; payload: Record<string, unknown>; at: string }>
}

export interface HarnessProbe {
  harness: string
  healthy: boolean
  message?: string
  checked_at: string
}
export interface Worker {
  id: string
  workspace: string
  name: string
  lease_expires_at?: string
  last_seen_at?: string
  revoked_at?: string
  probes: HarnessProbe[]
  created_at: string
}
export interface RateLimitStatus {
  status: string
  limit?: number
  remaining?: number
  reset_at?: string
}
export interface RateLimitHealth {
  work_order_id: string
  worker_id?: string
  harness: string
  model?: string
  rate_limit: RateLimitStatus
  observed_at: string
}
export interface HarnessModelFailure {
  harness: string
  model: string
  detail: string
  work_order_id: string
  observed_at: string
}
export interface SetupServiceability {
  auto_available: boolean
  auto_unavailable_reason?: string
  model_failures?: HarnessModelFailure[]
}
export interface WorkerList {
  workers: Worker[]
  auto_available: boolean
  auto_unavailable_reason?: string
  setup_serviceability?: Record<string, SetupServiceability>
  rate_limits?: RateLimitHealth[]
}
export interface TaskWorkerStatus {
  available: boolean
  required_harnesses: string[]
  reason: string
  last_heartbeat_at?: string
  last_heartbeat_age?: string
  queue_context: 'never_started' | 'interrupted'
}

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
  at_merge_gate: boolean
  pending_authority?: boolean
  forge_failure?: ForgeFailure
  spec?: SpecVersion
  attachments?: Artifact[]
  verification_evidence?: Artifact[]
  work_orders: WorkOrder[]
  review_diagnostics?: ReviewVerdictDiagnostic[]
  review_recovery?: ReviewRecoveryState
  interrupted_review_recovery?: InterruptedReviewRecoveryState
  stalled?: StalledState
  worker_status?: TaskWorkerStatus
  merge_readiness?: {
    state: 'MERGEABLE' | 'UNKNOWN' | 'CONFLICTING' | 'STALE'
    head_sha?: string
    url?: string
    number?: number
  }
}

export interface PendingProposal {
  id: string
  title: string
  tier: 'requirement' | 'system_design' | 'decision'
  version?: number
  origin_type: 'task' | 'session' | 'drift' | 'operator'
  origin_id?: string
  proposed_at: string
  age_seconds: number
}

export interface PendingProposalsResponse {
  items: PendingProposal[]
  attention: {
    task_count: number
    pending_proposal_count: number
    total: number
  }
}

export interface WorkOrder {
  id: string
  task_id: string
  job_id: string
  stage: 'spec' | 'implement' | 'review'
  state: 'queued' | 'claimed' | 'submitted' | 'completed' | 'cancelled' | 'stale' | 'timed_out'
  claimable: boolean
  blocking_task_ids?: string[]
  assignee?: TaskAssignee
  unsatisfiable_task_ids?: string[]
  claimed_by?: string
  session_id?: string
  attempt_id?: string
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
  last_attempt_id?: string
  last_attempt_outcome?: 'child_failure' | 'stalled' | 'released' | 'cancelled' | 'expired' | 'preempted'
  last_failure_category?: 'provider_usage_limit' | string
  last_failure_message?: string
  last_failure_detail?: string
  last_failure_exit_status?: number
  last_failure_at?: string
  automatic_retry_count?: number
  next_retry_at?: string
  retry_suppressed?: boolean
  retry_suppression_reason?: string
  redispatch_count: number
  operator_direction?: string
  progress?: string
  cost_usd: number
  tokens_in: number
  tokens_out: number
  usage_reported: boolean
  self_reported: boolean
  rate_limit?: RateLimitStatus
  rate_limit_observed_at?: string
  last_agent_activity_at?: string
  last_agent_activity_label?: string
  created_at?: string
  updated_at?: string
}

export interface Artifact {
  id: string
  workspace: string
  name: string
  content_type: string
  size_bytes: number
  role: 'task_context' | 'generated_audit' | 'generated_output' | 'verification_evidence'
  task_id?: string
  requirement_id?: string
  planning_session_id?: string
  download_url?: string
  created_at: string
}

export interface ReferenceDocument {
  id: string
  workspace: string
  name: string
  current_version: number
  created_at: string
  updated_at: string
}

export interface ReferenceDocumentVersion {
  document_id: string
  workspace: string
  version: number
  filename: string
  content_type: string
  content: string
  supersedes_version?: number
  created_by: string
  created_at: string
}

export interface RequirementStatement {
  id: string
  statement: string
  user_story?: { as_a: string; i_want: string; so_that: string }
  acceptance_criteria?: Array<{ id: string; statement: string }>
}

export interface Requirement {
  id: string
  slug: string
  title: string
  current_version?: number
  statement_high_water_mark: number
  workspace: string
  created_at: string
  updated_at: string
}

export interface RequirementVersion {
  requirement_id: string
  version: number
  content: string
  statements: RequirementStatement[]
  origin: 'chat' | 'drift_amendment' | 'feature_migration' | 'operator'
  origin_session_id?: string
  origin_drift_id?: string
  derived_from?: RequirementDerivation
  confirmed: boolean
  confirmed_by?: string
  confirmed_at?: string
  retired: boolean
  retired_by?: string
  retired_at?: string
  retired_by_version?: number
  workspace: string
  created_at: string
}

export interface RequirementDerivation {
  document_id: string
  version: number
  section_anchor: string
  target_id: string
}

/** The artifact a session declares it is working toward. */
export type PlanningSessionGoal = 'requirement' | 'system_design' | 'blueprint' | 'bundle' | 'open'

export interface PlanningBundle {
  id: string
  session_id: string
  title: string
  documents: Array<{
    kind: 'requirement' | 'system_design' | 'decision'
    id: string
    version?: number
    title?: string
    status?: string
  }>
  tasks: Array<{
    member_id: string
    created_task_id: string
    title: string
    body: string
    repo: string
    base_branch?: string
    depends_on?: string[]
    context?: { requirement_ids?: string[]; system_design_ids?: string[] }
  }>
  status: 'pending' | 'approved' | 'rejected'
  workspace: string
  decided_by?: string
  created_at: string
  decided_at?: string
}

export interface PlanningSession {
  id: string
  title?: string
  status: 'active' | 'finalized' | 'abandoned'
  /** Declared once at creation; sessions predating the goal read as `open`. */
  goal?: PlanningSessionGoal
  system_design_context_id?: string
  produced_system_design_id?: string
  produced_bundle_id?: string
  requirement_context_id?: string
  promotion?: RequirementDerivation
  produced_requirement_id?: string
  produced_task_id?: string
  transcript_artifact_id?: string
  model?: string
  effort?: string
  exploration_output_tokens?: number
  exploration_tokens_used?: number
  primary_repo?: string
  pinned_revisions?: Record<string, string>
  workspace: string
  created_at: string
  updated_at: string
  finalized_at?: string
}

export interface PlanningMessage {
  session_id: string
  seq: number
  role: 'user' | 'assistant' | 'system' | 'tool'
  content: string
  parts?: PlanningMessagePart[]
  workspace: string
  created_at: string
}

export interface PlanningMessagePart {
  type: string
  text?: string
  delta?: string
  toolName?: string
  toolCallId?: string
  state?: string
  input?: unknown
  output?: unknown
  errorText?: string
  [key: string]: unknown
}

export interface BlueprintLineage {
  task: Task
  spec?: SpecVersion
  events: TaskEvent[]
}

// The blueprint presentation surface. An anchor is an intent
// artifact derived from existing parent/child relations, never a stored
// entity, so this projection is read-only by construction.
export type BlueprintDeliveryState = 'in_delivery' | 'completed' | 'cancelled'

export interface BlueprintDelivery {
  state: BlueprintDeliveryState
  total: number
  merged: number
  closed: number
  open: number
}

export interface BlueprintChild extends TaskRelation {
  repo?: string
  summary?: string
  depends_on?: string[]
}

export interface BlueprintRequirementRef {
  id: string
  slug: string
  title: string
}

export interface BlueprintView {
  task: Task
  spec?: SpecVersion
  governing_version: number
  children: BlueprintChild[]
  delivery: BlueprintDelivery
  serves: BlueprintRequirementRef[]
  events: TaskEvent[]
  artifacts: Artifact[]
  planning_session?: PlanningSession
}

export interface RequirementView {
  requirement: Requirement
  current_version?: RequirementVersion
  pending_versions: RequirementVersion[]
  serving_blueprints: BlueprintLineage[]
  serving_tasks?: Task[]
  planning_sessions: PlanningSession[]
  artifacts: Artifact[]
  lineage: TaskEvent[]
  lineage_graph?: LineageGraph
  staleness?: RequirementStaleness
  migrated_seed: boolean
  confirmation_eligible: boolean
}

export interface RequirementSummary {
  requirement: Requirement
  current_version?: RequirementVersionSummary
  pending_version_count: number
  serving_tasks: Array<Pick<Task, 'id' | 'title' | 'state'>>
  staleness: RequirementStaleness
  confirmation_eligible: boolean
}

export type RequirementVersionSummary = Pick<
  RequirementVersion,
  'requirement_id' | 'version' | 'origin' | 'confirmed' | 'confirmed_by' | 'confirmed_at' | 'created_at'
>

export interface RequirementStaleness {
  delivery_after_intent: boolean
  partial_evaluation: boolean
  deliveries: RequirementDelivery[]
  active_drift: RepositoryDrift[]
}

export interface RequirementDelivery {
  signal_id: string
  task_id: string
  delivery_event_id: number
  event_kind: string
  label: string
  url?: string
  at: string
  pinned_version?: number
  current_version?: number
  needs_attention: boolean
  reasons: string[]
  follow_up?: {
    task_id: string
    title: string
    state: Task['state']
  }
}

export interface GovernedScope {
  repository: string
  paths: string[]
}
export interface SystemDesign {
  id: string
  slug: string
  title: string
  category: string
  current_version?: number
  workspace: string
  created_at: string
  updated_at: string
}
export interface SystemDesignVersion {
  document_id: string
  version: number
  content: string
  governs: GovernedScope[]
  origin: 'planning_session' | 'implementation_deliberation' | 'operator'
  origin_session_id?: string
  origin_task_id?: string
  confirmed: boolean
  confirmed_by?: string
  confirmed_at?: string
  dismissed: boolean
  dismissed_by?: string
  dismissed_at?: string
  workspace: string
  created_at: string
}
export interface SystemDesignView {
  document: SystemDesign
  current_version?: SystemDesignVersion
  pending_versions: SystemDesignVersion[]
  versions: SystemDesignVersion[]
  lineage: TaskEvent[]
  drift: RepositoryDrift[]
}
export interface Decision {
  id: string
  statement: string
  context: string
  alternatives_rejected: string
  status: 'proposed' | 'confirmed' | 'dismissed' | 'superseded'
  origin: 'planning_session' | 'implementation_deliberation' | 'operator'
  supersedes?: string
  confirmed_by?: string
  confirmed_at?: string
  dismissed_by?: string
  dismissed_at?: string
  superseded_by?: string
  workspace: string
  created_at: string
}
