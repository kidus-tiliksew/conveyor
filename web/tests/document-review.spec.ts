import { expect, test } from '@playwright/test'
import {
  alignedParagraphs,
  compareDocuments,
  formattedParagraph,
  formattedParagraphChanges,
  headingBlocks,
  paragraphCells,
  reviewBlocks,
  reviewRowParagraphs,
  wordChanges,
} from '../src/components/documents/document-review-model'

const vocabulary =
  'the worker renews its claim before the lease expires and reports progress while review waits for delivery evidence from each attempt'.split(
    ' ',
  )
// Deterministic prose: `words` words in sentences of twelve, varied by `seed`.
function prose(seed: number, words: number) {
  const list = Array.from({ length: words }, (_, i) => vocabulary[(seed * 31 + i * 7 + (i >> 3)) % vocabulary.length])
  return Array.from({ length: Math.ceil(words / 12) }, (_, n) => `${list.slice(n * 12, n * 12 + 12).join(' ')}.`).join(
    ' ',
  )
}
// A realistic design: eight sections of four 100-word paragraphs (400 words).
function longDesign(edit?: (section: number, paragraph: number, text: string) => string) {
  return Array.from(
    { length: 8 },
    (_, section) =>
      `## Section ${section + 1}\n\n${Array.from({ length: 4 }, (_, paragraph) => {
        const text = prose(section * 4 + paragraph, 100)
        return edit ? edit(section, paragraph, text) : text
      }).join('\n\n')}`,
  ).join('\n\n')
}
const changedSentence = (section: number, paragraph: number, text: string) =>
  section === 5 && paragraph === 1 ? `${text} Operators confirm every pending revision.` : text
// One paragraph pair costs 470 x 470 = 220,900 token cells, under the
// per-matrix guard; three pairs exceed the section limit and two do not.
const heavyParagraph = (word: string) => `${word} `.repeat(235).trim()
const heavySection = (title: string, word: string, count: number) =>
  `## ${title}\n\n${Array.from({ length: count }, (_, n) => `${heavyParagraph(word)} ${n}`).join('\n\n')}`
// A changed pair of 621 tokens a side costs 622 x 622 = 386,884 token cells:
// above the former 250,000-cell matrix guard, below the 500,000-cell section
// budget, so it keeps detailed highlighting. The 1,621-token pair costs
// 2,630,884 cells, above the 2,500,000-cell guard, and renders complete content
// in its own block.
const detailedCells = 622 * 622
const oversizedCells = 1622 * 1622
const guardParagraph = (side: 'base' | 'target') =>
  `Shared opening for the matrix guard. ${(side === 'base' ? 'one ' : 'two ').repeat(300)}${side === 'base' ? 'Base' : 'Target'} detailed ending sentinel.`
const oversizedParagraph = (side: 'base' | 'target') =>
  `Shared opening for the oversized block. ${(side === 'base' ? 'one ' : 'two ').repeat(800)}${side === 'base' ? 'Base' : 'Target'} oversized ending sentinel.`
const guardDocument = (side: 'base' | 'target') =>
  `# Overview\n\nShared unchanged introduction.\n\n## Detailed revision\n\n${guardParagraph(side)}\n\n## Oversized block\n\n${oversizedParagraph(side)}\n\n## Sibling\n\nRenew every ${side === 'base' ? 30 : 10} seconds.\n\n## Stable\n\nSame.`

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
  // One oversized paragraph pair takes its per-block fallback; the document keeps detailed comparison.
  const single = compareDocuments({ content: 'one '.repeat(800) }, { content: 'two '.repeat(800) })
  expect(single.limited).toBe(false)
  expect(single.rows).toMatchObject([{ id: 'heading:Overview:1', changed: true, limited: false }])
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
  expect(formattedParagraphChanges('one '.repeat(800), 'two '.repeat(800))).toBeUndefined()
  expect(paragraphCells('one '.repeat(300), 'one '.repeat(300))).toBe(0)
  expect(formattedParagraphChanges('one '.repeat(300), 'one '.repeat(300))).toEqual([
    { text: 'one '.repeat(300), kind: 'same', style: 'text' },
  ])
})

test('review model budgets renderer work per changed section', () => {
  const base = longDesign()
  const target = longDesign(changedSentence)
  const realistic = compareDocuments({ content: base }, { content: target })
  expect(realistic.limited).toBe(false)
  expect(realistic.rows).toHaveLength(8)
  expect(realistic.rows.filter((row) => row.changed).map((row) => row.title)).toEqual(['Section 6'])
  const edited = realistic.rows.find((row) => row.title === 'Section 6')
  expect(edited).toMatchObject({ changed: true, limited: false })
  const pairs = edited ? reviewRowParagraphs(edited) : []
  expect(pairs).toHaveLength(4)
  const changedPair = pairs.find((pair) => pair.before !== pair.after)
  expect(formattedParagraphChanges(changedPair?.before ?? '', changedPair?.after ?? '')).toContainEqual({
    text: ' Operators confirm every pending revision.',
    kind: 'added',
    style: 'text',
  })

  // Alignment and identical paragraphs cost only the paragraph matrix; each
  // changed pair adds its token matrix.
  expect(paragraphCells('Renew every 30 seconds.', 'Renew every 30 seconds.')).toBe(0)
  expect(paragraphCells('Renew every 30 seconds.', 'Renew every 10 seconds.')).toBe(9 * 9)
  expect(paragraphCells('', 'Added.')).toBe(3)
  expect(paragraphCells('- list item', '- other item')).toBe(0)
  expect(paragraphCells(heavyParagraph('a'), heavyParagraph('b'))).toBe(470 * 470)

  const isolated = compareDocuments(
    {
      content: `${heavySection('Oversized', 'alpha', 3)}\n\n## Sibling\n\nRenew every 30 seconds.\n\n## Stable\n\nSame.`,
    },
    {
      content: `${heavySection('Oversized', 'beta', 3)}\n\n## Sibling\n\nRenew every 10 seconds.\n\n## Stable\n\nSame.`,
    },
  )
  expect(isolated.limited).toBe(false)
  expect(isolated.rows.map(({ title, changed, limited }) => ({ title, changed, limited }))).toEqual([
    { title: 'Oversized', changed: true, limited: true },
    { title: 'Sibling', changed: true, limited: false },
    { title: 'Stable', changed: false, limited: false },
  ])
  expect(isolated.rows[0].before?.content).toContain(heavyParagraph('alpha'))
  expect(isolated.rows[0].after?.content).toContain(heavyParagraph('beta'))
  const sibling = reviewRowParagraphs(isolated.rows[1])
  expect(formattedParagraphChanges(sibling[0].before ?? '', sibling[0].after ?? '')).toContainEqual({
    text: '10',
    kind: 'added',
    style: 'text',
  })

  // Five sections under the section limit together exceed the document ceiling.
  const heavyDocument = (word: string) =>
    Array.from({ length: 5 }, (_, n) => heavySection(`Heavy ${n}`, word, 2)).join('\n\n')
  const aggregate = compareDocuments({ content: heavyDocument('alpha') }, { content: heavyDocument('beta') })
  expect(aggregate.limited).toBe(true)
  expect(aggregate.leftText).toBe(heavyDocument('alpha'))
  expect(aggregate.rightText).toBe(heavyDocument('beta'))
  expect(aggregate.rows).toEqual([])
  const four = (word: string) => Array.from({ length: 4 }, (_, n) => heavySection(`Heavy ${n}`, word, 2)).join('\n\n')
  const underCeiling = compareDocuments({ content: four('alpha') }, { content: four('beta') })
  expect(underCeiling.limited).toBe(false)
  expect(underCeiling.rows.every((row) => row.changed && !row.limited)).toBe(true)
})

test('review model consumers share the 2,500,000-cell matrix guard', () => {
  // Plain 300-word pairs cost 601 x 601 = 361,201 cells; 800-word pairs cost 1601 x 1601 = 2,563,201.
  expect(paragraphCells('one '.repeat(300), 'two '.repeat(300))).toBe(601 * 601)
  expect(paragraphCells('one '.repeat(800), 'two '.repeat(800))).toBe(0)
  // Separating spaces match, so each word is removed and replaced in place.
  expect(wordChanges('one '.repeat(300), 'two '.repeat(300))).toEqual(
    Array.from({ length: 300 }, () => [
      { text: 'one', kind: 'removed' },
      { text: 'two', kind: 'added' },
      { text: ' ', kind: 'same' },
    ]).flat(),
  )
  expect(wordChanges('one '.repeat(800), 'two '.repeat(800))).toBeUndefined()

  expect(paragraphCells(guardParagraph('base'), guardParagraph('target'))).toBe(detailedCells)
  expect(detailedCells).toBeGreaterThan(250_000)
  expect(detailedCells).toBeLessThan(500_000)
  const detailed = formattedParagraphChanges(guardParagraph('base'), guardParagraph('target'))
  expect(detailed).toContainEqual({ text: 'Shared opening for the matrix guard. ', kind: 'same', style: 'text' })
  expect(detailed).toContainEqual({ text: 'Target', kind: 'added', style: 'text' })
  expect(detailed).toContainEqual({ text: 'Base', kind: 'removed', style: 'text' })
  expect(paragraphCells(oversizedParagraph('base'), oversizedParagraph('target'))).toBe(0)
  expect(oversizedCells).toBeGreaterThan(2_500_000)
  expect(formattedParagraphChanges(oversizedParagraph('base'), oversizedParagraph('target'))).toBeUndefined()

  // Formatted-inline pairs: a styled run on both sides of the guard.
  const styled = (words: number, word: string) => `Use \`renew\` and **${word}**. ${`${word} `.repeat(words)}`
  expect(formattedParagraphChanges(styled(300, 'slow'), styled(300, 'fast'))).toContainEqual({
    text: 'fast',
    kind: 'added',
    style: 'strong',
  })
  expect(formattedParagraphChanges(styled(800, 'slow'), styled(800, 'fast'))).toBeUndefined()

  // Paragraph alignment: 600 paragraphs a side (361,201 cells) align; 1,600
  // paragraphs a side (2,563,201 cells) return the complete sources as one pair.
  const numbered = (prefix: string, count: number) =>
    Array.from({ length: count }, (_, n) => `${prefix}${n}`).join('\n\n')
  const aligned = alignedParagraphs(numbered('a', 600), numbered('a', 599))
  expect(aligned).toHaveLength(600)
  expect(aligned.at(-1)).toEqual({ before: 'a599', after: undefined })
  expect(alignedParagraphs(numbered('a', 1600), numbered('b', 1600))).toEqual([
    { before: numbered('a', 1600), after: numbered('b', 1600) },
  ])
  const sectionOf = (count: number, prefix: string) => `## Many\n\n${numbered(prefix, count)}`
  const manyAligned = compareDocuments({ content: sectionOf(600, 'a') }, { content: sectionOf(600, 'b') })
  expect(manyAligned.rows).toMatchObject([{ title: 'Many', changed: true, limited: false }])
  const manyLimited = compareDocuments({ content: sectionOf(1600, 'a') }, { content: sectionOf(1600, 'b') })
  expect(manyLimited.rows).toMatchObject([{ title: 'Many', changed: true, limited: true }])

  // The guard document keeps detailed comparison: only the detailed pair adds
  // token cells, so the section and document budgets are not what decides it.
  const guard = compareDocuments({ content: guardDocument('base') }, { content: guardDocument('target') })
  expect(guard.limited).toBe(false)
  expect(guard.rows.map(({ title, changed, limited }) => ({ title, changed, limited }))).toEqual([
    { title: 'Overview', changed: false, limited: false },
    { title: 'Detailed revision', changed: true, limited: false },
    { title: 'Oversized block', changed: true, limited: false },
    { title: 'Sibling', changed: true, limited: false },
    { title: 'Stable', changed: false, limited: false },
  ])
  expect(guardDocument('base').length + guardDocument('target').length).toBe(9366)
})

import type { Page } from '@playwright/test'
import { callerIdentity } from './helpers/caller-identity'

type Tier = 'requirements' | 'system-design'
async function seedReview(
  page: Page,
  tier: Tier,
  options: {
    first?: boolean
    historicalState?: 'dismissed' | 'superseded'
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
  if (options.historicalState)
    Object.assign(versions[0], {
      retired: true,
      dismissed: true,
      retired_by_version: options.historicalState === 'superseded' ? 3 : undefined,
    })
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
      return route.fulfill({ json: callerIdentity({ id: 'operator', role: options.reader ? 'viewer' : 'operator' }) })
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
    await expect(
      page
        .getByRole('heading', { name: 'Claim ownership', exact: true })
        .locator('..')
        .getByText('No confirmed version', { exact: true }),
    ).toBeVisible()
    await expect(
      page
        .getByRole('heading', { name: 'Claim ownership', exact: true })
        .locator('..')
        .getByText('v2 · Proposed', { exact: true }),
    ).toBeVisible()
    await expect(page.getByRole('region', { name: 'Needs your attention' })).toContainText(
      'Dismissing this version archives the document.',
    )
    await expect(page.getByText('Comparing against an empty base: no confirmed version exists yet.')).toBeVisible()
    await page.getByRole('tab', { name: 'changes', exact: true }).focus()
    await page.keyboard.press('ArrowRight')
    await expect(page.getByRole('tab', { name: 'history', exact: true })).toBeFocused()
    await page.getByRole('button', { name: 'Compare version 2', exact: true }).click()
    await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Dismiss version 2', exact: true }).click()
    expect(seed.calls[0]?.body ?? {}).toEqual({})
    await expect(
      page
        .getByRole('heading', { name: 'Claim ownership', exact: true })
        .locator('..')
        .getByText('v2 · Dismissed', { exact: true }),
    ).toBeVisible()
    await page.goto(`${seed.url.replace('target=2', 'target=99')}`)
    await expect(page.getByRole('alert')).toContainText('selected version is unavailable')
    await expect(page.getByRole('button', { name: /Confirm version/ })).toHaveCount(0)
    await page.getByRole('button', { name: 'Reset version selection' }).click()
    await expect(page.getByRole('alert')).toHaveCount(0)
  })

  for (const historicalState of ['dismissed', 'superseded'] as const)
    test(`${tier}: header names ${historicalState} selected history without confirmed authority`, async ({ page }) => {
      const seed = await seedReview(page, tier, { first: true, archived: true, historicalState })
      await page.goto(seed.url.replace('tab=changes', 'tab=document'))
      await expect(
        page
          .getByRole('heading', { name: 'Claim ownership', exact: true })
          .locator('..')
          .getByText('No confirmed version', { exact: true }),
      ).toBeVisible()
      await expect(
        page
          .getByRole('heading', { name: 'Claim ownership', exact: true })
          .locator('..')
          .getByText(historicalState === 'dismissed' ? 'v2 · Dismissed' : 'v2 · Superseded by v3', { exact: true }),
      ).toBeVisible()
      await expect(page.getByRole('tabpanel')).toContainText('Renew every 10 seconds.')
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

  test(`${tier}: long-section revisions keep navigation and section-local fallback`, async ({ page }, testInfo) => {
    const longSeed = await seedReview(page, tier, {
      baseContent: longDesign(),
      targetContent: longDesign(changedSentence),
    })
    await page.goto(longSeed.url)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    const navigator = page.getByRole('navigation', { name: 'Changed sections' })
    await expect(
      comparison.locator('ins').filter({ hasText: 'Operators confirm every pending revision.' }),
    ).toBeVisible()
    await expect(comparison).not.toContainText('Diff too large')
    await expect(navigator.getByRole('button', { name: /^Section / })).toHaveText(['Section 6'])
    await expect(comparison.getByText('Show unchanged section · Section 1', { exact: true })).toBeVisible()
    await page.screenshot({ path: testInfo.outputPath(`${tier}-long-section-revision.png`), fullPage: true })

    await page.unrouteAll({ behavior: 'wait' })
    const mixedSeed = await seedReview(page, tier, {
      baseContent: `${heavySection('Oversized', 'alpha', 3)}\n\n## Sibling\n\nRenew every 30 seconds.`,
      targetContent: `${heavySection('Oversized', 'beta', 3)}\n\n## Sibling\n\nRenew every 10 seconds.`,
    })
    await page.goto(mixedSeed.url)
    await expect(navigator.getByRole('button', { name: /^(Oversized|Sibling)$/ })).toHaveText(['Oversized', 'Sibling'])
    const oversized = comparison.getByRole('region', { name: 'Oversized', exact: true })
    await expect(oversized).toContainText('Detailed highlighting unavailable for this block')
    await expect(oversized).toContainText(`${heavyParagraph('alpha')} 2`)
    await expect(oversized).toContainText(`${heavyParagraph('beta')} 2`)
    await expect(oversized.locator('ins, del')).toHaveCount(0)
    const sibling = comparison.getByRole('region', { name: 'Sibling', exact: true })
    await expect(sibling.locator('ins').filter({ hasText: '10' })).toBeVisible()
    await expect(sibling.locator('del').filter({ hasText: '30' })).toBeVisible()
    await navigator.getByRole('button', { name: 'Sibling', exact: true }).click()
    await expect(sibling).toBeFocused()
    await page.getByRole('button', { name: 'Side by side', exact: true }).click()
    await expect(oversized).toContainText('Detailed highlighting unavailable for this block')
    await expect(sibling.locator('ins').filter({ hasText: '10' })).toBeVisible()
    await page.screenshot({ path: testInfo.outputPath(`${tier}-section-local-fallback.png`), fullPage: true })
  })

  test(`${tier}: paragraph pairs above the former matrix guard keep highlighting and navigation`, async ({
    page,
  }, testInfo) => {
    const seed = await seedReview(page, tier, {
      baseContent: guardDocument('base'),
      targetContent: guardDocument('target'),
    })
    await page.goto(seed.url)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    const navigator = page.getByRole('navigation', { name: 'Changed sections' })
    const detailed = comparison.getByRole('region', { name: 'Detailed revision', exact: true })
    const oversized = comparison.getByRole('region', { name: 'Oversized block', exact: true })
    const sibling = comparison.getByRole('region', { name: 'Sibling', exact: true })
    await expect(comparison).not.toContainText('Diff too large')
    await expect(navigator.getByRole('button', { name: /^(Detailed revision|Oversized block|Sibling)$/ })).toHaveText([
      'Detailed revision',
      'Oversized block',
      'Sibling',
    ])
    for (const mode of ['Inline', 'Side by side'] as const) {
      await page.getByRole('button', { name: mode, exact: true }).click()
      await expect(page.getByRole('button', { name: mode, exact: true })).toHaveAttribute('aria-pressed', 'true')
      // 386,884 token cells: detailed highlighting above the former 250,000-cell guard.
      await expect(detailed).not.toContainText('Detailed highlighting unavailable')
      await expect(detailed.locator('ins').filter({ hasText: /^two$/ })).toHaveCount(300)
      await expect(detailed.locator('del').filter({ hasText: /^one$/ })).toHaveCount(300)
      await expect(detailed.locator('ins').filter({ hasText: /^Target$/ })).toBeVisible()
      await expect(detailed.locator('del').filter({ hasText: /^Base$/ })).toBeVisible()
      await expect(detailed).toContainText('detailed ending sentinel.')
      // 2,630,884 token cells: above the 2,500,000-cell guard, complete content in its own block.
      await expect(oversized).toContainText('Detailed highlighting unavailable for this block')
      await expect(oversized).toContainText(oversizedParagraph('base'))
      await expect(oversized).toContainText(oversizedParagraph('target'))
      await expect(oversized).toContainText('Base oversized ending sentinel.')
      await expect(oversized).toContainText('Target oversized ending sentinel.')
      await expect(oversized.locator('ins, del')).toHaveCount(0)
      await expect(sibling.locator('ins').filter({ hasText: '10' })).toBeVisible()
      await navigator.getByRole('button', { name: 'Sibling', exact: true }).click()
      await expect(sibling).toBeFocused()
      await navigator.getByRole('button', { name: 'Detailed revision', exact: true }).click()
      await expect(detailed).toBeFocused()
      await page.getByRole('button', { name: 'Next', exact: true }).click()
      await expect(oversized).toBeFocused()
      await page.getByRole('button', { name: 'Previous', exact: true }).click()
      await expect(detailed).toBeFocused()
      await page.screenshot({
        path: testInfo.outputPath(`${tier}-matrix-guard-${mode === 'Inline' ? 'inline' : 'side-by-side'}.png`),
        fullPage: true,
      })
    }
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

// Records Chromium timing and heap observations for the representative guard
// comparison. Durations depend on the host, so they are attached as evidence
// rather than asserted.
test('system-design: guard comparison timing and heap observations', async ({ page, browser }, testInfo) => {
  const samples = 10
  const seed = await seedReview(page, 'system-design', {
    baseContent: guardDocument('base'),
    targetContent: guardDocument('target'),
  })
  await page.goto(seed.url)
  const detailedSelector = 'section[aria-label="Detailed revision"]'
  await expect(page.locator(`${detailedSelector} ins`).first()).toBeVisible()
  const cdp = await page.context().newCDPSession(page)
  const heap = async (collect: boolean) => {
    if (collect) await cdp.send('HeapProfiler.collectGarbage')
    return cdp.send('Runtime.getHeapUsage')
  }
  const heapBefore = await heap(true)

  // Model work the renderer performs, executed in the browser through the dev server's module graph.
  const model = await page.evaluate(
    async ({ base, target, samples }) => {
      const path = '/src/components/documents/document-review-model.ts'
      const m = (await import(
        /* @vite-ignore */ path
      )) as typeof import('../src/components/documents/document-review-model')
      const time = (work: () => void) =>
        Array.from({ length: samples }, () => {
          const start = performance.now()
          work()
          return performance.now() - start
        })
      const comparison = m.compareDocuments({ content: base }, { content: target })
      const changed = comparison.rows.filter((row) => row.changed && !row.limited)
      const detailedPair = m.reviewRowParagraphs(comparison.rows.find((row) => row.title === 'Detailed revision')!)[0]
      return {
        compareDocuments: time(() => m.compareDocuments({ content: base }, { content: target })),
        renderParagraphs: time(() => {
          for (const row of changed)
            for (const pair of m.reviewRowParagraphs(row))
              m.formattedParagraphChanges(pair.before ?? '', pair.after ?? '')
        }),
        detailedPair: time(() => m.formattedParagraphChanges(detailedPair.before ?? '', detailedPair.after ?? '')),
      }
    },
    { base: guardDocument('base'), target: guardDocument('target'), samples },
  )
  const heapAfterModel = await heap(false)

  // Click inside the page and resolve when the DOM satisfies `ready`, then after the next frame.
  const measureClick = (button: string, ready: string) =>
    page.evaluate(
      ({ button, ready }) =>
        new Promise<{ dom: number; frame: number }>((resolve, reject) => {
          const target = [...document.querySelectorAll<HTMLElement>('[role="tab"], button')].find(
            (node) => node.textContent?.trim().toLowerCase() === button,
          )
          if (!target) return reject(new Error(`missing ${button}`))
          const check = () => {
            const section = document.querySelector('section[aria-label="Detailed revision"]')
            if (!section) return false
            if (ready === 'highlighted') return section.querySelector('ins') !== null
            const labeled = [...section.querySelectorAll('p')].some((node) => node.textContent === 'Base')
            return ready === 'side' ? labeled : !labeled && section.querySelector('ins') !== null
          }
          const start = performance.now()
          const finish = () => {
            const dom = performance.now() - start
            requestAnimationFrame(() => setTimeout(() => resolve({ dom, frame: performance.now() - start })))
          }
          const observer = new MutationObserver(() => {
            if (!check()) return
            observer.disconnect()
            finish()
          })
          observer.observe(document.body, { childList: true, subtree: true, attributes: true })
          target.click()
          if (check()) {
            observer.disconnect()
            finish()
          }
        }),
      { button, ready },
    )
  const readiness = []
  for (let n = 0; n < samples; n++) {
    await page.getByRole('tab', { name: 'document', exact: true }).click()
    await expect(page.locator(detailedSelector)).toHaveCount(0)
    readiness.push(await measureClick('changes', 'highlighted'))
  }
  const modeSwitch = []
  for (let n = 0; n < samples; n++) {
    const side = n % 2 === 0
    modeSwitch.push({
      to: side ? 'side' : 'inline',
      ...(await measureClick(side ? 'side by side' : 'inline', side ? 'side' : 'inline')),
    })
  }
  const heapAfterUi = await heap(false)
  const heapAfterCollect = await heap(true)
  await cdp.detach()

  const summary = (values: number[]) => {
    const sorted = [...values].sort((a, b) => a - b)
    const middle = sorted.length >> 1
    return {
      samples: values.length,
      durations_ms: values,
      median_ms: sorted.length % 2 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2,
      max_ms: sorted.at(-1),
    }
  }
  const base = guardDocument('base')
  const target = guardDocument('target')
  const comparison = compareDocuments({ content: base }, { content: target })
  const os = await import('node:os')
  const measurements = {
    fixture: {
      characters: { base: base.length, target: target.length, total: base.length + target.length },
      blocks: { base: reviewBlocks({ content: base }).length, target: reviewBlocks({ content: target }).length },
      limits: {
        characters: 120_000,
        blocks: 400,
        matrix_cells: 2_500_000,
        section_cells: 500_000,
        document_cells: 2_000_000,
      },
      token_cells: Object.fromEntries(
        comparison.rows
          .filter((row) => row.changed)
          .map((row) => [
            row.title,
            reviewRowParagraphs(row).reduce(
              (sum, pair) => sum + paragraphCells(pair.before ?? '', pair.after ?? ''),
              0,
            ),
          ]),
      ),
      detailed_pair_cells: detailedCells,
      oversized_pair_cells: oversizedCells,
      alignment_cells_per_single_paragraph_section: 4,
      document_limited: comparison.limited,
    },
    model_ms: {
      compare_documents: summary(model.compareDocuments),
      render_changed_paragraphs: summary(model.renderParagraphs),
      detailed_pair_formatted_changes: summary(model.detailedPair),
    },
    ui_ms: {
      initial_comparison_readiness_dom: summary(readiness.map((sample) => sample.dom)),
      initial_comparison_readiness_next_frame: summary(readiness.map((sample) => sample.frame)),
      mode_switch_dom: summary(modeSwitch.map((sample) => sample.dom)),
      mode_switch_next_frame: summary(modeSwitch.map((sample) => sample.frame)),
      mode_switch_directions: modeSwitch.map((sample) => sample.to),
    },
    heap_bytes: {
      note: 'Runtime.getHeapUsage observations of the JS heap at the listed points; deltas are not peak allocation.',
      before_after_gc: heapBefore,
      after_model_samples: heapAfterModel,
      after_ui_samples: heapAfterUi,
      after_ui_samples_after_gc: heapAfterCollect,
    },
    matrix_estimate_bytes: {
      note: 'Uint32Array storage only, before row objects and token arrays.',
      detailed_pair: detailedCells * 4,
      matrix_guard: 2_500_000 * 4,
    },
    environment: {
      browser: browser.browserType().name(),
      browser_version: browser.version(),
      viewport: page.viewportSize(),
      host: {
        platform: os.platform(),
        release: os.release(),
        arch: os.arch(),
        cpu: os.cpus()[0]?.model,
        cpus: os.cpus().length,
        memory_bytes: os.totalmem(),
        node: process.version,
      },
      playwright_workers: process.env.PLAYWRIGHT_WORKERS ?? '2',
    },
  }
  expect(measurements.fixture.token_cells).toEqual({
    'Detailed revision': detailedCells,
    'Oversized block': 0,
    Sibling: 9 * 9,
  })
  expect(readiness).toHaveLength(samples)
  expect(modeSwitch).toHaveLength(samples)
  const output = testInfo.outputPath('guard-comparison-measurements.json')
  const { writeFile } = await import('node:fs/promises')
  await writeFile(output, `${JSON.stringify(measurements, null, 2)}\n`)
  await testInfo.attach('guard-comparison-measurements', { path: output, contentType: 'application/json' })
})
