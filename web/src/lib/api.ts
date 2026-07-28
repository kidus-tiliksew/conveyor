import type {
  ActivityItem,
  ActivitySummary,
  InterventionAction,
  Task,
  VersionedWorkspaceConfig,
  WorkspaceConfigDocument,
  WorkspaceConfigReceipt,
  WorkspaceInfo,
  WorkspaceRecord,
  RequirementNode,
  Artifact,
  WorkerList,
  WorkOrder,
  HarnessTemplate,
} from './types'

function workspaceURL(path: string) {
  const workspace = localStorage.getItem('conveyor-workspace') ?? ''
  if (!workspace) return path
  const separator = path.includes('?') ? '&' : '?'
  return `${path}${separator}workspace_id=${encodeURIComponent(workspace)}`
}

async function getJSON<T>(url: string): Promise<T> {
	const token = sessionStorage.getItem('conveyor-token') ?? ''
	const response = await fetch(url, { headers: token ? { Authorization: `Bearer ${token}` } : {} })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<T>
}

export function fetchActivity() {
	return getJSON<ActivitySummary[]>(workspaceURL('/v1/activity'))
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
  const response = await fetch('/v1/workspaces', { method: 'POST', headers: mutationHeaders(token), body: JSON.stringify(input) })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<WorkspaceRecord>
}

export function fetchRequirements() { return getJSON<RequirementNode[]>(workspaceURL('/v1/requirements')) }
export function fetchLifecycleDiagram() { return getJSON<{ mermaid: string }>(workspaceURL('/v1/lifecycle-diagram')) }
export function fetchTasks() { return getJSON<Task[]>(workspaceURL('/v1/tasks')) }
export async function fetchArtifacts(token: string) { const response = await fetch(workspaceURL('/v1/artifacts'), { headers: { Authorization: `Bearer ${token}` } }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<Artifact[]> }
export async function uploadArtifact(token: string, file: File, taskId?: string, featureId?: string, role?: Artifact['role']) { const body = new FormData(); body.set('file', file); if (taskId) body.set('task_id', taskId); if (featureId) body.set('feature_id', featureId); if (role) body.set('role', role); const response = await fetch(workspaceURL('/v1/artifacts'), { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'X-Conveyor-Actor': 'dashboard-operator' }, body }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<Artifact> }
export async function createFeature(token: string, input: { name: string; description: string; parent_id?: string }) { const response = await fetch(workspaceURL('/v1/features'), { method: 'POST', headers: mutationHeaders(token), body: JSON.stringify(input) }); if (!response.ok) throw new Error(await response.text()); return response.json() }
export async function assignTaskFeature(token: string, taskId: string, featureId: string) { const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/feature`), { method: 'PUT', headers: mutationHeaders(token), body: JSON.stringify({ feature_id: featureId }) }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<Task> }
// Fetch an attachment's bytes as an object URL for inline preview. The
// download route requires the operator token and forces attachment
// disposition, so an <img src> cannot load it directly — the caller revokes
// the returned URL when the preview unmounts.
export async function fetchArtifactObjectURL(token: string, artifact: Artifact) {
  const response = await fetch(workspaceURL(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`), { headers: token ? { Authorization: `Bearer ${token}` } : {} })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return URL.createObjectURL(await response.blob())
}
export async function downloadArtifact(token: string, artifact: Artifact) { const response = await fetch(workspaceURL(artifact.download_url ?? `/v1/artifacts/${encodeURIComponent(artifact.id)}`), { headers: { Authorization: `Bearer ${token}` } }); if (!response.ok) throw new Error(await response.text()); const blob = await response.blob(); const url = URL.createObjectURL(blob); const anchor = document.createElement('a'); anchor.href = url; anchor.download = artifact.name; anchor.click(); URL.revokeObjectURL(url) }

export function fetchWorkspaceConfig(token: string) {
	return fetch(workspaceURL('/v1/workspace/config'), { headers: mutationHeaders(token) }).then(async (response) => {
    if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
    const result = await response.json() as VersionedWorkspaceConfig
		const fallbackReview = { seats: [{ model: result.document.routing.stages.review?.model ?? '', harness: result.document.routing.stages.review?.harness }] }
		const review = { ...result.document.review, seats: result.document.review?.seats ?? fallbackReview.seats }
		const setups = (result.document.setups?.length ? result.document.setups : [{ name: 'default', execution_settings: result.document.execution_settings, review, refresh_review: 'delta' as const }])
			.map((setup) => ({
				...setup,
				review: { ...setup.review, seats: setup.review?.seats ?? review.seats },
				refresh_review: setup.refresh_review || 'delta' as const,
			}))
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
        review,
        setups,
        default_setup: result.document.default_setup || setups[0].name,
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
  body: string
  repo: string
  base_branch?: string
  hold?: boolean
  spec_approval?: boolean
  merge_approval?: boolean
  setup?: string
}

export async function fetchWorkers(token: string) { const response = await fetch(workspaceURL('/v1/workers'), { headers: mutationHeaders(token) }); if (!response.ok) throw new Error(await response.text()); const result = await response.json() as WorkerList; return { ...result, workers: (result.workers ?? []).map((worker) => ({ ...worker, probes: worker.probes ?? [] })) } }
export async function issueWorkerPairing(token: string) { const response = await fetch(workspaceURL('/v1/workers/pairings'), { method: 'POST', headers: mutationHeaders(token), body: JSON.stringify({ ttl_seconds: 600 }) }); if (!response.ok) throw new Error(await response.text()); return response.json() as Promise<{ pairing_token: string; expires_at: string }> }
export async function revokeWorker(token: string, id: string) { const response = await fetch(workspaceURL(`/v1/workers/${encodeURIComponent(id)}`), { method: 'DELETE', headers: mutationHeaders(token) }); if (!response.ok) throw new Error(await response.text()) }

export async function createTask(token: string, input: CreateTaskInput, attachments: File[] = [], idempotencyKey = '') {
  const body = new FormData()
  body.set('task', JSON.stringify({ ...input, source: 'dashboard' }))
  if (idempotencyKey) body.set('idempotency_key', idempotencyKey)
  for (const file of attachments) body.append('attachments', file)
  const response = await fetch(workspaceURL('/v1/tasks'), {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'X-Conveyor-Actor': 'dashboard-operator' },
    body,
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<Task>
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

// Toggle the per-task hold (spec §21.31): while held, workers never claim
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

export async function changeTaskSetup(taskId: string, token: string, input: { setup?: string; apply_latest?: boolean; reason: string; request_id: string }) {
  const response = await fetch(workspaceURL(`/v1/tasks/${encodeURIComponent(taskId)}/setup`), {
    method: 'POST', headers: mutationHeaders(token), body: JSON.stringify(input),
  })
  if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
  return response.json() as Promise<{ task: Task; review_transition: string }>
}

export async function recoverWorkOrder(workOrderId: string, token: string, requestId: string) {
	const response = await fetch(workspaceURL(`/v1/work-orders/${encodeURIComponent(workOrderId)}/recover`), {
		method: 'POST',
		headers: { ...mutationHeaders(token), 'Content-Type': 'application/json', 'X-Idempotency-Key': requestId },
		body: JSON.stringify({ request_id: requestId }),
	})
	if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
	return response.json() as Promise<WorkOrder>
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
      'X-Conveyor-Actor': 'dashboard-operator',
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
		method: 'POST', headers: mutationHeaders(token),
	})
	if (!response.ok) throw new Error((await response.text()).trim() || response.statusText)
	return response.json() as Promise<import('./types').WorkOrder>
}
