import { expect, type Page, type Route, test } from '@playwright/test'

const proposedAt = '2026-08-10T10:00:00Z'

async function initialize(page: Page) {
  await page.addInitScript(() => {
    localStorage.setItem('conveyor-workspace', 'demo')
  })
}

test('pending proposal label and attention badge stay on one line at the narrow reference viewport', async ({
  page,
}) => {
  await page.setViewportSize({ width: 550, height: 1982 })
  await initialize(page)
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals')
      return route.fulfill({
        json: {
          items: [
            {
              id: 'DEC-42',
              title: 'Operator attention',
              tier: 'decision',
              origin_type: 'operator',
              proposed_at: proposedAt,
              age_seconds: 60,
            },
          ],
          attention: { task_count: 0, pending_proposal_count: 1, total: 1 },
        },
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  const primary = page.getByRole('navigation', { name: 'Primary' })
  const pending = primary.getByRole('link', { name: /Pending proposals/ })
  const label = pending.getByText('Pending proposals', { exact: true })
  const badge = pending.getByText('1', { exact: true })

  await expect(primary).toHaveCSS('width', '256px')
  await expect(label).toBeVisible()
  await expect(badge).toBeVisible()
  expect(
    await label.evaluate((element) => {
      const lineHeight = Number.parseFloat(getComputedStyle(element).lineHeight)
      return element.scrollHeight <= lineHeight + 1
    }),
  ).toBe(true)
  expect(
    await page.evaluate(() => {
      const main = document.querySelector('main')
      const nav = document.querySelector('nav[aria-label="Primary"]')
      if (!(main instanceof HTMLElement) || !(nav instanceof HTMLElement)) return false
      return (
        document.documentElement.scrollWidth <= window.innerWidth &&
        nav.scrollWidth <= nav.clientWidth &&
        main.clientWidth > 0 &&
        main.getBoundingClientRect().right <= window.innerWidth
      )
    }),
  ).toBe(true)
})

test('pending proposal queue covers every document tier, resolves rows, updates the badge, and clears the task warning', async ({
  page,
}) => {
  await initialize(page)
  let requirementPending = true
  let decisionPending = true
  let designPending = true
  let designDismissRequests = 0

  await page.route('**/v1/**', async (route: Route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity')
      return route.fulfill({
        json: [
          { task: { id: 'one', state: 'awaiting_human' }, needs_attention: true },
          { task: { id: 'two', state: 'parked' }, needs_attention: true },
        ],
      })
    if (path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      expect(request.headers().authorization).toBeUndefined()
      const items = [
        ...(requirementPending
          ? [
              {
                id: 'req-attention',
                title: 'Operator attention',
                tier: 'requirement',
                version: 2,
                origin_type: 'task',
                origin_id: 'review-task',
                proposed_at: proposedAt,
                age_seconds: 3600,
              },
              {
                id: 'req-attention',
                title: 'Operator attention',
                tier: 'requirement',
                version: 3,
                origin_type: 'task',
                origin_id: 'review-task',
                proposed_at: proposedAt,
                age_seconds: 1800,
              },
            ]
          : []),
        ...(designPending
          ? [
              {
                id: 'design-dashboard',
                title: 'Web dashboard architecture',
                tier: 'system_design',
                version: 4,
                origin_type: 'session',
                origin_id: 'planning-1',
                proposed_at: proposedAt,
                age_seconds: 900,
              },
            ]
          : []),
        ...(decisionPending
          ? [
              {
                id: 'DEC-16',
                title: 'Generate dashboard output from web sources.',
                tier: 'decision',
                origin_type: 'operator',
                proposed_at: proposedAt,
                age_seconds: 120,
              },
            ]
          : []),
      ]
      return route.fulfill({
        json: { items, attention: { task_count: 2, pending_proposal_count: items.length, total: 2 + items.length } },
      })
    }
    if (path === '/v1/requirements/req-attention' && request.method() === 'GET')
      return route.fulfill({
        json: { requirement: { id: 'req-attention', title: 'Operator attention', current_version: 1 } },
      })
    if (path === '/v1/requirements/req-attention/versions/3/confirm') {
      expect(request.headers()['if-match']).toBe('"1"')
      requirementPending = false
      return route.fulfill({ json: {} })
    }
    if (path === '/v1/decisions/DEC-16/dismiss') {
      decisionPending = false
      return route.fulfill({ json: {} })
    }
    if (path === '/v1/system-designs/design-dashboard/versions/4/dismiss') {
      designDismissRequests++
      designPending = false
      return route.fulfill({ json: {} })
    }
    if (path === '/v1/tasks/review-task/activity')
      return route.fulfill({
        json: {
          task: {
            id: 'review-task',
            workspace: 'demo',
            source: 'mcp',
            title: 'Review task',
            body: 'A task with a proposed document update.',
            repo: 'conveyor',
            base_branch: 'main',
            branch: 'conveyor/task-review-task',
            state: 'running',
            created_at: proposedAt,
          },
          jobs: [],
          events: [],
          interventions: [],
          checkout_available: false,
          checkout_guidance: '',
          needs_attention: requirementPending,
          pending_authority: requirementPending,
          work_orders: [],
          attachments: [],
          verification_evidence: [],
        },
      })
    if (path.endsWith('/events/stream')) return route.fulfill({ status: 204 })
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  await expect(page.getByRole('heading', { name: 'Pending proposals' })).toBeVisible()
  await expect(page.getByText('Requirement', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('System Design', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('Decision', { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('6')
  await expect(page.getByRole('link', { name: /Requirements/ })).toContainText('1')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('4')
  await expect(
    page.getByText('Confirming a later version also dismisses earlier pending versions.').first(),
  ).toBeVisible()

  await page.goto('/tasks/review-task/full')
  const warning = page.getByRole('region', { name: 'Review is waiting on a document decision' })
  await expect(warning).toContainText('This review cannot be claimed until you confirm or dismiss')
  await warning.getByRole('link', { name: 'Confirm or dismiss the proposal' }).click()
  await expect(page.getByText('Showing proposals from task review-task.')).toBeVisible()
  const later = page.getByRole('listitem').filter({ hasText: 'v3' })
  await later.getByRole('button', { name: 'Confirm' }).click()
  await expect(page.getByText('No document decisions are waiting for you.')).toBeVisible()
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('4')
  await expect(page.getByRole('link', { name: /Requirements/ })).not.toContainText('1')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('2')

  await page.goBack()
  await expect(warning).toHaveCount(0)
  await page.goto('/pending-proposals')
  const decision = page.getByRole('listitem').filter({ hasText: 'Generate dashboard output from web sources.' })
  await decision.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByText('Generate dashboard output from web sources.')).toHaveCount(0)
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('3')
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('1')

  const design = page.getByRole('listitem').filter({ hasText: 'Web dashboard architecture' })
  await design.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByRole('dialog', { name: 'Dismiss version 4 of Web dashboard architecture' })).toBeVisible()
  expect(designDismissRequests).toBe(0)
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(design).toBeVisible()
  expect(designDismissRequests).toBe(0)
  await design.getByRole('button', { name: 'Dismiss' }).click()
  await page.getByRole('button', { name: 'Dismiss version 4' }).click()
  await expect.poll(() => designDismissRequests).toBe(1)
  await expect(page.getByText('No document decisions are waiting for you.')).toBeVisible()
})

test('pending proposals keeps task context suggestions off the workspace queue and its count', async ({ page }) => {
  await initialize(page)
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      return route.fulfill({
        json: {
          items: [
            {
              id: 'req-document-update',
              title: 'Document-only queue contract',
              tier: 'requirement',
              version: 2,
              origin_type: 'operator',
              proposed_at: proposedAt,
              age_seconds: 90,
            },
            // The page filter is defensive. The HTTP contract excludes this row.
            {
              id: 'req-intake',
              title: 'Task intake and triage',
              tier: 'task_context',
              origin_type: 'task',
              origin_id: 'context-task',
              target_kind: 'requirement',
              justification: 'This suggestion belongs on the task view.',
              proposed_at: proposedAt,
              age_seconds: 60,
            },
          ],
          attention: { task_count: 1, pending_proposal_count: 1, total: 2 },
        },
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  await expect(page.getByRole('listitem')).toHaveCount(1)
  await expect(page.getByText('Document-only queue contract')).toBeVisible()
  await expect(page.getByText('Task intake and triage')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Dismiss' })).toBeVisible()
  await expect(page.getByRole('link', { name: /Pending proposals/ })).toContainText('1')
  await expect(page.getByRole('link', { name: /Board/ })).toContainText('2')
})

test('attention navigation omits requirement and pending proposal badges when projection fails or is empty', async ({
  page,
}) => {
  await initialize(page)
  let failProjection = true
  let projectionRequests = 0
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      projectionRequests++
      if (failProjection) return route.fulfill({ status: 500, json: { error: 'projection unavailable' } })
      return route.fulfill({
        json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } },
      })
    }
    return route.fulfill({ json: [] })
  })

  await page.goto('/')
  await expect.poll(() => projectionRequests).toBeGreaterThan(0)
  await expect(page.getByRole('link', { name: 'Requirements', exact: true })).toHaveText('Requirements')
  await expect(page.getByRole('link', { name: 'Pending proposals', exact: true })).toHaveText('Pending proposals')

  failProjection = false
  await page.reload()
  await expect.poll(() => projectionRequests).toBeGreaterThan(1)
  await expect(page.getByRole('link', { name: 'Requirements', exact: true })).toHaveText('Requirements')
  await expect(page.getByRole('link', { name: 'Pending proposals', exact: true })).toHaveText('Pending proposals')
})

test('maintainer can read pending decisions without corpus-authority controls', async ({ page }) => {
  await initialize(page)
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_maintainer', role: 'maintainer' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals')
      return route.fulfill({
        json: {
          items: [
            {
              id: 'DEC-42',
              title: 'Operator-only corpus decision',
              tier: 'decision',
              origin_type: 'operator',
              proposed_at: proposedAt,
              age_seconds: 60,
            },
          ],
          attention: { task_count: 0, pending_proposal_count: 1, total: 1 },
        },
      })
    return route.fulfill({ json: [] })
  })

  await page.goto('/pending-proposals')
  const decision = page.getByRole('listitem').filter({ hasText: 'Operator-only corpus decision' })
  await expect(decision).toBeVisible()
  await expect(decision.getByRole('button', { name: 'Confirm' })).toHaveCount(0)
  await expect(decision.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
})

test('pending proposal consumers share one active query and hidden documents stop interval polling', async ({
  page,
}) => {
  await initialize(page)
  await page.addInitScript(() => {
    let visibility: DocumentVisibilityState = 'visible'
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibility,
    })
    Object.defineProperty(window, '__setTestVisibility', {
      value: (next: DocumentVisibilityState) => {
        visibility = next
        window.dispatchEvent(new Event('visibilitychange'))
      },
    })
  })
  let projectionRequests = 0
  await page.route('**/v1/**', async (route: Route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
    if (path === '/v1/me') return route.fulfill({ json: { id: 'usr_operator', role: 'operator' } })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
    if (path === '/v1/activity' || path === '/v1/blueprints') return route.fulfill({ json: [] })
    if (path === '/v1/pending-proposals') {
      projectionRequests++
      return route.fulfill({
        json: { items: [], attention: { task_count: 0, pending_proposal_count: 0, total: 0 } },
      })
    }
    return route.fulfill({ json: [] })
  })

  // AppShell and the queue page both subscribe to the same workspace key.
  await page.goto('/pending-proposals')
  await expect.poll(() => projectionRequests).toBe(1)

  await page.evaluate(() => {
    ;(window as Window & { __setTestVisibility(next: DocumentVisibilityState): void }).__setTestVisibility('hidden')
  })
  await page.waitForTimeout(16_000)
  expect(projectionRequests).toBe(1)

  await page.evaluate(() => {
    ;(window as Window & { __setTestVisibility(next: DocumentVisibilityState): void }).__setTestVisibility('visible')
  })
  await expect.poll(() => projectionRequests).toBe(2)

  await page.waitForTimeout(5_100)
  await page.context().setOffline(true)
  await page.context().setOffline(false)
  await expect.poll(() => projectionRequests).toBe(3)
})

for (const tier of ['requirement', 'system_design'] as const) {
  for (const outcome of ['success', 'validation', 'conflict'] as const) {
    test(`Revise ${tier} proposal handles ${outcome} and preserves the reviewed base`, async ({ page }) => {
      await initialize(page)
      const endpoint = tier === 'requirement' ? 'requirements' : 'system-designs'
      const calls: string[] = []
      let confirmed = false
      let proposed = false
      let currentVersion = 1
      let detailReads = 0
      const edited =
        tier === 'requirement'
          ? '# Corrected proposal\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep the complete content.\n```'
          : '# Corrected proposal\n\n```conveyor:governs\n- repo: conveyor\n  paths: [web/**]\n```'
      const pending = {
        version: 2,
        content: '# Pending content',
        origin: 'implementation',
        origin_task_id: 'origin-task',
      }
      await page.route('**/v1/**', async (route) => {
        const request = route.request()
        const path = new URL(request.url()).pathname
        if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
        if (path === '/v1/me') return route.fulfill({ json: { id: 'operator', role: 'operator' } })
        if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
        if (path === '/v1/tasks/origin-task/activity')
          return route.fulfill({
            json: {
              task: {
                id: 'origin-task',
                workspace: 'demo',
                source: 'mcp',
                title: 'Origin task',
                body: 'Pending authority',
                repo: 'conveyor',
                base_branch: 'main',
                branch: 'conveyor/task-origin',
                state: 'running',
                created_at: proposedAt,
              },
              jobs: [],
              events: [],
              interventions: [],
              work_orders: [],
              attachments: [],
              verification_evidence: [],
              checkout_available: false,
              checkout_guidance: '',
              needs_attention: !confirmed,
              pending_authority: !confirmed,
            },
          })
        if (path.endsWith('/events/stream')) return route.fulfill({ status: 204 })
        if (path === '/v1/pending-proposals') {
          const items = confirmed
            ? []
            : [
                {
                  id: 'document',
                  title: 'Proposal to correct',
                  tier,
                  version: 2,
                  origin_type: 'task',
                  origin_id: 'origin-task',
                  age_seconds: 60,
                },
                ...(proposed
                  ? [
                      {
                        id: 'document',
                        title: 'Proposal to correct',
                        tier,
                        version: 3,
                        origin_type: 'operator',
                        age_seconds: 0,
                      },
                    ]
                  : []),
              ]
          return route.fulfill({
            json: { items, attention: { pending_proposal_count: items.length, task_count: 0, total: items.length } },
          })
        }
        if (path === `/v1/${endpoint}/document`) {
          detailReads++
          return route.fulfill({
            json: {
              [tier === 'requirement' ? 'requirement' : 'document']: {
                id: 'document',
                current_version: currentVersion,
              },
              current_version: { version: currentVersion, content: '# Confirmed content' },
              pending_versions: [pending],
            },
          })
        }
        if (path === `/v1/${endpoint}/document/versions`) {
          expect(request.method()).toBe('POST')
          expect(request.postDataJSON()).toEqual({ content: edited, origin: 'operator' })
          expect(request.headers()['x-conveyor-csrf']).toBe('1')
          expect(request.headers().authorization).toBeUndefined()
          calls.push('propose')
          if (outcome === 'validation') return route.fulfill({ status: 400, body: 'Duplicate statement REQ-1' })
          proposed = true
          return route.fulfill({ status: 201, json: { version: 3, content: edited, origin: 'operator' } })
        }
        if (path === `/v1/${endpoint}/document/versions/3/confirm`) {
          calls.push('confirm')
          expect(request.postData()).toBe(
            outcome === 'success' ? JSON.stringify({ note: 'Correct the original scope.' }) : null,
          )
          expect(request.headers()['if-match']).toBe('"1"')
          if (outcome === 'conflict') return route.fulfill({ status: 409, json: { error: 'current_version_mismatch' } })
          confirmed = true
          return route.fulfill({ json: {} })
        }
        return route.fulfill({ json: [] })
      })
      if (outcome === 'success') {
        await page.goto('/tasks/origin-task/full')
        const warning = page.getByRole('region', { name: 'Review is waiting on a document decision' })
        await warning.getByRole('link', { name: 'Confirm or dismiss the proposal' }).click()
      } else await page.goto('/pending-proposals')
      await page.getByRole('button', { name: 'Revise', exact: true }).click()
      const dialog = page.getByRole('dialog', { name: 'Revise version 2 of Proposal to correct' })
      await expect(dialog.getByLabel('Proposal content')).toHaveValue(pending.content)
      await expect(dialog).toContainText('Origin: task origin-task')
      await expect(dialog).toContainText('# Confirmed content')
      await dialog.getByLabel('Proposal content').fill(edited)
      if (outcome === 'success') {
        const note = dialog.getByLabel('What was wrong with the original?')
        await note.fill('😀'.repeat(2000))
        await note.press('End')
        await note.pressSequentially('x')
        await expect(note).toHaveValue('😀'.repeat(2000))
        await expect(dialog).toContainText('2000 / 2000 characters')
        await note.fill('  Correct the original scope.  ')
      }
      if (outcome === 'conflict') currentVersion = 4 // Another operator changed the reviewed base.
      await dialog.getByRole('button', { name: 'Propose and confirm' }).click()
      if (outcome === 'success') {
        await expect(dialog).toHaveCount(0)
        await expect(page.getByText('No document decisions are waiting for you.')).toBeVisible()
        await expect(page.getByRole('link', { name: /Pending proposals/ })).not.toContainText('1')
        expect(calls).toEqual(['propose', 'confirm'])
        await page.goBack()
        await expect(page.getByRole('heading', { name: 'Origin task', exact: true })).toBeVisible()
        await expect(page.getByRole('region', { name: 'Review is waiting on a document decision' })).toHaveCount(0)
      } else {
        await expect(dialog.getByRole('alert')).toContainText(
          outcome === 'validation'
            ? 'Duplicate statement REQ-1'
            : 'Version 3 was proposed, but confirmation failed. It remains pending.',
        )
        await expect(dialog.getByLabel('Proposal content')).toHaveValue(edited)
        expect(calls).toEqual(outcome === 'validation' ? ['propose'] : ['propose', 'confirm'])
        if (outcome === 'conflict') {
          await expect(dialog.getByRole('button', { name: 'Propose and confirm' })).toBeDisabled()
          await dialog.getByRole('button', { name: 'Close', exact: true }).click()
          await expect(page.getByRole('listitem')).toHaveCount(2)
          await page.getByRole('listitem').filter({ hasText: 'v2' }).getByRole('button', { name: 'Revise' }).click()
          await expect(page.getByRole('dialog')).toContainText('Confirmed v4')
          expect(detailReads).toBeGreaterThan(1)
        } else {
          await expect(dialog.getByRole('button', { name: 'Propose and confirm' })).toBeEnabled()
        }
      }
    })
  }
}

for (const access of ['both', 'confirm-only', 'propose-only', 'neither'] as const) {
  test(`Revise capability gate requires both capabilities: ${access}`, async ({ page }) => {
    await initialize(page)
    if (access === 'confirm-only') {
      // Fixed production roles have no confirm-only bundle. Remove propose from
      // the served client bundle to exercise this independent capability check.
      await page.route('**/src/lib/workspace-capabilities.json*', async (route) => {
        const response = await route.fetch()
        await route.fulfill({ response, body: (await response.text()).replace(/"propose_documents",?/g, '') })
      })
    }
    await page.route('**/v1/**', async (route) => {
      const path = new URL(route.request().url()).pathname
      if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
      if (path === '/v1/me')
        return route.fulfill({
          json: {
            id: 'caller',
            role: access === 'neither' ? 'viewer' : access === 'propose-only' ? 'contributor' : 'operator',
          },
        })
      if (path === '/v1/pending-proposals')
        return route.fulfill({
          json: {
            items: ['requirement', 'system_design', 'decision'].map((tier) => ({
              id: tier,
              title: tier,
              tier,
              version: 2,
              origin_type: 'operator',
              age_seconds: 60,
            })),
            attention: { pending_proposal_count: 3, total: 3 },
          },
        })
      return route.fulfill({ json: [] })
    })
    await page.goto('/pending-proposals')
    await expect(page.getByRole('listitem')).toHaveCount(3)
    await expect(page.getByRole('button', { name: 'Revise', exact: true })).toHaveCount(access === 'both' ? 2 : 0)
    await expect(page.getByRole('button', { name: 'Confirm', exact: true })).toHaveCount(
      access === 'both' || access === 'confirm-only' ? 3 : 0,
    )
    await expect(
      page.getByRole('listitem').filter({ hasText: 'Decision' }).getByRole('button', { name: 'Revise' }),
    ).toHaveCount(0)
  })
}

for (const tier of ['requirement', 'system_design']) {
  for (const note of ['', '  \n ', '  Preserve the original scope.  ']) {
    test(`queue dismisses ${tier} with optional note ${JSON.stringify(note)}`, async ({ page }) => {
      await initialize(page)
      let body: string | null | undefined
      await page.route('**/v1/**', async (route) => {
        const path = new URL(route.request().url()).pathname
        if (path === '/v1/workspaces') return route.fulfill({ json: [{ id: 'demo', name: 'Demo' }] })
        if (path === '/v1/me') return route.fulfill({ json: { id: 'operator', role: 'operator' } })
        if (path === '/v1/workspace') return route.fulfill({ json: { workspace: 'demo', repos: ['conveyor'] } })
        if (path === '/v1/pending-proposals')
          return route.fulfill({
            json: {
              items:
                body === undefined
                  ? [
                      {
                        id: 'note-doc',
                        title: 'Note document',
                        tier,
                        version: 2,
                        origin_type: 'operator',
                        proposed_at: proposedAt,
                      },
                    ]
                  : [],
              attention: {
                task_count: 0,
                pending_proposal_count: body === undefined ? 1 : 0,
                total: body === undefined ? 1 : 0,
              },
            },
          })
        if (path === `/v1/${tier === 'requirement' ? 'requirements' : 'system-designs'}/note-doc/versions/2/dismiss`) {
          body = route.request().postData()
          return route.fulfill({ json: {} })
        }
        return route.fulfill({ json: [] })
      })
      await page.goto('/pending-proposals')
      await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
      const dialog = page.getByRole('dialog')
      const field = dialog.getByLabel('Why are you dismissing this?')
      await expect(dialog).toContainText('0 / 2000 characters')
      await field.fill('a'.repeat(2001))
      await expect(field).toHaveValue('a'.repeat(2000))
      await field.press('End')
      await field.pressSequentially('z')
      await expect(field).toHaveValue('a'.repeat(2000))
      await expect(dialog).toContainText('2000 / 2000 characters')
      await field.fill(note)
      await dialog.getByRole('button', { name: 'Dismiss version 2' }).click()
      await expect(dialog).toHaveCount(0)
      expect(body).toBe(note.trim() ? JSON.stringify({ note: note.trim() }) : null)
    })
  }
}
