import { expect, test } from '@playwright/test'

test('monitor page renders health, deduplication, task links, and drift age', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
    sessionStorage.setItem('conveyor-token', 'test-token')
  })
  await page.route('**/v1/workspaces', (route) =>
    route.fulfill({ json: [{ id: 'demo', name: 'Demo', config_version: 1 }] }),
  )
  await page.route('**/v1/workspace?**', (route) => route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } }))
  await page.route('**/v1/activity?**', (route) => route.fulfill({ json: [] }))
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
            detected_at: '2026-07-28T11:00:00Z',
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
  await expect(page.getByText('2 duplicates', { exact: true })).toBeVisible()
  await expect(page.getByText('created', { exact: true })).toBeVisible()
  // Scoped to the page body: the shell's own Tasks nav entry also matches a
  // substring search for "Task".
  await expect(page.getByRole('main').getByRole('link', { name: 'Task' }).first()).toHaveAttribute(
    'href',
    '/tasks/drift-task',
  )
})
