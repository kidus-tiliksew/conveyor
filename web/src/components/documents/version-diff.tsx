import { type ReactNode, useId, useMemo, useState } from 'react'
import { Button } from '../ui/button'
import { MarkdownProse } from '../ui/markdown-prose'
import {
  compareDocuments,
  type FormattedChange,
  formattedParagraphChanges,
  type ListItem,
  type ListItemChange,
  listBlockChanges,
  type ReviewRow,
  type ReviewSource,
  reviewRowBodies,
  reviewRowParagraphs,
} from './document-review-model'

type DiffLine = { text: string; changed: boolean }

export type VersionDiffSide = {
  content: string
  label?: string
  labelClassName?: string
  paneClassName?: string
  preClassName?: string
}

export function VersionDiff({
  left,
  right,
  bounded = false,
  enabled = true,
  className = 'grid gap-px border-t border-border bg-border md:grid-cols-2',
  noticeClassName = 'border-t border-border px-4 py-2 text-xs text-muted',
}: {
  left: VersionDiffSide
  right: VersionDiffSide
  bounded?: boolean
  enabled?: boolean
  className?: string
  noticeClassName?: string
}) {
  const comparison = useMemo(() => {
    if (!enabled) return undefined
    return bounded ? boundedLineChanges(left.content, right.content) : boundedLineChanges(left.content, right.content)
  }, [bounded, enabled, left.content, right.content])
  const [leftLines, rightLines] = comparison?.lines ?? [[], []]

  return (
    <>
      {comparison?.limited && (
        <p className={noticeClassName}>Diff too large; showing both versions without highlighting.</p>
      )}
      <div className={className}>
        <DiffSide side={left} lines={leftLines} changedClassName="bg-failure-soft text-failure" />
        <DiffSide side={right} lines={rightLines} changedClassName="bg-positive-soft text-positive" />
      </div>
    </>
  )
}

function DiffSide({
  side,
  lines,
  changedClassName,
}: {
  side: VersionDiffSide
  lines: DiffLine[]
  changedClassName: string
}) {
  return (
    <div className={side.paneClassName ?? 'bg-card p-4'}>
      {side.label && <p className={side.labelClassName ?? 'mb-2 text-xs font-medium'}>{side.label}</p>}
      <pre className={side.preClassName ?? 'whitespace-pre-wrap font-sans text-xs leading-5'}>
        {(lines.length ? lines : side.content.split('\n').map((text) => ({ text, changed: false }))).map(
          (line, index) => (
            <span key={`${index}-${line.text}`} className={line.changed ? `block ${changedClassName}` : 'block'}>
              {line.text || ' '}
            </span>
          ),
        )}
      </pre>
    </div>
  )
}

export function lineChanges(current: string, pending: string): readonly [DiffLine[], DiffLine[]] {
  const left = current.split('\n')
  const right = pending.split('\n')
  const common = Array.from({ length: left.length + 1 }, () => Array<number>(right.length + 1).fill(0))
  for (let i = left.length - 1; i >= 0; i--) {
    for (let j = right.length - 1; j >= 0; j--) {
      common[i][j] = left[i] === right[j] ? common[i + 1][j + 1] + 1 : Math.max(common[i + 1][j], common[i][j + 1])
    }
  }
  const unchangedLeft = new Set<number>()
  const unchangedRight = new Set<number>()
  let i = 0
  let j = 0
  while (i < left.length && j < right.length) {
    if (left[i] === right[j]) {
      unchangedLeft.add(i++)
      unchangedRight.add(j++)
    } else if (common[i + 1][j] >= common[i][j + 1]) i++
    else j++
  }
  return [
    left.map((text, index) => ({ text, changed: !unchangedLeft.has(index) })),
    right.map((text, index) => ({ text, changed: !unchangedRight.has(index) })),
  ]
}

const maxDiffMatrixCells = 250_000

export function boundedLineChanges(current: string, pending: string) {
  const left = current.split('\n')
  const right = pending.split('\n')
  if ((left.length + 1) * (right.length + 1) > maxDiffMatrixCells) {
    return {
      limited: true,
      lines: [left.map((text) => ({ text, changed: false })), right.map((text) => ({ text, changed: false }))] as const,
    }
  }
  return { limited: false, lines: lineChanges(current, pending) }
}

/** Shared review rendering; legacy editor callers above retain their contract. */
export function DocumentComparison({ before, after }: { before: ReviewSource; after: ReviewSource }) {
  const comparison = useMemo(() => compareDocuments(before, after), [before, after])
  const [mode, setMode] = useState<'inline' | 'side'>('inline')
  const [active, setActive] = useState(0)
  const prefix = useId()
  const changed = comparison.rows.filter((row) => row.changed)
  const go = (index: number) => {
    setActive(index)
    document.getElementById(`${prefix}-${changed[index]?.id}`)?.focus()
  }
  if (comparison.limited)
    return (
      <section aria-label="Version comparison" className="mt-6 min-w-0 space-y-4">
        <p role="status" className="rounded-md border border-attention/30 bg-attention-soft p-3 text-sm">
          Diff too large; showing both complete versions without highlighting. Detailed section navigation is
          unavailable.
        </p>
        <div className="grid min-w-0 gap-4 md:grid-cols-2">
          {[
            ['Base version', comparison.leftText],
            ['Target version', comparison.rightText],
          ].map(([label, text]) => (
            <div key={label} className="min-w-0">
              <h3 className="mb-2 text-sm font-semibold">{label}</h3>
              <pre className="max-h-[70vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border p-4 text-xs">
                {text || 'Empty version'}
              </pre>
            </div>
          ))}
        </div>
      </section>
    )
  return (
    <section aria-label="Version comparison" className="mt-6 min-w-0">
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <p className="text-sm text-muted">
          {changed.length ? `${changed.length} changed sections` : 'No changes between these versions.'}
        </p>
        <div className="flex gap-2">
          <Button
            variant={mode === 'inline' ? 'default' : 'secondary'}
            aria-pressed={mode === 'inline'}
            onClick={() => setMode('inline')}
          >
            Inline
          </Button>
          <Button
            variant={mode === 'side' ? 'default' : 'secondary'}
            aria-pressed={mode === 'side'}
            onClick={() => setMode('side')}
          >
            Side by side
          </Button>
        </div>
      </div>
      <div className={changed.length ? 'grid min-w-0 gap-4 lg:grid-cols-[10rem_minmax(0,1fr)]' : 'min-w-0'}>
        {changed.length > 0 && (
          <nav aria-label="Changed sections" className="min-w-0">
            <p className="mb-2 text-xs font-semibold text-muted">Changed sections</p>
            <div className="flex flex-wrap gap-1 lg:flex-col">
              {changed.map((row, index) => (
                <button
                  type="button"
                  key={row.id}
                  aria-current={active === index ? 'true' : undefined}
                  className={`rounded-md p-2 text-left text-xs break-words ${active === index ? 'bg-primary-soft text-primary' : 'text-muted hover:bg-surface'}`}
                  onClick={() => go(index)}
                >
                  {row.title}
                </button>
              ))}
            </div>
            <div className="mt-4 flex flex-wrap gap-2">
              <Button size="sm" variant="secondary" disabled={active === 0} onClick={() => go(active - 1)}>
                Previous
              </Button>
              <Button
                size="sm"
                variant="secondary"
                disabled={active >= changed.length - 1}
                onClick={() => go(active + 1)}
              >
                Next
              </Button>
            </div>
            <p className="mt-2 text-xs text-muted">
              {active + 1} of {changed.length} changes
            </p>
          </nav>
        )}
        <div className="min-w-0 space-y-4">
          {changed.length > 0 && (
            <p className="text-xs text-muted">
              Where detailed paragraph highlighting is available:{' '}
              <del className="bg-failure-soft text-failure">Removed text</del>{' '}
              <ins className="bg-positive-soft text-positive">Added text</ins>. Blocks labeled unavailable show complete
              content instead.
            </p>
          )}
          {!comparison.rows.length && <p className="text-sm text-muted">Both versions are empty.</p>}
          {comparison.rows.map((row) =>
            row.changed ? (
              <section
                key={row.id}
                id={`${prefix}-${row.id}`}
                tabIndex={-1}
                aria-label={row.title}
                className="min-w-0 scroll-mt-[42vh] overflow-hidden rounded-lg border border-border focus:outline-2 focus:outline-primary"
              >
                <h3 className="flex flex-wrap justify-between gap-2 border-b border-border bg-surface px-4 py-3 text-sm font-semibold">
                  <span>{row.title}</span>
                  <span className="text-xs font-normal text-muted">
                    {!row.before ? 'Added' : !row.after ? 'Deleted' : row.moved ? 'Moved' : 'Modified'}
                  </span>
                </h3>
                <ReviewRowContent row={row} mode={mode} />
              </section>
            ) : (
              <details key={row.id} className="min-w-0 rounded-lg border border-dashed border-border p-4">
                <summary className="cursor-pointer text-xs text-muted">Show unchanged section · {row.title}</summary>
                <div className="mt-4 min-w-0 overflow-auto">
                  <MarkdownProse>{row.after?.content ?? ''}</MarkdownProse>
                </div>
              </details>
            ),
          )}
        </div>
      </div>
    </section>
  )
}

function ReviewRowContent({ row, mode }: { row: ReviewRow; mode: 'inline' | 'side' }) {
  // A limited section exceeded the section budget: show both complete sources.
  if (row.limited) {
    const { before, after } = reviewRowBodies(row)
    return (
      <div className="min-w-0">
        <ReviewParagraph before={before} after={after} mode={mode} complete />
      </div>
    )
  }
  const pairs = reviewRowParagraphs(row)
  return (
    <div className="min-w-0">
      {pairs.map((pair, index) => (
        <ReviewParagraph
          key={`${index}-${pair.before}-${pair.after}`}
          before={pair.before}
          after={pair.after}
          mode={mode}
        />
      ))}
    </div>
  )
}

const unavailableNotice = 'Detailed highlighting unavailable for this block · showing complete before and after'
const insertedClassName = 'bg-positive-soft text-positive'
const removedClassName = 'bg-failure-soft text-failure'
type ReviewSide = 'before' | 'after'

function styledChange(word: FormattedChange) {
  let content: ReactNode = word.text
  if (word.style === 'code') content = <code>{content}</code>
  else if (word.style === 'emphasis') content = <em>{content}</em>
  else if (word.style === 'strong') content = <strong>{content}</strong>
  else if (word.style === 'link')
    content = (
      <a href={word.href} className="text-primary underline underline-offset-2">
        {content}
      </a>
    )
  return content
}

// With a side, renders that side's text; without one, renders both inline.
function ChangedWords({ words, side }: { words: FormattedChange[]; side?: ReviewSide }) {
  return words.map((word, index) => {
    if ((side === 'before' && word.kind === 'added') || (side === 'after' && word.kind === 'removed')) return null
    const key = `${index}-${word.kind}`
    return word.kind === 'added' ? (
      <ins key={key} className={insertedClassName}>
        {styledChange(word)}
      </ins>
    ) : word.kind === 'removed' ? (
      <del key={key} className={removedClassName}>
        {styledChange(word)}
      </del>
    ) : (
      <span key={key}>{styledChange(word)}</span>
    )
  })
}

function ReviewParagraph({
  before: beforeSource,
  after: afterSource,
  mode,
  complete = false,
}: {
  before?: string
  after?: string
  mode: 'inline' | 'side'
  complete?: boolean
}) {
  const before = beforeSource ?? ''
  const after = afterSource ?? ''
  // The block's key holds its text, so a mode change reuses this alignment.
  const list = useMemo(() => (complete ? undefined : listBlockChanges(before, after)), [after, before, complete])
  if (list)
    return (
      <ReviewListBlock
        items={list}
        mode={mode}
        beforeAbsent={beforeSource === undefined}
        afterAbsent={afterSource === undefined}
      />
    )
  const words = complete ? undefined : formattedParagraphChanges(before, after)
  if (mode === 'inline' && words)
    return (
      <div className="whitespace-pre-wrap break-words p-4 text-sm leading-7">
        <ChangedWords words={words} />
        {!before && !after && 'Empty section'}
      </div>
    )
  return (
    <div>
      {!words && <p className="px-4 pt-3 text-xs text-muted">{unavailableNotice}</p>}
      <div
        className={
          mode === 'side'
            ? 'grid min-w-0 divide-y divide-border md:grid-cols-2 md:divide-x md:divide-y-0'
            : 'min-w-0 divide-y divide-border'
        }
      >
        {(['before', 'after'] as const).map((side) => (
          <div key={side} className="min-w-0 p-4">
            <SideLabel side={side} />
            {(side === 'before' ? beforeSource : afterSource) === undefined ? (
              <AbsentSide />
            ) : words ? (
              <div className="whitespace-pre-wrap break-words text-sm leading-7">
                <ChangedWords words={words} side={side} />
              </div>
            ) : (
              <div className="min-w-0 overflow-auto">
                <MarkdownProse>{side === 'before' ? before : after}</MarkdownProse>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}

function SideLabel({ side }: { side: ReviewSide }) {
  return <p className="mb-3 text-xs font-semibold text-muted">{side === 'before' ? 'Base' : 'Target'}</p>
}

function AbsentSide() {
  return <p className="text-sm italic text-muted">No section in this version</p>
}

// Item rows hold their markers as text, not list counters, so a list starting
// at 31 shows 31. Both modes keep one row per aligned pair, so an inserted item
// can't shift the rows after it (component-web-document-surfaces "Block
// comparison").
function ReviewListBlock({
  items,
  mode,
  beforeAbsent,
  afterAbsent,
}: {
  items: ListItemChange[]
  mode: 'inline' | 'side'
  beforeAbsent: boolean
  afterAbsent: boolean
}) {
  const rows = (
    <ul className="min-w-0 list-none space-y-2 text-sm leading-7">
      {items.map((item, index) => (
        <ReviewListItem key={`${index}-${item.before?.text}-${item.after?.text}`} item={item} mode={mode} />
      ))}
    </ul>
  )
  if (mode === 'inline') return <div className="min-w-0 p-4">{rows}</div>
  return (
    <div className="min-w-0 space-y-3 p-4">
      <div className="grid grid-cols-2 gap-4">
        {(['before', 'after'] as const).map((side) => (
          <div key={side} className="min-w-0">
            <SideLabel side={side} />
            {(side === 'before' ? beforeAbsent : afterAbsent) && <AbsentSide />}
          </div>
        ))}
      </div>
      {rows}
    </div>
  )
}

function ReviewListItem({ item, mode }: { item: ListItemChange; mode: 'inline' | 'side' }) {
  const { before, after, changes } = item
  // A changed item without safe marks falls back alone. An identical item has
  // nothing to highlight, so it renders whole with no notice.
  const unavailable = !changes && before?.text !== after?.text
  const sides = (['before', 'after'] as const).filter((side) => item[side])
  const indented = { marginLeft: `${((after ?? before)?.depth ?? 0) * 1.5}rem` }
  if (mode === 'side')
    return (
      <li className="min-w-0 space-y-1" data-review-list-item>
        {unavailable && <p className="text-xs text-muted">{unavailableNotice}</p>}
        <div className="grid grid-cols-2 gap-4">
          {(['before', 'after'] as const).map((side) => (
            <div key={side} className="min-w-0" style={{ marginLeft: `${(item[side]?.depth ?? 0) * 1.5}rem` }}>
              {item[side] && (
                <ListItemLine marker={<SideMarker item={item} side={side} />}>
                  {changes ? <ChangedWords words={changes} side={side} /> : <ItemProse item={item[side]} />}
                </ListItemLine>
              )}
            </div>
          ))}
        </div>
      </li>
    )
  if (unavailable)
    return (
      <li className="min-w-0 space-y-1" style={indented} data-review-list-item>
        <p className="text-xs text-muted">{unavailableNotice}</p>
        {sides.map((side) => (
          <div key={side} className="min-w-0">
            <SideLabel side={side} />
            <ListItemLine marker={<SideMarker item={item} side={side} />}>
              <ItemProse item={item[side]} />
            </ListItemLine>
          </div>
        ))}
      </li>
    )
  return (
    <li className="min-w-0" style={indented} data-review-list-item>
      <ListItemLine marker={<InlineMarker item={item} />}>
        {changes ? <ChangedWords words={changes} /> : <ItemProse item={after ?? before} />}
      </ListItemLine>
    </li>
  )
}

function ListItemLine({ marker, children }: { marker: ReactNode; children: ReactNode }) {
  return (
    <div className="flex min-w-0 gap-2">
      {marker}
      <div className="min-w-0 flex-1 whitespace-pre-wrap break-words">{children}</div>
    </div>
  )
}

function ItemProse({ item }: { item?: ListItem }) {
  return (
    <div className="min-w-0 overflow-auto whitespace-normal">
      <MarkdownProse>{item?.text ?? ''}</MarkdownProse>
    </div>
  )
}

// Every bullet character renders the same list, so all show one bullet.
function markerLabel(item?: ListItem) {
  return item && /^[-+*]$/.test(item.marker) ? '•' : (item?.marker ?? '')
}

// A marker counts as changed when its label or nesting differs. An inserted or
// removed item has only one marker, which is its change.
function markerChanged({ before, after }: ListItemChange) {
  return !before || !after || markerLabel(before) !== markerLabel(after) || before.depth !== after.depth
}

function Marker({ label, kind }: { label: string; kind?: 'added' | 'removed' }) {
  if (!label) return null
  if (kind === 'added') return <ins className={insertedClassName}>{label}</ins>
  if (kind === 'removed') return <del className={removedClassName}>{label}</del>
  return <span>{label}</span>
}

function MarkerSlot({ children }: { children: ReactNode }) {
  return <span className="shrink-0 tabular-nums text-muted">{children}</span>
}

function SideMarker({ item, side }: { item: ListItemChange; side: ReviewSide }) {
  const label = markerLabel(item[side])
  if (!label) return null
  return (
    <MarkerSlot>
      <Marker label={label} kind={!markerChanged(item) ? undefined : side === 'before' ? 'removed' : 'added'} />
    </MarkerSlot>
  )
}

function InlineMarker({ item }: { item: ListItemChange }) {
  const before = markerLabel(item.before)
  const after = markerLabel(item.after)
  if (!before && !after) return null
  if (!markerChanged(item))
    return (
      <MarkerSlot>
        <Marker label={after} />
      </MarkerSlot>
    )
  return (
    <MarkerSlot>
      <Marker label={before} kind="removed" />
      {before && after && ' '}
      <Marker label={after} kind="added" />
    </MarkerSlot>
  )
}
