import { expect, test } from '@playwright/test'
import {
  alignedParagraphs,
  compareDocuments,
  formattedParagraph,
  formattedParagraphChanges,
  headingBlocks,
  reviewBlocks,
  wordChanges,
} from '../src/components/documents/document-review-model'

test('review model preserves stable identifiers, headings, moves and bounded complete sources', () => {
  const before = {
    content: 'Before prose',
    statements: [
      {
        id: 'REQ-1',
        statement: 'Renew every 30 seconds.',
        acceptance_criteria: [{ id: 'AC-1.1', statement: 'Keep the lease.' }],
      },
    ],
  }
  const after = {
    ...before,
    content: 'After prose',
    statements: [{ ...before.statements[0], statement: 'Renew every 10 seconds.' }],
  }
  expect(reviewBlocks(after).map((block) => block.id)).toEqual(['heading:Overview:1', 'REQ-1', 'AC-1.1'])
  expect(compareDocuments(before, after).rows.find((row) => row.id === 'AC-1.1')?.changed).toBe(false)
  expect(wordChanges('every 30 seconds', 'every 10 seconds')).toEqual([
    { text: 'every ', kind: 'same' },
    { text: '30', kind: 'removed' },
    { text: '10', kind: 'added' },
    { text: ' seconds', kind: 'same' },
  ])
  expect(
    headingBlocks('Intro\n\n## Repeat\nOne\n\n```md\n# Not a heading\n```\n\n## Repeat\nTwo').map(
      (block) => block.title,
    ),
  ).toEqual(['Overview', 'Repeat', 'Repeat (2)'])
  const moved = compareDocuments(
    { content: '# A\nAlpha\n# B\nBeta\n# Removed\nGone' },
    { content: '# B\nBeta\n# Added\nNew\n# A\nAlpha' },
  )
  expect(moved.rows.filter((row) => row.moved)).toHaveLength(2)
  expect(moved.rows.find((row) => row.title === 'Removed')?.after).toBeUndefined()
  expect(moved.rows.find((row) => row.title === 'Added')?.before).toBeUndefined()
  expect(alignedParagraphs('First\n\nLast', 'First\n\nInserted\n\nLast')).toEqual([
    { before: 'First', after: 'First' },
    { before: undefined, after: 'Inserted' },
    { before: 'Last', after: 'Last' },
  ])
  expect(headingBlocks('Heading\n=======\nBody\n\nSecond\n------\nMore').map((block) => block.title)).toEqual([
    'Heading',
    'Second',
  ])
  expect(compareDocuments({ content: '' }, { content: '' }).rows).toEqual([])
  expect(compareDocuments(before, before).rows.every((row) => !row.changed)).toBe(true)
  const large = `${'word '.repeat(30_000)}LAST SENTINEL`
  const fallback = compareDocuments({ content: '' }, { content: large })
  expect(fallback.limited).toBe(true)
  expect(fallback.rightText).toBe(large)
  expect(fallback.rows).toEqual([])
  expect(compareDocuments({ content: 'one '.repeat(300) }, { content: 'two '.repeat(300) }).limited).toBe(true)
  const formatted = formattedParagraphChanges(
    'Keep `unavailable`, *slow*, **manual**, and [the guide](https://example.test/old).',
    'Keep `unavailable`, *fast*, **automatic**, and [the guide](https://example.test/new).',
  )
  expect(formatted).toContainEqual({ text: 'unavailable', kind: 'same', style: 'code' })
  expect(formatted).toContainEqual({ text: 'slow', kind: 'removed', style: 'emphasis' })
  expect(formatted).toContainEqual({ text: 'fast', kind: 'added', style: 'emphasis' })
  expect(formatted).toContainEqual({ text: 'manual', kind: 'removed', style: 'strong' })
  expect(formatted).toContainEqual({ text: 'automatic', kind: 'added', style: 'strong' })
  expect(formatted).toContainEqual({
    text: 'the guide',
    kind: 'removed',
    style: 'link',
    href: 'https://example.test/old',
  })
  expect(formatted).toContainEqual({
    text: 'the guide',
    kind: 'added',
    style: 'link',
    href: 'https://example.test/new',
  })
  expect(formattedParagraph('[unsafe](javascript:alert(1))')).toBeUndefined()
  expect(formattedParagraph('*unterminated')).toBeUndefined()
  expect(formattedParagraph('- list item')).toBeUndefined()
  expect(formattedParagraph('<script>window.injected = true</script>')).toBeUndefined()
  expect(formattedParagraphChanges('one '.repeat(300), 'two '.repeat(300))).toBeUndefined()
})

import type { Page } from '@playwright/test'

type Tier = 'requirements' | 'system-design'
async function seedReview(
  page: Page,
  tier: Tier,
  options: {
    first?: boolean
    archived?: boolean
    reader?: boolean
    content?: string
    baseContent?: string
    targetContent?: string
    same?: boolean
  } = {},
) {
  await page.addInitScript(() => localStorage.setItem('conveyor-workspace', 'demo'))
  const base =
    '# Overview\n\nShared unchanged introduction.\n\n## Claim lifecycle\n\nRenew every 30 seconds.\n\nKeep the claim.\n\n## Removed\n\nOld policy.\n\n## Repeat\n\nFirst occurrence.\n\n## Repeat\n\nSecond occurrence.'
  const proposal =
    '# Overview\n\nShared unchanged introduction.\n\n## Claim lifecycle\n\nRenew every 10 seconds.\n\nInserted paragraph.\n\nKeep the claim.\n\n## Added\n\nNew policy.\n\n## Repeat\n\nFirst occurrence.\n\n## Repeat\n\nChanged second occurrence.'
  const makeVersion = (version: number, content: string) => ({
    requirement_id: 'doc-review',
    document_id: 'doc-review',
    workspace: 'demo',
    version,
    content,
    statements:
      tier === 'requirements'
        ? [
            {
              id: 'REQ-1',
              statement: 'The executor owns its claim.',
              acceptance_criteria: [
                { id: 'AC-1.1', statement: version === 1 ? 'Renew every 30 seconds.' : 'Renew every 10 seconds.' },
              ],
            },
          ]
        : undefined,
    governs: [],
    confirmed: version === 1,
    retired: version === 4,
    dismissed: version === 4,
    origin: 'operator',
    created_at: '2026-09-17T08:00:00Z',
    ...(version === 4
      ? {
          dismissed_by: 'Alex',
          retired_by: 'Alex',
          dismissed_at: '2026-09-17T09:00:00Z',
          retired_at: '2026-09-17T09:00:00Z',
          dismissal_note: 'Keep the existing behavior.',
        }
      : {}),
  })
  const versions = options.first
    ? [makeVersion(2, options.content ?? proposal)]
    : [
        makeVersion(1, options.baseContent ?? options.content ?? base),
        makeVersion(2, options.targetContent ?? options.content ?? (options.same ? base : proposal)),
        makeVersion(3, '# Newest proposal\n\nUnrelated newest text.'),
        makeVersion(4, '# Historical\n\nDismissed proposal.'),
      ]
  if (options.same) versions[1].statements = versions[0].statements
  if (options.content !== undefined || options.baseContent !== undefined || options.targetContent !== undefined)
    for (const v of versions) v.statements = []
  let current = options.first ? undefined : versions[0]
  const calls: Array<{ path: string; body: unknown }> = []
  let refuse = false
  const detail = (workspace: string, id = 'doc-review') => {
    const document = {
      id,
      slug: id,
      title: workspace === 'beta' ? 'Beta review' : id === 'doc-other' ? 'Other review' : 'Claim ownership',
      category: 'Architecture',
      workspace,
      current_version: current?.version,
      archived: options.archived ?? false,
      created_at: '2026-09-17T08:00:00Z',
      updated_at: '2026-09-17T08:00:00Z',
    }
    const scoped = versions.map((v) =>
      workspace === 'beta' || id === 'doc-other'
        ? { ...v, content: '# Different document\n\nScoped content.', statements: [] }
        : v,
    )
    return {
      requirement: document,
      document,
      current_version: current ? scoped.find((v) => v.version === current?.version) : undefined,
      pending_versions: scoped.filter((v) => !v.confirmed && !v.retired && !v.dismissed),
      versions: scoped,
      serving_tasks: [],
      serving_blueprints: [],
      planning_sessions: [],
      artifacts: [],
      lineage: [],
      lineage_total: 0,
      drift: [],
      confirmation_eligible: true,
      staleness: { deliveries: [], active_drift: [] },
    }
  }
  await page.route('**/v1/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const workspace = url.searchParams.get('workspace_id') ?? 'demo'
    if (path === '/v1/me')
      return route.fulfill({ json: { id: 'operator', role: options.reader ? 'viewer' : 'operator' } })
    if (path === '/v1/workspaces')
      return route.fulfill({
        json: [
          { id: 'demo', name: 'Demo' },
          { id: 'beta', name: 'Beta' },
        ],
      })
    if (path === '/v1/workspace') return route.fulfill({ json: { workspace, repos: ['conveyor'] } })
    if (path === '/v1/pending-proposals') return route.fulfill({ json: { proposals: [], total: 0 } })
    if (path === `/v1/${tier === 'requirements' ? 'requirements' : 'system-designs'}`)
      return route.fulfill({
        json: ['doc-review', 'doc-other'].map((id) => ({
          ...detail(workspace, id),
          pending_version_count: 2,
          drift_count: 0,
        })),
      })
    if (/\/doc-(review|other)$/.test(path))
      return route.fulfill({ json: detail(workspace, path.endsWith('doc-other') ? 'doc-other' : 'doc-review') })
    if (path.endsWith('/versions'))
      return route.fulfill({
        json: detail(workspace, path.includes('doc-other') ? 'doc-other' : 'doc-review').versions,
      })
    if (request.method() === 'POST' && /\/(confirm|dismiss)$/.test(path)) {
      calls.push({ path, body: request.postDataJSON() })
      if (refuse) return route.fulfill({ status: 409, body: 'The selected version changed; refresh and retry.' })
      const number = Number(path.match(/versions\/(\d+)/)?.[1])
      const selected = versions.find((v) => v.version === number)
      if (!selected) throw new Error('Unexpected version')
      if (path.endsWith('dismiss')) {
        selected.retired = true
        selected.dismissed = true
        selected.dismissal_note = (request.postDataJSON() as { note?: string })?.note
      } else {
        selected.confirmed = true
        current = selected
        for (const v of versions)
          if (v.version < number && !v.confirmed) {
            v.retired = true
            v.dismissed = true
          }
      }
      return route.fulfill({ json: { version: selected, document: detail(workspace).document } })
    }
    return route.fulfill({ json: [] })
  })
  return {
    calls,
    setRefuse: (value: boolean) => {
      refuse = value
    },
    url: `/${tier}?${tier === 'requirements' ? 'requirement' : 'document'}=doc-review&tab=changes&target=2`,
  }
}

for (const tier of ['requirements', 'system-design'] as const) {
  test(`${tier}: exact proposal, selectors, comparison navigation, history and workspace isolation`, async ({
    page,
  }) => {
    const seed = await seedReview(page, tier)
    await page.goto(seed.url)
    await expect(page.getByRole('tab', { name: 'changes', exact: true })).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByLabel('Target version', { exact: true })).toHaveValue('2')
    await expect(page.getByRole('button', { name: 'Confirm version 2', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Confirm version 3', exact: true })).toHaveCount(0)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    await expect(comparison.locator('ins').filter({ hasText: '10' }).first()).toBeVisible()
    await comparison.getByText('Show unchanged section · Overview', { exact: true }).click()
    await expect(comparison.getByText('Shared unchanged introduction.', { exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Next', exact: true }).click()
    await expect(comparison.locator('section:focus')).toHaveCount(1)
    await page.getByRole('button', { name: 'Previous', exact: true }).click()
    await page.getByRole('button', { name: 'Side by side', exact: true }).click()
    await expect(comparison.getByText('No section in this version').first()).toBeVisible()
    await page.getByLabel('Base version', { exact: true }).selectOption('0')
    await expect(page.getByText('Comparing against an empty base.')).toBeVisible()
    await page.getByLabel('Target version', { exact: true }).selectOption('4')
    await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
    await page.getByRole('tab', { name: 'history', exact: true }).click()
    await expect(page.getByText("Operator's reason: Keep the existing behavior.")).toBeVisible()
    await page.getByRole('button', { name: 'Compare version 1', exact: true }).click()
    await expect(page.getByLabel('Target version', { exact: true })).toHaveValue('1')
    await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
    await page
      .getByRole('navigation', { name: 'Document tree' })
      .getByRole('button', { name: /Other review/ })
      .click()
    await expect(page.getByRole('tab', { name: 'document', exact: true })).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByText('Scoped content.')).toBeVisible()
    await page.getByRole('button', { name: 'Switch to Beta' }).click()
    await page
      .getByRole('link', { name: tier === 'requirements' ? 'Requirements' : 'System Design', exact: true })
      .click()
    await expect(page.getByRole('heading', { name: 'Beta review', exact: true })).toBeVisible()
    await expect(page.getByText('Claim ownership', { exact: true })).toHaveCount(0)
    expect(seed.calls).toEqual([])
  })

  test(`${tier}: dismiss cancel, optional note, refusal and successful selected-version mutation`, async ({ page }) => {
    const seed = await seedReview(page, tier)
    await page.goto(seed.url)
    await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Dismiss version 2 of Claim ownership' })
    await expect(dialog).toContainText('cannot be confirmed later')
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
    expect(seed.calls).toHaveLength(0)
    await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
    seed.setRefuse(true)
    await dialog.getByRole('button', { name: 'Dismiss version 2', exact: true }).click()
    await expect(dialog).toContainText('selected version changed')
    seed.setRefuse(false)
    await dialog.getByRole('textbox').fill('Please retain the expiry rule.')
    await dialog.getByRole('button', { name: 'Dismiss version 2', exact: true }).click()
    await expect(dialog).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
    expect(seed.calls.at(-1)?.path).toContain('/versions/2/dismiss')
    expect(seed.calls.at(-1)?.body).toEqual({ note: 'Please retain the expiry rule.' })
    await page.getByRole('button', { name: 'Review changes · v3', exact: true }).click()
    await page.getByRole('button', { name: 'Confirm version 3', exact: true }).click()
    await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
    expect(seed.calls.at(-1)?.path).toContain('/versions/3/confirm')
  })

  test(`${tier}: first proposal, no-note dismissal, invalid selection and keyboard tabs`, async ({ page }) => {
    const seed = await seedReview(page, tier, { first: true })
    await page.goto(seed.url)
    await expect(page.getByText('Comparing against an empty base: no confirmed version exists yet.')).toBeVisible()
    await page.getByRole('tab', { name: 'changes', exact: true }).focus()
    await page.keyboard.press('ArrowRight')
    await expect(page.getByRole('tab', { name: 'history', exact: true })).toBeFocused()
    await page.getByRole('button', { name: 'Compare version 2', exact: true }).click()
    await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Dismiss version 2', exact: true }).click()
    expect(seed.calls[0]?.body ?? {}).toEqual({})
    await page.goto(`${seed.url.replace('target=2', 'target=99')}`)
    await expect(page.getByRole('alert')).toContainText('selected version is unavailable')
    await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
    await page.getByRole('button', { name: 'Reset version selection' }).click()
    await expect(page.getByRole('alert')).toHaveCount(0)
  })

  for (const state of ['reader', 'archived'] as const)
    test(`${tier}: ${state} selection remains readable without proposal mutations`, async ({ page }) => {
      const seed = await seedReview(page, tier, { [state]: true })
      await page.goto(seed.url)
      await expect(page.getByRole('region', { name: 'Version comparison', exact: true })).toBeVisible()
      await expect(page.getByRole('button', { name: /Confirm version|^Dismiss$|^Revise$/ })).toHaveCount(0)
    })

  test(`${tier}: empty, identical, rich Markdown and complete large fallback`, async ({ page }) => {
    for (const content of [
      '',
      '# Stable\n\nNo change.',
      '# Rich\n\n- list item\n\n| Field | Value |\n| --- | --- |\n| Lease | 10 |\n\n```go\nrenew()\n```\n\n```mermaid\nflowchart LR\n A-->B\n```',
      `${'large input '.repeat(12_000)}LAST SENTINEL`,
    ]) {
      await page.unrouteAll({ behavior: 'wait' })
      const seed = await seedReview(page, tier, { content })
      await page.goto(seed.url)
      const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
      if (content.length > 120_000) {
        await expect(comparison).toContainText('both complete versions without highlighting')
        await expect(comparison.locator('pre').last()).toContainText('LAST SENTINEL')
        await expect(page.getByRole('navigation', { name: 'Changed sections' })).toHaveCount(0)
      } else {
        await expect(comparison).toContainText('No changes between these versions.')
        if (!content) await expect(comparison).toContainText('Both versions are empty.')
        else {
          await comparison.locator('summary').click()
          if (content.includes('# Rich')) {
            await expect(comparison.getByRole('table')).toBeVisible()
            await expect(comparison.locator('code').filter({ hasText: 'renew()' })).toBeVisible()
            await expect(comparison.locator('[data-mermaid] svg')).toBeVisible()
          }
        }
      }
    }
  })

  test(`${tier}: desktop and narrow review evidence`, async ({ page }, testInfo) => {
    const seed = await seedReview(page, tier)
    await page.goto(seed.url)
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 1000 })
      await expect(page.getByRole('button', { name: 'Confirm version 2', exact: true })).toBeVisible()
      for (const name of ['Confirm version 2', 'Dismiss', 'Revise']) {
        const action = page.getByRole('button', { name, exact: true })
        await action.scrollIntoViewIfNeeded()
        expect(
          await action.evaluate((node) => {
            const box = node.getBoundingClientRect()
            return box.left >= 0 && box.right <= innerWidth
          }),
        ).toBe(true)
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      await page.getByRole('button', { name: 'Inline', exact: true }).click()
      await page.getByRole('navigation', { name: 'Changed sections' }).getByRole('button').first().click()
      await page.screenshot({ path: testInfo.outputPath(`${tier}-inline-${width}.png`), fullPage: true })
      await page.getByRole('button', { name: 'Side by side', exact: true }).click()
      await page.getByRole('navigation', { name: 'Changed sections' }).getByRole('button').first().click()
      await page.screenshot({ path: testInfo.outputPath(`${tier}-side-by-side-${width}.png`), fullPage: true })
      await page.getByRole('tab', { name: 'history', exact: true }).click()
      await page.screenshot({ path: testInfo.outputPath(`${tier}-history-${width}.png`), fullPage: true })
      await page.getByRole('tab', { name: 'changes', exact: true }).click()
    }
  })

  test(`${tier}: seeded formatted paragraphs retain semantics in both comparison modes`, async ({ page }, testInfo) => {
    const base =
      '# Fail-open entitlement policy\n\nIf Billing returns nil or `unavailable`, AI and Middleware proceed. Only a confirmed usable subscription that lacks the named feature takes the fallback path.\n\nUse `legacy`, *slow*, **manual**, and [the old guide](https://example.test/old).'
    const target =
      '# Fail-open entitlement policy\n\nIf Billing returns nil or `unavailable`, AI and Middleware proceed under the visitor fail-open policy. Only a confirmed usable subscription that lacks the named feature takes the fallback path.\n\nUse `current`, *fast*, **automatic**, and [the new guide](https://example.test/new).'
    const seed = await seedReview(page, tier, { baseContent: base, targetContent: target })
    await page.goto(seed.url)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    const unchangedCode = comparison.locator('code').filter({ hasText: 'unavailable' }).first()
    await expect(unchangedCode).toBeVisible()
    expect(await unchangedCode.evaluate((node) => node.closest('ins, del') === null)).toBe(true)
    await expect(comparison.locator('ins').filter({ hasText: 'under the visitor fail-open policy' })).toBeVisible()
    await expect(comparison.locator('del code').filter({ hasText: 'legacy' })).toBeVisible()
    await expect(comparison.locator('ins code').filter({ hasText: 'current' })).toBeVisible()
    await expect(comparison.locator('del em').filter({ hasText: 'slow' })).toBeVisible()
    await expect(comparison.locator('ins em').filter({ hasText: 'fast' })).toBeVisible()
    await expect(comparison.locator('del strong').filter({ hasText: 'manual' })).toBeVisible()
    await expect(comparison.locator('ins strong').filter({ hasText: 'automatic' })).toBeVisible()
    await expect(comparison.locator('del a[href="https://example.test/old"]')).toHaveText('the old guide')
    await expect(comparison.locator('ins a[href="https://example.test/new"]')).toHaveText('the new guide')
    await page.screenshot({ path: testInfo.outputPath(`${tier}-seeded-formatted-inline.png`), fullPage: true })
    await page.getByRole('button', { name: 'Side by side', exact: true }).click()
    await expect(comparison.locator('del code').filter({ hasText: 'legacy' })).toBeVisible()
    await expect(comparison.locator('ins code').filter({ hasText: 'current' })).toBeVisible()
    await page.screenshot({
      path: testInfo.outputPath(`${tier}-seeded-formatted-before-after-side-by-side.png`),
      fullPage: true,
    })
  })
}

for (const tier of ['requirements', 'system-design'] as const) {
  test(`${tier}: late document response cannot replace the selected document`, async ({ page }) => {
    const seed = await seedReview(page, tier)
    let release: (() => void) | undefined
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    let requested = false
    await page.route(
      `**/v1/${tier === 'requirements' ? 'requirements' : 'system-designs'}/doc-review?*`,
      async (route) => {
        requested = true
        await held
        await route.fallback()
      },
    )
    await page.goto(seed.url)
    await expect.poll(() => requested).toBe(true)
    await page
      .getByRole('navigation', { name: 'Document tree' })
      .getByRole('button', { name: /Other review/ })
      .click()
    await expect(page.getByText('Scoped content.', { exact: true })).toBeVisible()
    release?.()
    await expect(page.getByRole('heading', { name: 'Other review', exact: true })).toBeVisible()
    await expect(page.getByText('Renew every 10 seconds.', { exact: true })).toHaveCount(0)
  })

  test(`${tier}: selection changes discard dialogs and changed rich blocks retain safe Markdown`, async ({ page }) => {
    const seed = await seedReview(page, tier, {
      first: true,
      content:
        '# Rich\n\n- list item\n\n| Name | Value |\n| --- | --- |\n| Lease | 10 |\n\n```go\nrenew()\n```\n\n```mermaid\nflowchart LR\n A-->B\n```\n\n<script>window.injected = true</script>',
    })
    await page.goto(seed.url)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    await expect(comparison.getByRole('table')).toBeVisible()
    await expect(comparison.locator('[data-mermaid] svg')).toBeVisible()
    await expect(comparison.locator('script')).toHaveCount(0)
    await expect(comparison).toContainText('Detailed highlighting unavailable for this block')
    await page.getByRole('tab', { name: 'document', exact: true }).click()
    await page.getByRole('tab', { name: 'changes', exact: true }).click()
    await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
    await page.getByRole('dialog').getByRole('textbox').fill('unsent note')
    await page.goBack()
    await expect(page.getByRole('tab', { name: 'document', exact: true })).toHaveAttribute('aria-selected', 'true')
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(seed.calls).toHaveLength(0)
  })
}
