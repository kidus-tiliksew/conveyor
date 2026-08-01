// Visual-capture harness for the board and task detail surfaces: renders a
// rich mocked activity feed and writes screenshots to test-results/shots/ for
// design review. The idle-board state also guards uniform lane sizing.
import { expect, test, type Page, type Route } from '@playwright/test'

const now = Date.now()
const ago = (minutes: number) => new Date(now - minutes * 60_000).toISOString()

const setupContract = {
  name: 'default-2',
  execution_settings: {
    control_plane: { triage: { model: 'openai/gpt-5.6-luna', timeout: '20m' } },
    spec: { harness: 'codex', model: 'gpt-5.6-sol', model_policy: 'explicit', timeout: '30m' },
    implementation: { harness: 'claude', model: 'claude-opus-5', model_policy: 'explicit', effort: 'high', timeout: '2h' },
    review: { execution: 'mcp', timeout: '30m', fallback_harness: 'codex' },
  },
  review: { seats: [{ model: 'gpt-5.6-sol', harness: 'codex', effort: 'high' }, { model: 'claude-opus-5', harness: 'claude', effort: 'medium' }] },
  refresh_review: 'delta',
}

function task(overrides: Record<string, unknown>) {
  return {
    id: 'task-260731-000001',
    workspace: 'conveyor',
    source: 'github:kidus-tiliksew/conveyor#412',
    title: 'Untitled',
    body: '',
    class: 'feature',
    level: 'L1',
    spec_approval: true,
    merge_approval: true,
    policy_version: 3,
    setup: 'default-2',
    setup_contract: setupContract,
    repo: 'conveyor',
    base_branch: 'main',
    branch: 'conveyor/task-260731-000001',
    state: 'running',
    created_at: ago(600),
    ...overrides,
  }
}

function summary(overrides: Record<string, unknown>, taskOverrides: Record<string, unknown>) {
  return {
    task: task(taskOverrides),
    latest_stage: 'implement',
    last_event_at: ago(12),
    needs_attention: false,
    ...overrides,
  }
}

const activity = [
  summary({ latest_stage: 'triage', last_event_at: ago(2) }, {
    id: 'task-260731-a1b2c3', state: 'queued', next_stage: 'triage', class: 'bug',
    title: 'Board column counts drift after a task is cancelled mid-review',
    source: 'github:kidus-tiliksew/conveyor#418', branch: 'conveyor/task-260731-a1b2c3',
  }),
  summary({ latest_stage: 'triage', last_event_at: ago(41) }, {
    id: 'task-260731-d4e5f6', state: 'running', class: 'chore', repo: 'conveyor-docs',
    title: 'Regenerate the MCP tool reference from the current server schema',
    source: 'cli', branch: 'conveyor-docs/task-260731-d4e5f6',
  }),
  summary({ latest_stage: 'spec', last_event_at: ago(7) }, {
    id: 'task-260731-773a10', state: 'running', class: 'feature',
    title: 'Requirement documents gain a confirmed-by attribution line',
    source: 'api', branch: 'conveyor/task-260731-773a10',
  }),
  summary({ latest_stage: 'spec', last_event_at: ago(96), needs_attention: false }, {
    id: 'task-260731-91aa02', state: 'queued', next_stage: 'spec', class: 'feature', hold: true,
    title: 'Planning session transcripts archive as artifacts on finalize',
    source: 'github:kidus-tiliksew/conveyor#401', branch: 'conveyor/task-260731-91aa02',
  }),
  summary({ latest_stage: 'implement', last_event_at: ago(3) }, {
    id: 'task-260731-4c8d1e', state: 'running', class: 'feature',
    title: 'Dependency-gated claiming derives blocked from the enforcement layer',
    source: 'github:kidus-tiliksew/conveyor#420', branch: 'conveyor/task-260731-4c8d1e',
  }),
  summary({
    latest_stage: 'implement', last_event_at: ago(55),
    stalled: { needed: true, reason: 'The worker child exited without a submission', last_failure: 'harness exited: status 1 — provider rejected the configured model' },
  }, {
    id: 'task-260731-6f2b99', state: 'running', class: 'bug',
    title: 'Worktree containment fails when the repo has an active rebase',
    source: 'monitor', branch: 'conveyor/task-260731-6f2b99',
  }),
  summary({
    latest_stage: 'implement', last_event_at: ago(180),
    stalled: { needed: false, reason: '', blocking_task_ids: ['task-260731-4c8d1e'] },
  }, {
    id: 'task-260731-08c5aa', state: 'queued', next_stage: 'implement', class: 'feature',
    title: 'Graph-walk context assembly reads lineage edges instead of the feed',
    source: 'api', branch: 'conveyor/task-260731-08c5aa',
    blocking_task_ids: ['task-260731-4c8d1e'],
    dependencies: [{ id: 'task-260731-4c8d1e', title: 'Dependency-gated claiming derives blocked from the enforcement layer', state: 'running' }],
  }),
  summary({ latest_stage: 'review', last_event_at: ago(1) }, {
    id: 'task-260731-b31f47', state: 'running', class: 'feature',
    title: 'Blueprint materialization fans approved decompositions into child tasks',
    source: 'github:kidus-tiliksew/conveyor#399', branch: 'conveyor/task-260731-b31f47',
  }),
  summary({
    latest_stage: 'review', last_event_at: ago(22),
    review_diagnostics: [{ status: 'claimed_without_verdict', work_order_id: 'wo-9', review_round: 2, review_seat: 2, reason: 'claim active past the lease window' }],
  }, {
    id: 'task-260731-cc7712', state: 'running', class: 'bug',
    title: 'Review seats retain their assignment across a setup change',
    source: 'cli', branch: 'conveyor/task-260731-cc7712',
  }),
  summary({ latest_stage: 'verify', last_event_at: ago(9) }, {
    id: 'task-260731-2ad4b0', state: 'running', class: 'feature',
    title: 'Evidence-gated submit_for_review rejects an empty evidence set',
    source: 'github:kidus-tiliksew/conveyor#388', branch: 'conveyor/task-260731-2ad4b0',
  }),
  summary({ latest_stage: 'review', last_event_at: ago(4), needs_attention: true }, {
    id: 'task-260731-gate01', state: 'awaiting_human', class: 'feature',
    title: 'Activities surfaces read as one story instead of a machine log',
    source: 'github:kidus-tiliksew/conveyor#421', branch: 'conveyor/task-260731-gate01',
  }),
  summary({ latest_stage: 'merge', last_event_at: ago(18), needs_attention: true }, {
    id: 'task-260731-mrg001', state: 'approved', class: 'chore',
    title: 'Pin the worker harness argv snapshot into the dispatch record',
    source: 'cli', branch: 'conveyor/task-260731-mrg001',
  }),
  summary({
    latest_stage: 'triage', last_event_at: ago(240), needs_attention: true,
    forge_failure: { category: 'auth', detail: 'resource not accessible by integration', surface: 'issue publication', at: ago(240) },
  }, {
    id: 'task-260731-park01', state: 'parked', class: 'question',
    title: 'Should monitor-created tasks inherit the source issue’s labels?',
    source: 'monitor', branch: 'conveyor/task-260731-park01',
  }),
  summary({ latest_stage: 'merge', last_event_at: ago(320) }, {
    id: 'task-260730-1af22a', state: 'merged', class: 'feature',
    title: 'Complete the blueprint materialization handoff',
    source: 'github:kidus-tiliksew/conveyor#377', branch: 'conveyor/task-260730-1af22a',
  }),
  summary({ latest_stage: 'spec', last_event_at: ago(690) }, {
    id: 'task-260729-55bd31', state: 'closed', class: 'bug', repo: 'conveyor-docs',
    title: 'Duplicate of #362 — closed without merging',
    source: 'api', branch: 'conveyor-docs/task-260729-55bd31',
  }),
]

const gateDetail = {
  task: task({
    id: 'task-260731-gate01', state: 'awaiting_human', class: 'feature',
    title: 'Activities surfaces read as one story instead of a machine log',
    source: 'github:kidus-tiliksew/conveyor#421', branch: 'conveyor/task-260731-gate01',
    body: 'The board card and the task detail both leak pipeline mechanics. Fold the mechanics into the narrative and keep only the signal a reviewer acts on.\n\nThe card should answer: what is it, is it moving, does it need me.',
    github: { task_id: 'task-260731-gate01', repository: 'kidus-tiliksew/conveyor', spec_version: 2, source: 'api', issue_number: 421, issue_url: 'https://github.com/kidus-tiliksew/conveyor/issues/421', state: 'published', create_state: 'confirmed', create_attempts: 1, reconcile_misses: 0, attempts: 1, created_at: ago(600), updated_at: ago(590) },
  }),
  jobs: [
    { id: 'j-triage', task_id: 'task-260731-gate01', stage: 'triage', harness: 'in-process', model_tier: 'openai/gpt-5.6-luna', runner: 'in-process', confinement: 'none', cost_usd: 0.03, tokens_in: 8200, tokens_out: 900, state: 'done', started_at: ago(598), ended_at: ago(596) },
    { id: 'j-spec', task_id: 'task-260731-gate01', stage: 'spec', harness: 'codex', model_tier: 'gpt-5.6-sol', runner: 'worker', confinement: 'none', cost_usd: 0.41, tokens_in: 61000, tokens_out: 7400, state: 'done', started_at: ago(560), ended_at: ago(540) },
    { id: 'j-impl', task_id: 'task-260731-gate01', stage: 'implement', harness: 'claude', model_tier: 'claude-opus-5', runner: 'worker', confinement: 'none', cost_usd: 3.12, tokens_in: 410000, tokens_out: 52000, state: 'done', started_at: ago(420), ended_at: ago(180) },
    { id: 'j-rev-1', task_id: 'task-260731-gate01', stage: 'review', harness: 'codex', model_tier: 'gpt-5.6-sol', runner: 'worker', confinement: 'none', cost_usd: 0.88, tokens_in: 120000, tokens_out: 9000, state: 'done', started_at: ago(120), ended_at: ago(104) },
    { id: 'j-rev-2', task_id: 'task-260731-gate01', stage: 'review', harness: 'claude', model_tier: 'claude-opus-5', runner: 'worker', confinement: 'none', cost_usd: 1.02, tokens_in: 133000, tokens_out: 11000, state: 'done', started_at: ago(120), ended_at: ago(96) },
  ],
  events: [
    { id: 1, task_id: 'task-260731-gate01', job_id: 'j-triage', kind: 'job.created', actor_id: 'system', actor_role: 'system', payload: {}, at: ago(599) },
    { id: 2, task_id: 'task-260731-gate01', job_id: 'j-triage', kind: 'triage.completed', actor_id: 'triage', actor_role: 'agent', payload: { summary: 'Classified as a feature against conveyor. Routing to spec with the §13.3 presentation constraints attached.' }, at: ago(596) },
    { id: 3, task_id: 'task-260731-gate01', kind: 'spec.version_created', actor_id: 'spec', actor_role: 'agent', payload: { version: 1 }, at: ago(540) },
    { id: 4, task_id: 'task-260731-gate01', kind: 'spec.version_approved', actor_id: 'operator', actor_role: 'human', payload: { version: 1 }, at: ago(500) },
    { id: 5, task_id: 'task-260731-gate01', kind: 'github_issue.associated', actor_id: 'system', actor_role: 'system', payload: { issue_url: 'https://github.com/kidus-tiliksew/conveyor/issues/421' }, at: ago(498) },
    { id: 6, task_id: 'task-260731-gate01', job_id: 'j-impl', kind: 'job.summary', actor_id: 'claude', actor_role: 'agent', payload: { summary: 'Reworked the board card to a two-line identity block, folded the provenance and repo chips into a single footer, and moved the audit numbers behind hover. Added a regression test for the badge budget.' }, at: ago(182) },
    { id: 7, task_id: 'task-260731-gate01', kind: 'pull_request.opened', actor_id: 'system', actor_role: 'system', payload: { url: 'https://github.com/kidus-tiliksew/conveyor/pull/423' }, at: ago(178) },
    { id: 8, task_id: 'task-260731-gate01', job_id: 'j-rev-1', kind: 'review.completed', actor_id: 'worker', actor_role: 'runner', payload: { review_work_order_id: 'wo-rev-1', review_round: 1, verdict: 'approve', summary: 'Approved', feedback: 'The card reads cleanly at 288px. Badge budget holds at two in every state I could construct.', review_seat: 1 }, at: ago(104) },
    { id: 9, task_id: 'task-260731-gate01', job_id: 'j-rev-2', kind: 'review.completed', actor_id: 'worker', actor_role: 'runner', payload: { review_work_order_id: 'wo-rev-2', review_round: 1, verdict: 'approve', summary: 'Approved', feedback: 'Verified the dependency tooltip still announces to screen readers after the markup change.', review_seat: 2 }, at: ago(96) },
    { id: 10, task_id: 'task-260731-gate01', kind: 'review.round_completed', actor_id: 'system', actor_role: 'system', payload: { review_round: 1, verdict: 'approve', summary: 'Unanimous', reviews: [{ review_work_order_id: 'wo-rev-1' }, { review_work_order_id: 'wo-rev-2' }] }, at: ago(95) },
  ],
  interventions: [
    { id: 1, task_id: 'task-260731-gate01', actor_id: 'operator', actor_role: 'human', action: 'approve', reason_code: 'approved', comment: 'Spec covers the badge budget and the tooltip contract. Go.', at: ago(500) },
  ],
  work_orders: [
    { id: 'wo-impl', task_id: 'task-260731-gate01', job_id: 'j-impl', stage: 'implement', state: 'completed', claimable: false, model: 'claude-opus-5', model_enforcement: 'worker-pinned', required_effort: 'high', queue_entered_at: ago(430), queue_deadline: ago(-1000), redispatch_count: 0, cost_usd: 3.12, tokens_in: 410000, tokens_out: 52000, self_reported: false },
    { id: 'wo-rev-1', task_id: 'task-260731-gate01', job_id: 'j-rev-1', stage: 'review', state: 'completed', claimable: false, review_round: 1, review_seat: 1, model: 'gpt-5.6-sol', model_enforcement: 'worker-pinned', required_effort: 'high', queue_entered_at: ago(125), queue_deadline: ago(-1000), redispatch_count: 0, cost_usd: 0.88, tokens_in: 120000, tokens_out: 9000, self_reported: false },
    { id: 'wo-rev-2', task_id: 'task-260731-gate01', job_id: 'j-rev-2', stage: 'review', state: 'completed', claimable: false, review_round: 1, review_seat: 2, model: 'claude-opus-5', model_enforcement: 'self-reported', queue_entered_at: ago(125), queue_deadline: ago(-1000), redispatch_count: 0, cost_usd: 1.02, tokens_in: 133000, tokens_out: 11000, self_reported: true },
  ],
  checkout_available: true,
  checkout_command: 'conveyor checkout task-260731-gate01',
  checkout_guidance: '',
  needs_attention: true,
  spec: {
    task_id: 'task-260731-gate01', version: 2, approved: true, acceptance_count: 4, created_at: ago(540), approved_at: ago(500),
    acceptance: [
      { id: 'AC-1', criterion: 'The board card shows at most two status chips in any state.', verify: 'playwright' },
      { id: 'AC-2', criterion: 'Provenance and repo render on one footer line.', verify: 'playwright' },
      { id: 'AC-3', criterion: 'Token and cost figures never render as persistent text.', verify: 'playwright' },
      { id: 'AC-4', criterion: 'Existing dashboard e2e specs pass unchanged.', verify: 'test' },
    ],
    decomposition: null,
    content: '## Specification\n\nThe activity surfaces currently render every mechanical fact the pipeline records. A reviewer opening the board needs three answers per card: what is this, is it moving, does it need me.\n\n### Board card\n\nCollapse the identity block to title plus a single muted meta line. Reserve chips for state that changes what the operator does next.\n\n### Task detail\n\nThe header repeats what the timeline already narrates. Keep the facts a reviewer references (branch, base, PR) and move the rest behind disclosure.',
  },
  verification_evidence: [
    { id: 'art-1', workspace: 'conveyor', name: 'board-card-states.png', content_type: 'image/png', size_bytes: 184320, role: 'verification_evidence', task_id: 'task-260731-gate01', created_at: ago(180) },
    { id: 'art-2', workspace: 'conveyor', name: 'playwright-report.txt', content_type: 'text/plain', size_bytes: 4096, role: 'verification_evidence', task_id: 'task-260731-gate01', created_at: ago(179) },
  ],
  attachments: [],
  merge_readiness: { state: 'MERGEABLE', head_sha: 'a1b2c3d4', url: 'https://github.com/kidus-tiliksew/conveyor/pull/423', number: 423 },
}

// The running-sheet shots want a task mid-flight: a live review job, two
// review seats, a costed timeline tail. That is ordinary work, so this fixture
// carries no children — children would make it a blueprint anchor, and an
// anchor has no sheet to screenshot and takes no work orders (spec §21.49).
const runningDetail = {
  ...gateDetail,
  task: task({
    id: 'task-260731-b31f47', state: 'running', class: 'feature',
    title: 'Blueprint materialization fans approved decompositions into child tasks',
    source: 'github:kidus-tiliksew/conveyor#399', branch: 'conveyor/task-260731-b31f47',
    body: 'An approved spec carrying a decomposition block should fan out into child tasks entering at implement.',
  }),
  needs_attention: false,
  merge_readiness: undefined,
  interventions: [],
  events: gateDetail.events.slice(0, 5),
  jobs: [
    ...gateDetail.jobs.slice(0, 3),
    { id: 'j-rev-live-1', task_id: 'task-260731-b31f47', stage: 'review', harness: 'codex', model_tier: 'gpt-5.6-sol', runner: 'worker', confinement: 'none', cost_usd: 0.44, tokens_in: 71000, tokens_out: 4200, state: 'running', started_at: ago(14) },
  ],
  work_orders: [
    { id: 'wo-rev-live-1', task_id: 'task-260731-b31f47', job_id: 'j-rev-live-1', stage: 'review', state: 'claimed', claimable: false, review_round: 2, review_seat: 1, model: 'gpt-5.6-sol', model_enforcement: 'worker-pinned', required_effort: 'high', progress: 'Reading the diff against the materialization contract.', queue_entered_at: ago(16), queue_deadline: ago(-1000), redispatch_count: 0, cost_usd: 0.44, tokens_in: 71000, tokens_out: 4200, self_reported: false },
    { id: 'wo-rev-live-2', task_id: 'task-260731-b31f47', job_id: 'j-rev-live-2', stage: 'review', state: 'queued', claimable: true, review_round: 2, review_seat: 2, required_model: 'claude-opus-5', queue_entered_at: ago(16), queue_deadline: ago(-1000), redispatch_count: 0, cost_usd: 0, tokens_in: 0, tokens_out: 0, self_reported: false },
  ],
  spec: { ...gateDetail.spec, task_id: 'task-260731-b31f47' },
  verification_evidence: [],
}

const details: Record<string, unknown> = {
  'task-260731-gate01': gateDetail,
  'task-260731-b31f47': runningDetail,
}

async function mockAPIs(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'conveyor')
    sessionStorage.setItem('conveyor-token', 'operator-token')
  })
  await page.route('**/v1/**', async (route: Route) => {
    const url = new URL(route.request().url())
    if (url.pathname === '/v1/workspaces') { await route.fulfill({ json: [{ id: 'conveyor', name: 'Conveyor', config_version: 4, created_at: ago(9000) }] }); return }
    if (url.pathname === '/v1/activity') { await route.fulfill({ json: activity }); return }
    const detail = /^\/v1\/tasks\/([^/]+)\/activity$/.exec(url.pathname)
    if (detail) { await route.fulfill({ json: details[decodeURIComponent(detail[1])] ?? gateDetail }); return }
    if (url.pathname === '/v1/workspace') { await route.fulfill({ json: { workspace: 'conveyor', max_bounces: 3, database: 'postgres', repos: [], routing: [] } }); return }
    if (url.pathname === '/v1/workers') { await route.fulfill({ json: { workers: [], auto_available: true } }); return }
    await route.fulfill({ json: [] })
  })
}

const shots = 'test-results/shots'

for (const theme of ['light', 'dark'] as const) {
  test(`design harness ${theme}`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: theme })
    await page.setViewportSize({ width: 1680, height: 1050 })
    await mockAPIs(page)

    await page.goto('/')
    await page.getByText('Activities surfaces read as one story').first().waitFor()
    await page.screenshot({ path: `${shots}/board-${theme}.png` })

    // Filtering the feed must not change lane widths when most stages empty.
    await page.getByPlaceholder('Search tasks').fill('blueprint')
    await page.waitForTimeout(300)
    const columns = page.getByLabel('Task board').locator('section[aria-label]')
    await expect(columns).toHaveCount(7)
    const widths = await columns.evaluateAll((lanes) => lanes.map((lane) => lane.getBoundingClientRect().width))
    expect(Math.max(...widths) - Math.min(...widths)).toBeLessThan(1)
    await page.screenshot({ path: `${shots}/board-idle-${theme}.png` })
    // Wide desktop: equal lanes still grow together to use spare row width.
    await page.setViewportSize({ width: 2240, height: 1120 })
    await page.waitForTimeout(300)
    const wideWidths = await columns.evaluateAll((lanes) => lanes.map((lane) => lane.getBoundingClientRect().width))
    expect(Math.max(...wideWidths) - Math.min(...wideWidths)).toBeLessThan(1)
    await page.screenshot({ path: `${shots}/board-idle-wide-${theme}.png` })
    await page.setViewportSize({ width: 1680, height: 1050 })
    await page.getByPlaceholder('Search tasks').fill('')

    await page.goto('/tasks/task-260731-gate01')
    await page.getByLabel('Task detail').waitFor()
    await page.waitForTimeout(500)
    await page.screenshot({ path: `${shots}/sheet-gate-${theme}.png` })
    await page.locator('.task-sheet-body').evaluate((node) => { node.scrollTop = 0 })
    await page.waitForTimeout(200)
    await page.screenshot({ path: `${shots}/sheet-head-${theme}.png` })

    await page.goto('/tasks/task-260731-b31f47')
    await page.getByLabel('Task detail').waitFor()
    await page.waitForTimeout(500)
    await page.screenshot({ path: `${shots}/sheet-running-${theme}.png` })
    await page.locator('.task-sheet-body').evaluate((node) => { node.scrollTop = node.scrollHeight })
    await page.waitForTimeout(200)
    await page.screenshot({ path: `${shots}/sheet-running-tail-${theme}.png` })

    await page.goto('/tasks/task-260731-gate01/full')
    await page.getByLabel('Task content').waitFor()
    await page.waitForTimeout(500)
    await page.screenshot({ path: `${shots}/full-gate-${theme}.png`, fullPage: false })
    await page.getByLabel('Task content').evaluate((node) => { node.scrollTop = 0 })
    await page.waitForTimeout(200)
    await page.screenshot({ path: `${shots}/full-head-${theme}.png` })
  })
}
