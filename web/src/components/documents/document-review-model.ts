import type { RequirementVersion } from '../../lib/types'

export type ReviewBlock = { id: string; title: string; content: string }
export type WordChange = { text: string; kind: 'same' | 'added' | 'removed' }
export type InlineStyle = 'text' | 'code' | 'emphasis' | 'strong' | 'link'
export type FormattedChange = WordChange & { style: InlineStyle; href?: string }
export type ReviewRow = {
  id: string
  title: string
  before?: ReviewBlock
  after?: ReviewBlock
  moved: boolean
  changed: boolean
}
export type ReviewSource = { content: string; statements?: RequirementVersion['statements'] }

// component-web-dashboard v6: bound work before allocating matrices or parsing Markdown.
const maxCharacters = 120_000
const maxBlocks = 400
const maxCells = 250_000

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

export function headingBlocks(content: string): ReviewBlock[] {
  const blocks: ReviewBlock[] = []
  const occurrences = new Map<string, number>()
  let title = 'Overview'
  let lines: string[] = []
  let fence: string | undefined
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
    const marker = line.match(/^\s{0,3}(`{3,}|~{3,})/)
    if (marker) {
      if (!fence) fence = marker[1]
      else if (marker[1][0] === fence[0] && marker[1].length >= fence.length && /^\s{0,3}(`+|~+)\s*$/.test(line))
        fence = undefined
    }
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
  return text.length > 0 && !/[`*_[\]<>\\]/.test(text)
}

// Parse only the bounded, ordinary inline subset owned by the document review
// renderer. Block Markdown and ambiguous/malformed inline syntax deliberately
// return undefined so callers can render both complete sources with MarkdownProse.
export function formattedParagraph(text: string): InlineRun[] | undefined {
  if (
    /(^|\n)\s{0,3}(?:#{1,6}\s|>|[-+*]\s|\d+[.)]\s|`{3,}|~{3,})/.test(text) ||
    /(^|\n)\s{0,3}(?:=+|-+)\s*(?:\n|$)/.test(text) ||
    /(^|\n).*\|.*(?:\n|$)/.test(text) ||
    /<|>|~|\\|!\[/.test(text)
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

export function formattedParagraphChanges(before: string, after: string): FormattedChange[] | undefined {
  const beforeRuns = formattedParagraph(before)
  const afterRuns = formattedParagraph(after)
  if (!beforeRuns || !afterRuns) return undefined
  const left = inlineTokens(beforeRuns)
  const right = inlineTokens(afterRuns)
  if ((left.length + 1) * (right.length + 1) > maxCells) return undefined
  const matrix = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1))
  for (let i = left.length - 1; i >= 0; i--)
    for (let j = right.length - 1; j >= 0; j--)
      matrix[i][j] =
        left[i].key === right[j].key ? matrix[i + 1][j + 1] + 1 : Math.max(matrix[i + 1][j], matrix[i][j + 1])
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
    }
  })
  // Keep removed sections next to their next surviving neighbour.
  for (let i = 0; i < left.length; i++) {
    const block = left[i]
    if (nextIDs.has(block.id)) continue
    const next = left.slice(i + 1).find((candidate) => nextIDs.has(candidate.id))
    const index = next ? rows.findIndex((row) => row.id === next.id) : rows.length
    rows.splice(index, 0, { id: block.id, title: block.title, before: block, moved: false, changed: true })
  }
  let work = 0
  for (const row of rows) {
    if (!row.changed || !row.before || !row.after) continue
    work += (tokens(row.before.content).length + 1) * (tokens(row.after.content).length + 1)
    if (work > maxCells) return fallback
  }
  return { limited: false, leftText, rightText, rows }
}

function paragraphs(content: string) {
  const parts: string[] = []
  let lines: string[] = []
  let fence: string | undefined
  for (const line of content.split('\n')) {
    const marker = line.match(/^\s{0,3}(`{3,}|~{3,})/)
    if (marker) {
      if (!fence) fence = marker[1]
      else if (marker[1][0] === fence[0] && marker[1].length >= fence.length && /^\s{0,3}(`+|~+)\s*$/.test(line))
        fence = undefined
    }
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
  // Each section already passed the aggregate token-work bound. This guard
  // also makes this helper safe for standalone callers.
  if ((left.length + 1) * (right.length + 1) > maxCells) return [{ before, after }]
  const matrix = Array.from({ length: left.length + 1 }, () => new Uint32Array(right.length + 1))
  for (let i = left.length - 1; i >= 0; i--)
    for (let j = right.length - 1; j >= 0; j--)
      matrix[i][j] = left[i] === right[j] ? matrix[i + 1][j + 1] + 1 : Math.max(matrix[i + 1][j], matrix[i][j + 1])
  const pairs: Array<{ before?: string; after?: string }> = []
  let removed: string[] = []
  let added: string[] = []
  const flush = () => {
    for (let n = 0; n < Math.max(removed.length, added.length); n++) pairs.push({ before: removed[n], after: added[n] })
    removed = []
    added = []
  }
  let i = 0
  let j = 0
  while (i < left.length || j < right.length) {
    if (i < left.length && j < right.length && left[i] === right[j]) {
      flush()
      pairs.push({ before: left[i++], after: right[j++] })
    } else if (i < left.length && (j === right.length || matrix[i + 1][j] >= matrix[i][j + 1])) removed.push(left[i++])
    else added.push(right[j++])
  }
  flush()
  return pairs
}
