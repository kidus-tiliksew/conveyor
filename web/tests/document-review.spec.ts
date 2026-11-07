import { expect, test } from '@playwright/test'
import {
  alignedParagraphs,
  compareDocuments,
  formattedParagraph,
  formattedParagraphChanges,
  headingBlocks,
  listBlockChanges,
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
  expect(listBlockChanges('- list item', '- list item')).toMatchObject([
    { after: { marker: '-', depth: 0, text: 'list item' }, changes: [{ text: 'list item', kind: 'same' }] },
  ])
  expect(formattedParagraph('| Field | Value |\n| --- | --- |\n| Lease | 10 |')).toBeUndefined()
  expect(formattedParagraph('```go\nrenew()\n```')).toBeUndefined()
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
  // A list block costs its item alignment plus each changed item's token matrix.
  expect(paragraphCells('- list item', '- other item')).toBe(2 * 2 + 4 * 4)
  expect(paragraphCells('- list item', '- list item')).toBe(0)
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

const listItemBase =
  '31. **Cancellation.** It is valid from any non-terminal state. `POST .../close` requires a non-blank reason of at most 64 bytes and refuses a terminal task with 409.'
const listItemTarget =
  '31. **Cancellation.** It is valid from any non-terminal state. `POST .../close` requires a non-blank `reason` of at most 255 characters, or with no `reason` a `reason_code` of at most 64 bytes, and refuses a terminal task with 409.'
const listSiblings =
  '32. **Closure.** A closed task keeps its history.\n33. **Retention.** Events stay append-only and readable.'
const terminationList = (item: string) =>
  `# Task lifecycle\n\n## Termination\n\n${item}\n${listSiblings}\n\n## Stable\n\nSame.`

// Fences opened on a marker line: base, target, the fence item's marker, the sibling's marker.
const markerFences = [
  ['- ```text\n  - old code\n  ```\n- sibling old', '- ```text\n  - new code\n  ```\n- sibling new', '-', '-'],
  ['* ~~~sh\n  1. old code\n  ~~~\n* sibling old', '* ~~~sh\n  1. new code\n  ~~~\n* sibling new', '*', '*'],
  [
    '1. ```text\n   2. old code\n   ```\n2. sibling old',
    '1. ```text\n   2. new code\n   ```\n2. sibling new',
    '1.',
    '2.',
  ],
  [
    '10) ~~~sh\n    - old code\n    ~~~\n11) sibling old',
    '10) ~~~sh\n    - new code\n    ~~~\n11) sibling new',
    '10)',
    '11)',
  ],
] as const

const added = (changes?: Array<{ text: string; kind: string }>) =>
  (changes ?? []).filter((change) => change.kind === 'added').map((change) => change.text)
const removed = (changes?: Array<{ text: string; kind: string }>) =>
  (changes ?? []).filter((change) => change.kind === 'removed').map((change) => change.text)
const annotated = (changes?: Array<{ kind: string }>) => (changes ?? []).filter((change) => change.kind !== 'same')

test('review model diffs list blocks per item and keeps source markers', () => {
  const items = listBlockChanges(`${listItemBase}\n${listSiblings}`, `${listItemTarget}\n${listSiblings}`)
  expect(items?.map((item) => item.after?.marker)).toEqual(['31.', '32.', '33.'])
  expect(items?.map((item) => item.before?.marker)).toEqual(['31.', '32.', '33.'])
  expect(added(items?.[0].changes).join('')).toContain('255')
  expect(removed(items?.[0].changes).join('')).toContain('reason')
  expect(items?.[0].changes).toContainEqual({
    text: 'Cancellation.',
    kind: 'same',
    style: 'strong',
  })
  expect(annotated(items?.[1].changes)).toEqual([])
  expect(annotated(items?.[2].changes)).toEqual([])

  // Bulleted items, a continuation line, and an ordered `)` marker.
  const continued = listBlockChanges(
    '- first line\n  renew every 30 seconds\n- second',
    '- first line\n  renew every 10 seconds\n- second',
  )
  expect(continued).toHaveLength(2)
  expect(continued?.[0].after?.text).toBe('first line\nrenew every 10 seconds')
  expect(added(continued?.[0].changes)).toEqual(['10'])
  expect(removed(continued?.[0].changes)).toEqual(['30'])
  expect(annotated(continued?.[1].changes)).toEqual([])
  expect(listBlockChanges('7) seven', '7) eight')?.[0].after?.marker).toBe('7)')

  // A nested item aligns and highlights alone, at its own depth.
  const nested = listBlockChanges(
    '15. Bounded work.\n    - A pair renders whole.\n    - A section keeps its entry.\n16. Line diff.',
    '15. Bounded work.\n    - A pair renders whole.\n    - A section keeps its navigator entry.\n16. Line diff.',
  )
  expect(nested?.map((item) => [item.after?.marker, item.after?.depth])).toEqual([
    ['15.', 0],
    ['-', 1],
    ['-', 1],
    ['16.', 0],
  ])
  expect(nested?.map((item) => annotated(item.changes).length)).toEqual([0, 0, 1, 0])

  // Text before the first marker stays its own row with no marker.
  const leadIn = listBlockChanges('Rules:\n- one', 'Rules now:\n- one')
  expect(leadIn?.map((item) => item.after?.marker)).toEqual(['', '-'])
  expect(added(leadIn?.[0].changes).join('')).toContain('now')
  expect(annotated(leadIn?.[1].changes)).toEqual([])

  // Prose, thematic breaks, setext underlines, and indented code are not lists.
  expect(listBlockChanges('Renew every 30 seconds.', 'Renew every 10 seconds.')).toBeUndefined()
  expect(listBlockChanges('- - -', '- - -')).toBeUndefined()
  expect(listBlockChanges('- item\n---', '- other\n---')).toBeUndefined()
  expect(listBlockChanges('    - code', '    - other')).toBeUndefined()
  expect(listBlockChanges('    code\n    - a', '    code\n    - b')).toBeUndefined()

  // Paragraph text absorbs a deep line, and an ordered marker other than 1.
  expect(listBlockChanges('Released in\n2024. Then more', 'Released in\n2025. Then more')).toBeUndefined()
  expect(listBlockChanges('Some text\n    - not list', 'Some text\n    - not it')).toBeUndefined()
  expect(listBlockChanges('Steps:\n1. one', 'Steps:\n1. two')?.map((item) => item.after?.marker)).toEqual(['', '1.'])

  // CRLF line endings keep each item on its own row.
  const crlf = listBlockChanges('- a one\r\n- b two\r\n- c three', '- a one\r\n- b four\r\n- c three')
  expect(crlf?.map((item) => item.after?.text)).toEqual(['a one', 'b four', 'c three'])
})

test('review model aligns inserted, removed, and renumbered list items', () => {
  const inserted = listBlockChanges('- alpha one\n- gamma three', '- alpha one\n- beta two\n- gamma three')
  expect(inserted?.map((item) => [item.before?.text, item.after?.text])).toEqual([
    ['alpha one', 'alpha one'],
    [undefined, 'beta two'],
    ['gamma three', 'gamma three'],
  ])
  expect(inserted?.[1].changes).toEqual([{ text: 'beta two', kind: 'added', style: 'text' }])
  expect(annotated(inserted?.[0].changes)).toEqual([])
  expect(annotated(inserted?.[2].changes)).toEqual([])

  const dropped = listBlockChanges('- alpha one\n- beta two\n- gamma three', '- alpha one\n- gamma three')
  expect(dropped?.map((item) => [item.before?.text, item.after?.text])).toEqual([
    ['alpha one', 'alpha one'],
    ['beta two', undefined],
    ['gamma three', 'gamma three'],
  ])
  expect(dropped?.[1].changes).toEqual([{ text: 'beta two', kind: 'removed', style: 'text' }])

  // A renumbering pairs each item with its own text and keeps both numbers.
  const renumbered = listBlockChanges(
    '1. alpha one\n2. beta two\n3. gamma three',
    '1. delta four\n2. alpha one\n3. beta two\n4. gamma three',
  )
  expect(renumbered?.map((item) => [item.before?.marker, item.after?.marker])).toEqual([
    [undefined, '1.'],
    ['1.', '2.'],
    ['2.', '3.'],
    ['3.', '4.'],
  ])
  expect(renumbered?.slice(1).every((item) => annotated(item.changes).length === 0)).toBe(true)

  // An insertion beside an edit keeps the edit paired with its own item.
  const editBesideInsert = listBlockChanges(
    '- renew the claim every 30 seconds\n- stable item',
    '- brand new unrelated rule\n- renew the claim every 10 seconds\n- stable item',
  )
  expect(editBesideInsert?.map((item) => [item.before?.text, item.after?.text])).toEqual([
    [undefined, 'brand new unrelated rule'],
    ['renew the claim every 30 seconds', 'renew the claim every 10 seconds'],
    ['stable item', 'stable item'],
  ])
  expect(added(editBesideInsert?.[1].changes)).toEqual(['10'])

  // Loose items are separate paragraphs; a renumbering joins them into one
  // aligned list pair, and prose in the same hunk stays positional.
  expect(alignedParagraphs('1. alpha one\n\n2. beta two', '1. new item\n\n2. alpha one\n\n3. beta two')).toEqual([
    { before: '1. alpha one\n\n2. beta two', after: '1. new item\n\n2. alpha one\n\n3. beta two' },
  ])
  const loose = listBlockChanges('1. alpha one\n\n2. beta two', '1. new item\n\n2. alpha one\n\n3. beta two')
  expect(loose?.map((item) => [item.before?.text, item.after?.text])).toEqual([
    [undefined, 'new item'],
    ['alpha one', 'alpha one'],
    ['beta two', 'beta two'],
  ])
  // An edited continuation paragraph stays with its item through a renumbering.
  const continuedLoose = alignedParagraphs(
    '1. alpha one\n\n   more alpha detail\n\n2. beta two',
    '1. new item\n\n2. alpha one\n\n   more alpha details\n\n3. beta two',
  )
  expect(continuedLoose).toHaveLength(1)
  expect(
    listBlockChanges(continuedLoose[0].before ?? '', continuedLoose[0].after ?? '')?.map((item) => [
      item.before?.text,
      item.after?.text,
    ]),
  ).toEqual([
    [undefined, 'new item'],
    ['alpha one\nmore alpha detail', 'alpha one\nmore alpha details'],
    ['beta two', 'beta two'],
  ])
  expect(alignedParagraphs('First\n\nLast', 'New\n\n- item\n\nLast')).toEqual([
    { before: 'First', after: 'New' },
    { before: undefined, after: '- item' },
    { before: 'Last', after: 'Last' },
  ])
})

test('review model falls back one unsafe list item at a time', () => {
  const mixed = listBlockChanges(
    '- safe 30 seconds\n- table item\n  | Name | Value |\n  | --- | --- |\n  | Lease | 30 |\n- <b>markup</b> before\n- last 1',
    '- safe 10 seconds\n- table item\n  | Name | Value |\n  | --- | --- |\n  | Lease | 10 |\n- <b>markup</b> after\n- last 2',
  )
  expect(mixed).toHaveLength(4)
  expect(added(mixed?.[0].changes)).toEqual(['10'])
  expect(mixed?.[1].changes).toBeUndefined()
  expect(mixed?.[1].after?.text).toContain('| Lease | 10 |')
  expect(mixed?.[2].changes).toBeUndefined()
  expect(added(mixed?.[3].changes)).toEqual(['2'])

  // A list marker inside a fence doesn't start an item, and the fence falls back.
  const fenced = listBlockChanges(
    '- run this\n  ```sh\n  - not an item\n\n  1. nor this\n  ```\n- after 1',
    '- run this\n  ```sh\n  - still not an item\n\n  1. nor this\n  ```\n- after 2',
  )
  expect(fenced).toHaveLength(2)
  expect(fenced?.[0].changes).toBeUndefined()
  expect(added(fenced?.[1].changes)).toEqual(['2'])

  // Leaving an item ends an unclosed fence inside it, so later items stay items.
  const unclosed = listBlockChanges('- a\n  ```\n  code\n- b one\n- c', '- a\n  ```\n  other\n- b two\n- c')
  expect(unclosed?.map((item) => item.after?.marker)).toEqual(['-', '-', '-'])
  expect(unclosed?.[0].changes).toBeUndefined()
  expect(added(unclosed?.[1].changes)).toEqual(['two'])

  // A dash line in an item's fence is code, not a setext underline.
  const dashes = listBlockChanges('- a\n  ```\n  ---\n  ```\n- b one', '- a\n  ```\n  ---\n  x\n  ```\n- b two')
  expect(dashes).toHaveLength(2)
  expect(dashes?.[0].changes).toBeUndefined()
  expect(added(dashes?.[1].changes)).toEqual(['two'])

  // A fence opened on the marker line stays whole in its own item, its list-looking
  // code starts no item, and the changed sibling stays paired and highlighted.
  for (const [before, after, firstMarker, siblingMarker] of markerFences) {
    const items = listBlockChanges(before, after)
    expect(items?.map((item) => item.after?.marker)).toEqual([firstMarker, siblingMarker])
    expect(items?.map((item) => item.before?.marker)).toEqual([firstMarker, siblingMarker])
    expect(items?.[0].after?.text.split('\n').at(0)).toMatch(/^(?:```text|~~~sh)$/)
    expect(items?.[0].after?.text.split('\n').at(-1)).toMatch(/^(?:```|~~~)$/)
    expect(items?.[0].after?.text).toContain('new code')
    expect(items?.[0].before?.text).toContain('old code')
    expect(items?.[0].changes).toBeUndefined()
    expect(items?.[1].after?.text).toBe('sibling new')
    expect(items?.[1].before?.text).toBe('sibling old')
    expect(added(items?.[1].changes)).toEqual(['new'])
    expect(removed(items?.[1].changes)).toEqual(['old'])
  }
  // A blank line in such a fence keeps the fence in one paragraph pair, and the
  // paragraphs after it stay separate.
  const blankInFence = alignedParagraphs(
    '- ```text\n  - old code\n\n  - more\n  ```\n- sibling old\n\nAfter text.\n\nLast text.',
    '- ```text\n  - new code\n\n  - more\n  ```\n- sibling new\n\nAfter text.\n\nLast text.',
  )
  expect(blankInFence).toHaveLength(3)
  const blankItems = listBlockChanges(blankInFence[0].before ?? '', blankInFence[0].after ?? '')
  expect(blankItems?.map((item) => item.after?.text)).toEqual(['```text\n- new code\n\n- more\n```', 'sibling new'])
  expect(added(blankItems?.[1].changes)).toEqual(['new'])
  // Code in such a fence starts no section, and the section after it survives.
  expect(
    headingBlocks('## A\n\n- ```md\n  ## not a heading\n  ```\n\n## B\n\nBody').map((block) => block.title),
  ).toEqual(['A', 'B'])
  // Without a closing line, leaving the item ends the fence and later items stay items.
  const unclosedOnMarker = listBlockChanges('- ```\n  code\n- b one\n- c', '- ```\n  other\n- b two\n- c')
  expect(unclosedOnMarker?.map((item) => item.after?.marker)).toEqual(['-', '-', '-'])
  expect(added(unclosedOnMarker?.[1].changes)).toEqual(['two'])
  // Markers nested on one line and a tab after the marker hold the fence the same way.
  expect(
    listBlockChanges('- - ```\n    - code\n    ```\n- b one', '- - ```\n    - more\n    ```\n- b two')?.map(
      (item) => item.after?.text,
    ),
  ).toEqual(['- ```\n  - more\n  ```', 'b two'])
  expect(
    listBlockChanges('-\t```\n    - code\n    ```\n- b one', '-\t```\n    - more\n    ```\n- b two')?.map(
      (item) => item.after?.text,
    ),
  ).toEqual(['```\n- more\n```', 'b two'])
  // A line with a closing backtick run is inline code, not a fence.
  const inlineTicks = listBlockChanges('- ```a``` old\n- sibling old', '- ```a``` new\n- sibling new')
  expect(inlineTicks?.map((item) => item.after?.text)).toEqual(['```a``` new', 'sibling new'])

  const javascriptLink = listBlockChanges('- [one](javascript:alert(1))\n- two', '- [one](javascript:alert(2))\n- two')
  expect(javascriptLink?.[0].changes).toBeUndefined()
  expect(annotated(javascriptLink?.[1].changes)).toEqual([])
})

test('review model highlights literal characters and keeps real block structure as a fallback', () => {
  for (const [before, after, change] of [
    ['Route a -> b today.', 'Route a -> c today.', 'c'],
    ['Require a > b today.', 'Require a > c today.', 'c'],
    ['Require x < y today.', 'Require x < z today.', 'z'],
    ['Allow x <y today.', 'Allow x <z today.', 'z'],
    ['Use `a | b` today.', 'Use `a | c` today.', 'c'],
    ['Read a | b today.', 'Read a | c today.', 'c'],
    ['About ~5 minutes.', 'About ~9 minutes.', '9'],
    ['Match C:\\dir\\file today.', 'Match C:\\dir\\other today.', 'other'],
    ['Name `<id>` today.', 'Name `<key>` today.', 'key'],
    ['Use `~~x~~` and `\\*` today.', 'Use `~~y~~` and `\\*` today.', 'y'],
    ['**a > b** today.', '**a > c** today.', 'c'],
  ]) {
    expect(formattedParagraphChanges(before, after), before).toContainEqual(
      expect.objectContaining({ text: change, kind: 'added' }),
    )
    expect(paragraphCells(before, after), before).toBeGreaterThan(0)
  }

  for (const unsafe of [
    '| Field | Value |\n| --- | --- |\n| Lease | 10 |',
    'Field | Value\n--- | ---\nLease | 10',
    '| Field |\n|:---:|\n| Lease |',
    '```go\nrenew()\n```',
    '~~~\nrenew()\n~~~',
    '```mermaid\nflowchart LR\n A-->B\n```',
    '> quoted policy',
    '# Heading',
    'Setext\n---',
    '<script>window.injected = true</script>',
    'Text with <b>bold</b> inside.',
    'Closing </div> tag.',
    'A comment <!-- hidden --> here.',
    'A declaration <!DOCTYPE html> here.',
    'An instruction <?php echo ?> here.',
    '<div',
    'See <https://example.test/a> now.',
    'Mail <1person@example.test> now.',
    '<img src=x onerror=alert(1)>',
    'Struck ~~out~~ words.',
    'Escaped \\* star.',
    'Escaped \\`x` tick.',
    'Hard break\\\nnext line.',
    '![alt](https://example.test/a.png)',
  ]) {
    const changed = `${unsafe}\n\nextra`
    expect(formattedParagraph(unsafe), unsafe).toBeUndefined()
    expect(formattedParagraphChanges(unsafe, `${unsafe} changed`), unsafe).toBeUndefined()
    expect(paragraphCells(unsafe, `${unsafe} changed`), unsafe).toBe(0)
    expect(listBlockChanges(unsafe, changed), unsafe).toBeUndefined()
  }
})

test('review model counts list work in its section and document budgets', () => {
  // Item alignment, the similar-item pairing of an uneven gap, and token diffs all count.
  expect(paragraphCells('- a b c', '- a b d')).toBe(2 * 2 + 6 * 6)
  const uneven = paragraphCells('- alpha beta\n- gamma delta', '- alpha beta two\n- new words\n- gamma delta two')
  // 3 x 4 alignment, 3 x 4 score, 2 x 3 weight, 4 unique words x 3 scanned, then
  // (3 + 1) x (5 + 1) token cells for each of the two edited pairs and 1 x 4 for the insertion.
  expect(uneven).toBe(12 + 12 + 6 + 12 + 24 + 24 + 4)

  // An alignment over the per-matrix guard renders whole and allocates nothing.
  const hugeList = (word: string) => Array.from({ length: 1600 }, (_, n) => `- ${word}${n}`).join('\n')
  expect(paragraphCells(hugeList('a'), hugeList('b'))).toBe(0)
  expect(listBlockChanges(hugeList('a'), hugeList('b'))).toBeUndefined()
  // A pairing over the guard renders whole, and its allocated alignment still counts.
  const wideList = (word: string) => Array.from({ length: 1000 }, (_, n) => `- ${word}${n}`).join('\n')
  expect(paragraphCells(wideList('a'), wideList('b'))).toBe(1001 * 1001)
  expect(listBlockChanges(wideList('a'), wideList('b'))).toBeUndefined()
  const wideSections = Array.from({ length: 8 }, (_, n) => n)
  const wideDocument = (word: string) => wideSections.map((n) => `## Wide ${n}\n\n${wideList(word)}`).join('\n\n')
  const wide = compareDocuments({ content: wideDocument('a') }, { content: wideDocument('b') })
  expect(wide.limited).toBe(false)
  expect(wide.rows.every((row) => row.limited)).toBe(true)

  // Each of these edited items costs 470 x 470 token cells, like heavyParagraph,
  // and keeps enough shared words to stay paired with its own text.
  const heavyItem = (word: string, n: number) => `- item${n} ${'same '.repeat(200)}${`${word} `.repeat(34)}`.trimEnd()
  const heavyList = (title: string, word: string, count: number) =>
    `## ${title}\n\n${Array.from({ length: count }, (_, n) => heavyItem(word, n)).join('\n')}`
  const isolated = compareDocuments(
    { content: `${heavyList('Oversized', 'alpha', 3)}\n\n## Sibling\n\n- Renew every 30 seconds.` },
    { content: `${heavyList('Oversized', 'beta', 3)}\n\n## Sibling\n\n- Renew every 10 seconds.` },
  )
  expect(isolated.limited).toBe(false)
  expect(isolated.rows.map(({ title, limited }) => ({ title, limited }))).toEqual([
    { title: 'Oversized', limited: true },
    { title: 'Sibling', limited: false },
  ])
  const listDocument = (word: string, sections: number) =>
    Array.from({ length: sections }, (_, n) => heavyList(`Heavy ${n}`, word, 2)).join('\n\n')
  const aggregate = compareDocuments({ content: listDocument('alpha', 5) }, { content: listDocument('beta', 5) })
  expect(aggregate.limited).toBe(true)
  expect(aggregate.rows).toEqual([])
  const underCeiling = compareDocuments({ content: listDocument('alpha', 4) }, { content: listDocument('beta', 4) })
  expect(underCeiling.limited).toBe(false)
  expect(underCeiling.rows.every((row) => row.changed && !row.limited)).toBe(true)

  // A rewrite of every word leaves no similar items, so it costs one removal and one insertion per item.
  const rewriteItems = (word: string) =>
    `## Rules\n\n${Array.from({ length: 40 }, (_, n) => `${n + 1}. ${`${word}${n} `.repeat(60).trimEnd()}`).join('\n')}`
  const rewritten = compareDocuments({ content: rewriteItems('old') }, { content: rewriteItems('new') })
  expect(rewritten.limited).toBe(false)
  expect(rewritten.rows).toMatchObject([{ title: 'Rules', changed: true, limited: false }])
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
    // Versions recorded before task-authored proposals were retired keep their
    // origin in history (req-task-authority-boundary AC-1.4).
    ...(version === 3 ? { origin: 'implementation_deliberation', origin_task_id: 'legacy-task' } : {}),
    ...(version === 4 ? { origin: 'planning_session', origin_session_id: 'legacy-session' } : {}),
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
      return route.fulfill({
        json: callerIdentity({ id: 'operator', role: options.reader ? 'viewer' : 'operator' }, route.request()),
      })
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
    const history = page.getByRole('region', { name: 'Version history' })
    await expect(history).toContainText('implementation deliberation · Task legacy-task')
    await expect(history).toContainText('planning session · Session legacy-session')
    await expect(history).not.toContainText(/waiting on|cannot be claimed|withheld/i)
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
    await expect.poll(() => seed.calls.at(-1)?.path).toContain('/versions/3/confirm')
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

  test(`${tier}: every tab and a Markdown table share one column`, async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 })
    const seed = await seedReview(page, tier, {
      content: `# Rich\n\nA paragraph long enough to reach the edge of the reading column. ${'More words follow here. '.repeat(20)}\n\n| Field | Value |\n| --- | --- |\n| Lease | 10 |`,
    })
    await page.goto(seed.url)
    const edges = async (locator: ReturnType<Page['locator']>) => {
      const box = await locator.boundingBox()
      if (!box) throw new Error('expected a laid-out element')
      return { left: Math.round(box.x), right: Math.round(box.x + box.width) }
    }
    const tabRow = page.getByRole('tablist', { name: 'Document views' }).locator('..')
    await expect(page.getByRole('region', { name: 'Version comparison', exact: true })).toBeVisible()
    const changes = await edges(tabRow)
    await page.getByRole('tab', { name: 'document' }).click()
    const table = page.locator('.document-body .markdown-table')
    await expect(table).toBeVisible()
    expect(await edges(tabRow)).toEqual(changes)
    expect(await edges(table)).toEqual(changes)
    expect(await edges(page.locator('.document-body .markdown > p').first())).toEqual(changes)
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

  test(`${tier}: seeded list fixture highlights only the changed item in both comparison modes`, async ({
    page,
  }, testInfo) => {
    // Tall enough that the section screenshots below aren't clipped.
    await page.setViewportSize({ width: 1600, height: 1400 })
    const seed = await seedReview(page, tier, {
      baseContent: terminationList(listItemBase),
      targetContent: terminationList(listItemTarget),
    })
    await page.goto(seed.url)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    const block = comparison.getByRole('region', { name: 'Termination', exact: true })
    const items = block.getByRole('listitem')
    for (const mode of ['Inline', 'Side by side']) {
      await page.getByRole('button', { name: mode, exact: true }).click()
      await expect(items).toHaveCount(3)
      await expect(items.nth(0)).toContainText('31.')
      await expect(items.nth(1)).toContainText('32.')
      await expect(items.nth(2)).toContainText('33.')
      await expect(items.nth(0).locator('ins').filter({ hasText: '255' })).toBeVisible()
      await expect(items.nth(0).locator('ins code').filter({ hasText: 'reason_code' })).toBeVisible()
      await expect(items.nth(0).locator('del').filter({ hasText: 'reason' })).toBeVisible()
      await expect(items.nth(0).locator('strong').filter({ hasText: 'Cancellation.' }).first()).toBeVisible()
      await expect(items.nth(1).locator('ins, del')).toHaveCount(0)
      await expect(items.nth(2).locator('ins, del')).toHaveCount(0)
      await expect(block).not.toContainText('Detailed highlighting unavailable')
      await block.screenshot({
        path: testInfo.outputPath(`${tier}-seeded-list-${mode === 'Inline' ? 'inline' : 'side-by-side'}.png`),
      })
    }
  })

  test(`${tier}: an inserted item and an unsafe item leave their sibling items highlighted`, async ({ page }) => {
    const seed = await seedReview(page, tier, {
      baseContent:
        '# Rules\n\n## Limits\n\n1. Renew every 30 seconds.\n2. Keep the claim.\n3. Use <b>old</b> markup.\n    - Nested 30 detail.\n4. Last rule.',
      targetContent:
        '# Rules\n\n## Limits\n\n1. Renew every 10 seconds.\n2. Inserted policy text here.\n3. Keep the claim.\n4. Use <b>new</b> markup.\n    - Nested 10 detail.\n5. Last rule.',
    })
    await page.goto(seed.url)
    const block = page
      .getByRole('region', { name: 'Version comparison', exact: true })
      .getByRole('region', { name: 'Limits', exact: true })
    const items = block.getByRole('listitem')
    for (const mode of ['Inline', 'Side by side']) {
      await page.getByRole('button', { name: mode, exact: true }).click()
      await expect(items).toHaveCount(6)
      await expect(items.nth(0).locator('ins').filter({ hasText: '10' })).toBeVisible()
      await expect(items.nth(1).locator('ins').filter({ hasText: 'Inserted policy text here.' })).toBeVisible()
      await expect(items.nth(2)).toContainText('Keep the claim.')
      await expect(items.nth(2).getByText('Keep the claim.').locator('xpath=ancestor-or-self::ins')).toHaveCount(0)
      await expect(items.nth(2).locator('del').filter({ hasText: '2.' })).toBeVisible()
      await expect(items.nth(2).locator('ins').filter({ hasText: '3.' })).toBeVisible()
      await expect(items.nth(3)).toContainText('Detailed highlighting unavailable for this block')
      await expect(items.nth(3).locator('b')).toHaveCount(0)
      await expect(items.nth(4).locator('ins').filter({ hasText: '10' })).toBeVisible()
      await expect(items.nth(4)).not.toContainText('Detailed highlighting unavailable')
      await expect(items.nth(5)).toContainText('Last rule.')
      await expect(block.getByText('Detailed highlighting unavailable for this block')).toHaveCount(1)
    }
  })

  for (const [name, marker, sibling] of [
    ['bulleted', '-', '-'],
    ['ordered', '1.', '2.'],
  ] as const) {
    test(`${tier}: a fence opened on a ${name} marker stays in its item beside a highlighted sibling`, async ({
      page,
    }) => {
      const indent = ' '.repeat(marker.length + 1)
      const list = (word: string) =>
        `# Rules\n\n## Fenced\n\n${marker} \`\`\`text\n${indent}- ${word} code\n${indent}2. ${word} step\n${indent}\`\`\`\n${sibling} sibling ${word}`
      const seed = await seedReview(page, tier, { baseContent: list('old'), targetContent: list('new') })
      await page.goto(seed.url)
      const block = page
        .getByRole('region', { name: 'Version comparison', exact: true })
        .getByRole('region', { name: 'Fenced', exact: true })
      const items = block.getByRole('listitem')
      for (const mode of ['Inline', 'Side by side']) {
        await page.getByRole('button', { name: mode, exact: true }).click()
        await expect(items).toHaveCount(2)
        await expect(items.nth(0)).toContainText('Detailed highlighting unavailable for this block')
        await expect(items.nth(0).locator('pre code').first()).toContainText('- old code')
        await expect(items.nth(0).locator('pre code').first()).toContainText('2. old step')
        await expect(items.nth(0).locator('pre code').last()).toContainText('- new code')
        await expect(items.nth(0).locator('pre code').last()).toContainText('2. new step')
        await expect(items.nth(0).locator('ins, del')).toHaveCount(0)
        await expect(items.nth(1).locator('ins').filter({ hasText: 'new' })).toBeVisible()
        await expect(items.nth(1).locator('del').filter({ hasText: 'old' })).toBeVisible()
        await expect(items.nth(1)).toContainText('sibling')
        await expect(items.nth(1)).not.toContainText('Detailed highlighting unavailable')
        await expect(items.nth(1).locator('pre')).toHaveCount(0)
        if (marker === '1.') await expect(items.nth(1)).toContainText('2.')
      }
    })
  }

  test(`${tier}: real block structure keeps its notice and HTML-like input renders safely`, async ({ page }) => {
    const seed = await seedReview(page, tier, {
      baseContent:
        '# Rich\n\n## Table\n\n| Name | Value |\n| --- | --- |\n| Lease | 30 |\n\n## Code\n\n```go\nrenew(30)\n```\n\n## Markup\n\n<img src=x onerror="window.injected = true"> old <script>window.injected = true</script>\n\n## Arrows\n\nRoute a -> b when `x | y` holds.',
      targetContent:
        '# Rich\n\n## Table\n\n| Name | Value |\n| --- | --- |\n| Lease | 10 |\n\n## Code\n\n```go\nrenew(10)\n```\n\n## Markup\n\n<img src=x onerror="window.injected = true"> new <script>window.injected = true</script>\n\n## Arrows\n\nRoute a -> c when `x | y` holds.',
    })
    await page.goto(seed.url)
    const comparison = page.getByRole('region', { name: 'Version comparison', exact: true })
    for (const mode of ['Inline', 'Side by side']) {
      await page.getByRole('button', { name: mode, exact: true }).click()
      for (const name of ['Table', 'Code', 'Markup']) {
        const section = comparison.getByRole('region', { name, exact: true })
        await expect(section).toContainText('Detailed highlighting unavailable for this block')
        await expect(section.locator('ins, del')).toHaveCount(0)
      }
      await expect(comparison.getByRole('region', { name: 'Table', exact: true }).getByRole('table')).toHaveCount(2)
      await expect(comparison.locator('script, img')).toHaveCount(0)
      expect(await page.evaluate(() => 'injected' in window)).toBe(false)
      const arrows = comparison.getByRole('region', { name: 'Arrows', exact: true })
      await expect(arrows).not.toContainText('Detailed highlighting unavailable')
      await expect(arrows.locator('ins').filter({ hasText: 'c' })).toBeVisible()
      await expect(arrows.locator('code').filter({ hasText: 'x | y' }).first()).toBeVisible()
      await expect(arrows).toContainText('a ->')
    }
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
