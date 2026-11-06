import type { RequirementVersion } from '../../lib/types'

export type ReviewBlock = { id: string; title: string; content: string }
export type WordChange = { text: string; kind: 'same' | 'added' | 'removed' }
export type InlineStyle = 'text' | 'code' | 'emphasis' | 'strong' | 'link'
export type FormattedChange = WordChange & { style: InlineStyle; href?: string }
// One list item. `marker` is the source marker (`31.`, `-`), and is empty for
// text that precedes the first marker in its block. Nested items stay flat and
// carry their depth, so each one aligns and highlights on its own.
export type ListItem = { marker: string; depth: number; text: string }
// One aligned row of a list block. `changes` is undefined when the row can't
// carry safe inline marks; that row alone renders whole
// (component-web-document-surfaces "Block comparison" rule 15).
export type ListItemChange = { before?: ListItem; after?: ListItem; changes?: FormattedChange[] }
export type ReviewRow = {
  id: string
  title: string
  before?: ReviewBlock
  after?: ReviewBlock
  moved: boolean
  changed: boolean
  // Set when the section's detailed comparison exceeds the section budget; the
  // renderer shows its complete before and after content instead.
  limited: boolean
}
export type ReviewSource = { content: string; statements?: RequirementVersion['statements'] }

// component-web-document-surfaces "Block comparison and bounds": bound work before
// allocating matrices or parsing Markdown.
const maxCharacters = 120_000
const maxBlocks = 400
// Largest single matrix: one paragraph alignment, one list item alignment, or
// one paragraph or list item token diff.
// A 2.5M-cell Uint32 matrix holds about 10 MB before row and token overhead.
// Paragraph matrices above maxSectionCells still make their section fall back
// alone; pairs above this guard render complete content in their own block.
const maxCells = 2_500_000
// Work is counted in the matrix cells the review renderer allocates. The shared
// LCS loop measured about 7-11 ns per cell on a desktop core (Node 22); assume
// a phone core is about five times slower. A section at the section limit
// costs about 5 ms on desktop and falls back alone, keeping its navigator
// entry. The document ceiling costs about 20 ms on desktop and 100 ms on a
// phone per render, and the renderer repeats paragraph diffs when the mode
// changes. Measured full rewrites of a 25-section design cost about 0.6M
// cells at 15k characters and 1.7M at 42k, so they keep detailed comparison;
// an 87k-character rewrite exceeds the ceiling and takes the document-wide
// fallback. With list blocks counted, the 29 current System Designs stay under
// the ceiling: the largest (40k characters, 170 list lines) costs 0.8M cells
// when every word changes and 1.5M when every third word changes.
const maxSectionCells = 500_000
const maxDocumentCells = 2_000_000

export function reviewText(source: ReviewSource) {
  if (!source.statements?.length) return source.content
  return [
    source.content.replace(/\n?```conveyor:requirements[\s\S]*?```\n?/g, '\n').trim(),
    ...source.statements.flatMap((statement) => [
      `${statement.id}: ${statement.statement}`,
      ...(statement.user_story
        ? [
            `As ${statement.user_story.as_a}, I want ${statement.user_story.i_want}, so that ${statement.user_story.so_that}.`,
          ]
        : []),
      ...(statement.acceptance_criteria ?? []).map((ac) => `${ac.id}: ${ac.statement}`),
    ]),
  ]
    .filter(Boolean)
    .join('\n\n')
}

// Columns a line prefix spans from `start`, with tab stops every four columns.
function columnWidth(text: string, start = 0) {
  let column = start
  for (const character of text) column += character === '\t' ? 4 - (column % 4) : 1
  return column - start
}

function lineIndent(line: string) {
  return columnWidth(line.match(/^[ \t]*/)?.[0] ?? '')
}

// A fence's content column is its list item's, or 0 outside a list item.
type OpenFence = { marker: string; contentColumn: number }

const fenceMarker = /^(`{3,}(?=[^`]*$)|~{3,})/
const listMarkerPrefix = /^[ \t]*(?:(?:[-+*]|\d{1,9}[.)])[ \t]+)+/

// A fence opened on a block's own line, at most three columns in, or on a list
// marker line (`- ```text`, `1. ~~~`). A backtick info string holds no backtick,
// so ```` ```x``` ```` is inline code.
function openedFence(line: string): OpenFence | undefined {
  const prefix = line.match(listMarkerPrefix)?.[0]
  if (prefix) {
    const marker = line.slice(prefix.length).match(fenceMarker)?.[1]
    return marker ? { marker, contentColumn: columnWidth(prefix) } : undefined
  }
  if (lineIndent(line) > 3) return undefined
  const marker = line.trimStart().match(fenceMarker)?.[1]
  return marker ? { marker, contentColumn: 0 } : undefined
}

// A line left of its list item's content ends the item and any fence inside it.
function leftFenceItem(line: string, fence: OpenFence) {
  return fence.contentColumn > 0 && !!line.trim() && lineIndent(line) < fence.contentColumn
}

function closesFence(line: string, fence: OpenFence) {
  const closer = line.match(/^[ \t]*(`{3,}|~{3,})[ \t]*$/)?.[1]
  return (
    closer?.[0] === fence.marker[0] &&
    closer.length >= fence.marker.length &&
    lineIndent(line) - fence.contentColumn <= 3
  )
}

// The fence open after `line`, for block and paragraph splitting
// (component-web-document-surfaces "Block comparison" rule 13).
function fenceAfterLine(line: string, fence: OpenFence | undefined) {
  if (!fence || leftFenceItem(line, fence)) return openedFence(line)
  return closesFence(line, fence) ? undefined : fence
}

export function headingBlocks(content: string): ReviewBlock[] {
  const blocks: ReviewBlock[] = []
  const occurrences = new Map<string, number>()
  let title = 'Overview'
  let lines: string[] = []
  let fence: OpenFence | undefined
  const flush = () => {
    const text = lines.join('\n').trim()
    if (!text) return
    const occurrence = (occurrences.get(title) ?? 0) + 1
    occurrences.set(title, occurrence)
    blocks.push({
      id: `heading:${title}:${occurrence}`,
      title: occurrence > 1 ? `${title} (${occurrence})` : title,
      content: text,
    })
  }
  const sourceLines = content.split('\n')
  for (let index = 0; index < sourceLines.length; index++) {
    const line = sourceLines[index]
    fence = fenceAfterLine(line, fence)
    const setext = !fence && line.trim() && /^\s{0,3}(?:=+|-+)\s*$/.test(sourceLines[index + 1] ?? '')
    const heading = !fence && line.match(/^\s{0,3}#{1,6}\s+(.+?)\s*#*\s*$/)
    if (heading || setext) {
      flush()
      title = heading ? heading[1] : line.trim()
      lines = []
    }
    lines.push(line)
    if (setext) lines.push(sourceLines[++index])
  }
  flush()
  return blocks
}

export function reviewBlocks(source: ReviewSource): ReviewBlock[] {
  if (!source.statements?.length) return headingBlocks(source.content)
  const prose = source.content.replace(/\n?```conveyor:requirements[\s\S]*?```\n?/g, '\n').trim()
  return [
    ...headingBlocks(prose),
    ...source.statements.flatMap((statement) => [
      {
        id: statement.id,
        title: statement.id,
        content: [
          statement.statement,
          ...(statement.user_story
            ? [
                `As ${statement.user_story.as_a}, I want ${statement.user_story.i_want}, so that ${statement.user_story.so_that}.`,
              ]
            : []),
        ].join('\n\n'),
      },
      ...(statement.acceptance_criteria ?? []).map((ac) => ({ id: ac.id, title: ac.id, content: ac.statement })),
    ]),
  ]
}

function tokens(text: string) {
  return text.match(/\s+|[\p{L}\p{N}_]+|[^\s\p{L}\p{N}_]/gu) ?? []
}

export function wordChanges(before: string, after: string): WordChange[] | undefined {
  const left = tokens(before)
  const right = tokens(after)
  if ((left.length + 1) * (right.length + 1) > maxCells) return undefined
  const matrix = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1))
  for (let i = left.length - 1; i >= 0; i--) {
    for (let j = right.length - 1; j >= 0; j--)
      matrix[i][j] = left[i] === right[j] ? matrix[i + 1][j + 1] + 1 : Math.max(matrix[i + 1][j], matrix[i][j + 1])
  }
  const result: WordChange[] = []
  const append = (text: string, kind: WordChange['kind']) => {
    const last = result.at(-1)
    if (last?.kind === kind) last.text += text
    else result.push({ text, kind })
  }
  let i = 0
  let j = 0
  while (i < left.length || j < right.length) {
    if (i < left.length && j < right.length && left[i] === right[j]) {
      append(left[i++], 'same')
      j++
    } else if (i < left.length && (j === right.length || matrix[i + 1][j] >= matrix[i][j + 1]))
      append(left[i++], 'removed')
    else append(right[j++], 'added')
  }
  return result
}

type InlineRun = { text: string; style: InlineStyle; href?: string }
type InlineToken = InlineRun & { key: string }

function safeLinkDestination(destination: string) {
  if (
    !destination ||
    [...destination].some((character) => character.charCodeAt(0) <= 0x20 || character.charCodeAt(0) === 0x7f) ||
    destination.startsWith('//')
  )
    return false
  const scheme = destination.match(/^([A-Za-z][A-Za-z\d+.-]*):/)?.[1].toLowerCase()
  return !scheme || scheme === 'http' || scheme === 'https' || scheme === 'mailto'
}

function plainInlineContent(text: string) {
  return text.length > 0 && !/[`*_[\]\\]/.test(text)
}

// A GFM table's delimiter row. A bare dash line without a pipe is a setext
// underline or thematic break, which other guards handle.
function hasTableDelimiterRow(text: string) {
  return text.split('\n').some((line) => {
    const row = line.trim()
    return row.includes('|') && /^\|?(?:\s*:?-+:?\s*\|)*\s*:?-+:?\s*\|?$/.test(row)
  })
}

const setextUnderline = /(^|\n)\s{0,3}(?:=+|-+)\s*(?:\n|$)/
const setextLine = /^ {0,3}(?:=+|-+)[ \t]*$/
// Inline Markdown the renderer would misrepresent as literal text: raw HTML
// tags, comments and declarations, autolinks, HTML blocks at a line start,
// `~~` strikethrough, backslash escapes and hard breaks, and images. Any other
// `<`, `>`, `~`, `|`, or `\` renders as literal text, escaped by React. Tested
// outside inline code spans, whose content is already literal.
const unsafeInline =
  /<\/?[A-Za-z][^<>]*>|<[!?]|<[^\s<>@]+@[^\s<>]+>|(?:^|\n)\s{0,3}<\/?[A-Za-z]|~~|\\(?:[!-/:-@[-`{-~]|\n)|!\[/

// Parse only the bounded, ordinary inline subset owned by the document review
// renderer. Block Markdown and ambiguous/malformed inline syntax deliberately
// return undefined so callers can render both complete sources with MarkdownProse.
export function formattedParagraph(text: string): InlineRun[] | undefined {
  if (
    /(^|\n)\s{0,3}(?:#{1,6}\s|>|[-+*]\s|\d+[.)]\s|`{3,}|~{3,})/.test(text) ||
    setextUnderline.test(text) ||
    hasTableDelimiterRow(text) ||
    unsafeInline.test(text.replace(/(?<![`\\])`[^`\n]+`(?!`)/g, ''))
  )
    return undefined
  const runs: InlineRun[] = []
  const append = (run: InlineRun) => {
    const last = runs.at(-1)
    if (last?.style === run.style && last.href === run.href) last.text += run.text
    else runs.push(run)
  }
  let cursor = 0
  while (cursor < text.length) {
    if (text[cursor] === '`') {
      if (text.startsWith('``', cursor)) return undefined
      const end = text.indexOf('`', cursor + 1)
      const content = end < 0 ? '' : text.slice(cursor + 1, end)
      if (!content || content.includes('\n') || /^\s|\s$/.test(content)) return undefined
      append({ text: content, style: 'code' })
      cursor = end + 1
      continue
    }
    if (text.startsWith('**', cursor) || text.startsWith('__', cursor)) {
      const marker = text.slice(cursor, cursor + 2)
      const end = text.indexOf(marker, cursor + 2)
      const content = end < 0 ? '' : text.slice(cursor + 2, end)
      if (!plainInlineContent(content) || /^\s|\s$/.test(content)) return undefined
      append({ text: content, style: 'strong' })
      cursor = end + 2
      continue
    }
    if (text[cursor] === '*' || text[cursor] === '_') {
      const marker = text[cursor]
      const previous = text[cursor - 1]
      const next = text[cursor + 1]
      if (marker === '_' && /[\p{L}\p{N}]/u.test(previous ?? '') && /[\p{L}\p{N}]/u.test(next ?? '')) {
        append({ text: marker, style: 'text' })
        cursor++
        continue
      }
      const end = text.indexOf(marker, cursor + 1)
      const content = end < 0 ? '' : text.slice(cursor + 1, end)
      if (!plainInlineContent(content) || /^\s|\s$/.test(content)) return undefined
      append({ text: content, style: 'emphasis' })
      cursor = end + 1
      continue
    }
    if (text[cursor] === '[') {
      const labelEnd = text.indexOf('](', cursor + 1)
      const destinationEnd = labelEnd < 0 ? -1 : text.indexOf(')', labelEnd + 2)
      if (labelEnd < 0 || destinationEnd < 0) return undefined
      const label = text.slice(cursor + 1, labelEnd)
      const href = text.slice(labelEnd + 2, destinationEnd)
      if (
        !plainInlineContent(label) ||
        !safeLinkDestination(href) ||
        href.includes('(') ||
        text[destinationEnd + 1] === ')'
      )
        return undefined
      append({ text: label, style: 'link', href })
      cursor = destinationEnd + 1
      continue
    }
    if (text[cursor] === ']') return undefined
    let end = cursor + 1
    while (end < text.length && !'`*_[\\<>'.includes(text[end]) && !text.startsWith('![', end)) end++
    append({ text: text.slice(cursor, end), style: 'text' })
    cursor = end
  }
  return runs
}

function inlineTokens(runs: InlineRun[]): InlineToken[] {
  return runs.flatMap((run) =>
    tokens(run.text).map((text) => ({
      ...run,
      text,
      key: `${run.style}\u0000${run.href ?? ''}\u0000${text}`,
    })),
  )
}

function formattedTokens(before: string, after: string) {
  const beforeRuns = formattedParagraph(before)
  const afterRuns = formattedParagraph(after)
  if (!beforeRuns || !afterRuns) return undefined
  return [inlineTokens(beforeRuns), inlineTokens(afterRuns)] as const
}

// Cells formattedParagraphChanges allocates for one pair. Identical text needs
// no matrix; unsafe Markdown and pairs above maxCells allocate none and render
// complete content instead.
function tokenCells(before: string, after: string) {
  if (before === after) return 0
  const pair = formattedTokens(before, after)
  if (!pair) return 0
  const cells = (pair[0].length + 1) * (pair[1].length + 1)
  return cells > maxCells ? 0 : cells
}

// Cells the renderer allocates for one aligned pair: a list block's item
// alignment and item token diffs, or a paragraph's token diff.
export function paragraphCells(before: string, after: string) {
  if (before === after) return 0
  return listComparison(before, after)?.cells ?? tokenCells(before, after)
}

const listMarker = /^([ \t]*)([-+*]|\d{1,9}[.)])(?:([ \t]+)(.*))?$/
const thematicBreak = /^ {0,3}([-*_])[ \t]*(?:\1[ \t]*){2,}$/

// Split a block into its list items, or return undefined when it holds none.
// Marker lines start items, including nested ones. Every other line continues
// the current item, so a fence, table, or quote inside an item stays in that
// item and makes only that item fall back. A fence body never starts an item.
function listItems(block: string): ListItem[] | undefined {
  const items: ListItem[] = []
  const openContentColumns: number[] = []
  let contentColumn = 0
  let fence: OpenFence | undefined
  const attach = (line: string) => {
    const text = line.replace(/^[ \t]+/, (indent) => indent.slice(contentColumn))
    const current = items.at(-1)
    if (!current) items.push({ marker: '', depth: 0, text })
    else current.text = current.text ? `${current.text}\n${text}` : text
  }
  for (const line of block.split(/\r?\n/)) {
    if (fence && leftFenceItem(line, fence)) fence = undefined
    if (fence) {
      attach(line)
      if (closesFence(line, fence)) fence = undefined
      continue
    }
    if (!line.trim()) continue
    const indent = lineIndent(line)
    // Four leading columns before any content is indented code, not a list.
    if (!items.length && indent > 3) return undefined
    const inList = items.some((item) => item.marker)
    const lineFence = line.trimStart().match(fenceMarker)?.[1]
    if (lineFence) {
      fence = { marker: lineFence, contentColumn: inList && indent >= contentColumn ? contentColumn : 0 }
      attach(line)
      continue
    }
    if (setextLine.test(line) || thematicBreak.test(line)) return undefined
    const marker = line.match(listMarker)
    // Paragraph text absorbs a deep line, and any ordered marker other than 1.
    if (!marker || (!inList && items.length && (indent > 3 || /^(?!0*1[.)])\d/.test(marker[2])))) {
      attach(line)
      continue
    }
    while ((openContentColumns.at(-1) ?? 0) > indent) openContentColumns.pop()
    const text = marker[4] ?? ''
    items.push({ marker: marker[2], depth: openContentColumns.length, text })
    const markerEnd = indent + marker[2].length
    contentColumn = markerEnd + (marker[3] ? columnWidth(marker[3], markerEnd) : 1)
    openContentColumns.push(contentColumn)
    // A fence opened on the marker line belongs to this item, including through
    // another marker on the same line (`- - ```text`).
    const opened = text.replace(/^(?:(?:[-+*]|\d{1,9}[.)])[ \t]+)+/, '').match(fenceMarker)?.[1]
    if (opened) fence = { marker: opened, contentColumn }
  }
  return items.some((item) => item.marker) ? items : undefined
}

function startsListItem(paragraph: string) {
  return listMarker.test(paragraph.split('\n', 1)[0]) && !!listItems(paragraph)
}

// A run of loose paragraphs that item alignment can treat as one list: list
// items and the indented paragraphs that continue them.
function isLooseListRun(run: string[]) {
  return (
    startsListItem(run[0]) && run.every((paragraph) => startsListItem(paragraph) || /^(?: {2,}|\t)\S/.test(paragraph))
  )
}

function wordCounts(text: string) {
  const counts = new Map<string, number>()
  for (const token of tokens(text)) if (token.trim()) counts.set(token, (counts.get(token) ?? 0) + 1)
  return counts
}

// Items whose words overlap at least this much (Dice coefficient, per mille)
// are one edited item. Below it they show as one removal and one insertion.
const minimumItemSimilarity = 500

type ListItemPair = { before?: ListItem; after?: ListItem }

// Pair the differing items between two exact matches by word overlap, so an
// insertion beside an edit doesn't shift the edit onto the inserted item.
function pairSimilarItems(removed: ListItem[], added: ListItem[]) {
  // One item a side has one possible pairing and needs no matrix.
  if (removed.length === 1 && added.length === 1)
    return { pairs: [{ before: removed[0], after: added[0] }] as ListItemPair[], cells: 0 }
  if (!removed.length || !added.length)
    return {
      pairs: [
        ...removed.map((before): ListItemPair => ({ before })),
        ...added.map((after): ListItemPair => ({ after })),
      ],
      cells: 0,
    }
  const left = removed.map((item) => wordCounts(item.text))
  const right = added.map((item) => wordCounts(item.text))
  const size = (counts: Map<string, number>) => [...counts.values()].reduce((sum, count) => sum + count, 0)
  const rightSizes = right.map(size)
  // The score matrix, the weight matrix, and the word scan of every item pair.
  const cells =
    (left.length + 1) * (right.length + 1) +
    left.length * right.length +
    left.reduce((sum, counts) => sum + counts.size, 0) * right.length
  if (cells > maxCells) return undefined
  const weights = left.map((counts) => {
    const leftSize = size(counts)
    return Uint16Array.from(right, (candidate, column) => {
      let shared = 0
      for (const [word, count] of counts) shared += Math.min(count, candidate.get(word) ?? 0)
      const weight = Math.round((2000 * shared) / Math.max(1, leftSize + rightSizes[column]))
      return weight >= minimumItemSimilarity ? weight : 0
    })
  })
  const score = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1))
  for (let i = left.length - 1; i >= 0; i--)
    for (let j = right.length - 1; j >= 0; j--)
      score[i][j] = Math.max(score[i + 1][j], score[i][j + 1], weights[i][j] ? weights[i][j] + score[i + 1][j + 1] : 0)
  const pairs: ListItemPair[] = []
  let i = 0
  let j = 0
  while (i < left.length || j < right.length) {
    if (i < left.length && j < right.length && weights[i][j] && score[i][j] === weights[i][j] + score[i + 1][j + 1])
      pairs.push({ before: removed[i++], after: added[j++] })
    else if (i < left.length && (j === right.length || score[i + 1][j] >= score[i][j + 1]))
      pairs.push({ before: removed[i++] })
    else pairs.push({ after: added[j++] })
  }
  return { pairs, cells }
}

// Align a list block's items and count every matrix the renderer allocates for
// them: item alignment, similar-item pairing, and each pair's token diff.
// Returns undefined when neither side is a list. Without `pairs`, an alignment
// exceeded maxCells, so the block renders whole; `cells` still counts the
// alignment matrix when one was allocated first. Markers don't take part in
// alignment, so a renumbered item stays paired with its own text.
function listComparison(before: string, after: string) {
  const beforeItems = listItems(before)
  const afterItems = listItems(after)
  if (!beforeItems && !afterItems) return undefined
  const plain = (text: string): ListItem[] => (text ? [{ marker: '', depth: 0, text }] : [])
  const left = beforeItems ?? plain(before)
  const right = afterItems ?? plain(after)
  if (before === after) return { cells: 0, pairs: left.map((item): ListItemPair => ({ before: item, after: item })) }
  const alignment = (left.length + 1) * (right.length + 1)
  if (alignment > maxCells) return { cells: 0 }
  let cells = alignment
  const pairs: ListItemPair[] = []
  let oversized = false
  walkAlignment(
    left.map((item) => item.text),
    right.map((item) => item.text),
    (i, j) => pairs.push({ before: left[i], after: right[j] }),
    (removed, added) => {
      const gap = pairSimilarItems(
        removed.map((i) => left[i]),
        added.map((j) => right[j]),
      )
      if (!gap) oversized = true
      else {
        pairs.push(...gap.pairs)
        cells += gap.cells
      }
    },
  )
  if (oversized) return { cells: alignment }
  for (const pair of pairs) cells += tokenCells(pair.before?.text ?? '', pair.after?.text ?? '')
  return { cells, pairs }
}

// Per-item changes for a list block, or undefined for a block that isn't one.
export function listBlockChanges(before: string, after: string): ListItemChange[] | undefined {
  return listComparison(before, after)?.pairs?.map((pair) => ({
    ...pair,
    changes: formattedParagraphChanges(pair.before?.text ?? '', pair.after?.text ?? ''),
  }))
}

export function formattedParagraphChanges(before: string, after: string): FormattedChange[] | undefined {
  const pair = formattedTokens(before, after)
  if (!pair) return undefined
  const [left, right] = pair
  // Identical tokens always take the matching branch below, so skip the matrix.
  let matrix: Uint32Array[] = []
  if (before !== after) {
    if ((left.length + 1) * (right.length + 1) > maxCells) return undefined
    matrix = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1))
    for (let i = left.length - 1; i >= 0; i--)
      for (let j = right.length - 1; j >= 0; j--)
        matrix[i][j] =
          left[i].key === right[j].key ? matrix[i + 1][j + 1] + 1 : Math.max(matrix[i + 1][j], matrix[i][j + 1])
  }
  const changes: FormattedChange[] = []
  const append = (token: InlineToken, kind: WordChange['kind']) => {
    const last = changes.at(-1)
    if (last?.kind === kind && last.style === token.style && last.href === token.href) last.text += token.text
    else changes.push({ text: token.text, kind, style: token.style, ...(token.href ? { href: token.href } : {}) })
  }
  let i = 0
  let j = 0
  while (i < left.length || j < right.length) {
    if (i < left.length && j < right.length && left[i].key === right[j].key) {
      append(left[i++], 'same')
      j++
    } else if (i < left.length && (j === right.length || matrix[i + 1][j] >= matrix[i][j + 1]))
      append(left[i++], 'removed')
    else append(right[j++], 'added')
  }
  return changes
}

export function compareDocuments(before: ReviewSource, after: ReviewSource) {
  const leftText = reviewText(before)
  const rightText = reviewText(after)
  const fallback = { limited: true, leftText, rightText, rows: [] as ReviewRow[] }
  if (leftText.length + rightText.length > maxCharacters) return fallback
  const left = reviewBlocks(before)
  const right = reviewBlocks(after)
  if (left.length + right.length > maxBlocks) return fallback
  const prior = new Map(left.map((block, index) => [block.id, { block, index }]))
  const nextIDs = new Set(right.map((block) => block.id))
  const commonBefore = left.filter((block) => nextIDs.has(block.id)).map((block) => block.id)
  const commonAfter = right.filter((block) => prior.has(block.id)).map((block) => block.id)
  const rows: ReviewRow[] = right.map((block) => {
    const old = prior.get(block.id)?.block
    const moved = !!old && commonBefore.indexOf(block.id) !== commonAfter.indexOf(block.id)
    return {
      id: block.id,
      title: block.title,
      before: old,
      after: block,
      moved,
      changed: !old || old.content !== block.content || moved,
      limited: false,
    }
  })
  // Keep removed sections next to their next surviving neighbour.
  for (let i = 0; i < left.length; i++) {
    const block = left[i]
    if (nextIDs.has(block.id)) continue
    const next = left.slice(i + 1).find((candidate) => nextIDs.has(candidate.id))
    const index = next ? rows.findIndex((row) => row.id === next.id) : rows.length
    rows.splice(index, 0, {
      id: block.id,
      title: block.title,
      before: block,
      moved: false,
      changed: true,
      limited: false,
    })
  }
  let work = 0
  for (const row of rows) {
    if (!row.changed) continue
    const cells = sectionCells(row)
    // A limited section renders complete content and allocates no matrix.
    if (cells > maxSectionCells) row.limited = true
    else work += cells
    if (work > maxDocumentCells) return fallback
  }
  return { limited: false, leftText, rightText, rows }
}

// Section bodies compared by the renderer; a heading block's own heading line
// is already shown as the row title. An absent side stays undefined.
export function reviewRowBodies(row: ReviewRow) {
  const body = (block?: ReviewBlock) =>
    block && row.id.startsWith('heading:') ? block.content.replace(/^\s{0,3}#{1,6}\s+[^\n]+\n?/, '') : block?.content
  return { before: body(row.before), after: body(row.after) }
}

export function reviewRowParagraphs(row: ReviewRow) {
  const { before = '', after = '' } = reviewRowBodies(row)
  return alignedParagraphs(before, after)
}

// Cells the renderer allocates for one changed row: the paragraph-alignment
// matrix plus each aligned pair's token matrix. Returns Infinity when the
// alignment itself exceeds maxCells, because the pairs would then be unbounded.
function sectionCells(row: ReviewRow) {
  const { before = '', after = '' } = reviewRowBodies(row)
  const alignment = (paragraphs(before).length + 1) * (paragraphs(after).length + 1)
  if (alignment > maxCells) return Number.POSITIVE_INFINITY
  let cells = alignment
  for (const pair of alignedParagraphs(before, after)) {
    cells += paragraphCells(pair.before ?? '', pair.after ?? '')
    if (cells > maxSectionCells) break
  }
  return cells
}

function paragraphs(content: string) {
  const parts: string[] = []
  let lines: string[] = []
  let fence: OpenFence | undefined
  for (const line of content.split('\n')) {
    fence = fenceAfterLine(line, fence)
    if (!line.trim() && !fence) {
      if (lines.length) parts.push(lines.join('\n'))
      lines = []
    } else lines.push(line)
  }
  if (lines.length) parts.push(lines.join('\n'))
  return parts
}

export function alignedParagraphs(before: string, after: string): Array<{ before?: string; after?: string }> {
  const left = paragraphs(before)
  const right = paragraphs(after)
  // compareDocuments marks a section limited before this guard applies. The
  // guard also makes this helper safe for standalone callers.
  if ((left.length + 1) * (right.length + 1) > maxCells) return [{ before, after }]
  const pairs: Array<{ before?: string; after?: string }> = []
  walkAlignment(
    left,
    right,
    (i, j) => pairs.push({ before: left[i], after: right[j] }),
    (removedIndexes, addedIndexes) => {
      const removed = removedIndexes.map((i) => left[i])
      const added = addedIndexes.map((j) => right[j])
      // Loose list items are separate paragraphs. A renumbering leaves them no
      // exact match, so join the run and let item alignment pair them.
      if (
        removed.length &&
        added.length &&
        removed.length + added.length > 2 &&
        isLooseListRun(removed) &&
        isLooseListRun(added)
      )
        pairs.push({ before: removed.join('\n\n'), after: added.join('\n\n') })
      else
        for (let n = 0; n < Math.max(removed.length, added.length); n++)
          pairs.push({ before: removed[n], after: added[n] })
    },
  )
  return pairs
}

// Longest-common-subsequence walk shared by paragraph and list item alignment.
// `gap` receives the indexes that differ between two matches, in order.
function walkAlignment(
  left: string[],
  right: string[],
  match: (i: number, j: number) => void,
  gap: (removed: number[], added: number[]) => void,
) {
  const matrix = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1))
  for (let i = left.length - 1; i >= 0; i--)
    for (let j = right.length - 1; j >= 0; j--)
      matrix[i][j] = left[i] === right[j] ? matrix[i + 1][j + 1] + 1 : Math.max(matrix[i + 1][j], matrix[i][j + 1])
  let removed: number[] = []
  let added: number[] = []
  const flush = () => {
    if (removed.length || added.length) gap(removed, added)
    removed = []
    added = []
  }
  let i = 0
  let j = 0
  while (i < left.length || j < right.length) {
    if (i < left.length && j < right.length && left[i] === right[j]) {
      flush()
      match(i++, j++)
    } else if (i < left.length && (j === right.length || matrix[i + 1][j] >= matrix[i][j + 1])) removed.push(i++)
    else added.push(j++)
  }
  flush()
}
