import { expect, test } from '@playwright/test'

test('monitor page renders health, deduplication, task links, and drift age', async ({ page }) => {
  let resolution: Record<string, string> | undefined
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] }),
  )
  await page.route('**/v1/me**', (route) => route.fulfill({ json: { id: 'usr_operator', role: 'operator' } }))
  await page.route('**/v1/workspace?**', (route) => route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } }))
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
  await page.route('**/v1/requirements?**', (route) =>
    route.fulfill({
      json: [
        {
          requirement: { id: 'req-confirmed', title: 'Confirmed intent', current_version: 1 },
          current_version: { requirement_id: 'req-confirmed', version: 1, confirmed: true },
          pending_versions: [],
        },
        {
          requirement: { id: 'req-pending', title: 'Pending intent' },
          pending_versions: [{ requirement_id: 'req-pending', version: 1, confirmed: false }],
        },
      ],
    }),
  )
  await page.route('**/v1/monitor/drift/*/resolve?**', async (route) => {
    resolution = route.request().postDataJSON() as Record<string, string>
    await route.fulfill({
      json: {
        id: 'direct_push:conveyor:abc',
        outcome: resolution.outcome,
        requirement_id: resolution.requirement_id,
      },
    })
  })
  await page.route('**/v1/monitor?**', (route) =>
    route.fulfill({
      json: {
        workspace_id: 'demo',
        enabled: true,
        last_successful_observation: '2026-07-28T12:00:00Z',
        observations: [
          {
            workspace_id: 'demo',
            repository: 'conveyor',
            kind: 'post_merge_failure',
            occurrence_id: 'check:77:attempt:1',
            source_url: 'https://example.test/check/77',
            task_id: 'monitor-task',
            task_outcome: 'created',
            state: 'deduplicated',
            deduplicated_count: 2,
            created_at: '2026-07-28T12:00:00Z',
            updated_at: '2026-07-28T12:02:00Z',
          },
          {
            workspace_id: 'demo',
            repository: 'conveyor',
            kind: 'revert',
            occurrence_id: 'revert:saturated',
            source_url: 'https://example.test/commit/saturated',
            state: 'observed',
            deduplicated_count: 0,
            last_error: 'Task monitor-task already has 5 unresolved drift signals.',
            created_at: '2026-07-28T12:03:00Z',
            updated_at: '2026-07-28T12:03:00Z',
          },
        ],
        drift: [
          {
            id: 'direct_push:conveyor:abc',
            workspace_id: 'demo',
            repository: 'conveyor',
            kind: 'direct_push',
            source_url: 'https://example.test/commit/abc',
            commit_sha: 'abc',
            task_id: 'drift-task',
            detected_at: new Date(Date.now() - 3_600_000).toISOString(),
          },
        ],
        drift_count: 1,
        oldest_drift_age: 3_600_000_000_000,
      },
    }),
  )

  await page.goto('/monitor')
  await expect(page.getByRole('heading', { name: 'Repository monitor' })).toBeVisible()
  await expect(page.getByText('Enabled', { exact: true })).toBeVisible()
  await expect(page.getByText('1', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('1h', { exact: true })).toBeVisible()
  await expect(page.getByText('direct_push', { exact: true })).toBeVisible()
  await expect(page.getByText('Detected 1h ago', { exact: true })).toBeVisible()
  await expect(page.getByText('2 duplicates', { exact: true })).toBeVisible()
  await expect(page.getByText('created', { exact: true })).toBeVisible()
  // Scoped to the page body: the shell's own Tasks nav entry also matches a
  // substring search for "Task".
  await expect(page.getByRole('main').getByRole('link', { name: 'Task' }).first()).toHaveAttribute(
    'href',
    '/tasks/drift-task',
  )
  const refusal = page.getByText('revert:saturated').locator('..')
  await expect(refusal).toContainText('Task monitor-task already has 5 unresolved drift signals.')
  await expect(refusal.getByRole('link', { name: 'Task' })).toHaveCount(0)

  const form = page.getByRole('form', { name: 'Resolve drift direct_push:conveyor:abc' })
  await form.getByLabel('Resolution outcome for direct_push:conveyor:abc').selectOption('requirements_amended')
  const picker = form.getByLabel('Confirmed requirement for direct_push:conveyor:abc')
  await expect(picker.getByRole('option', { name: 'Confirmed intent' })).toBeAttached()
  await expect(picker.getByRole('option', { name: 'Pending intent' })).toHaveCount(0)
  await picker.selectOption('req-confirmed')
  await form.getByRole('button', { name: 'Resolve' }).click()
  await expect.poll(() => resolution).toEqual({ outcome: 'requirements_amended', requirement_id: 'req-confirmed' })
})
