import { type ReactNode, useId, useMemo, useState } from 'react'
import { Button } from '../ui/button'
import { MarkdownProse } from '../ui/markdown-prose'
import {
  alignedParagraphs,
  compareDocuments,
  type FormattedChange,
  formattedParagraphChanges,
  type ReviewRow,
  type ReviewSource,
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
            <p className="mb-2 text-[10px] font-semibold uppercase tracking-wider text-muted">Changed sections</p>
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
  const body = (content = '') =>
    row.id.startsWith('heading:') ? content.replace(/^\s{0,3}#{1,6}\s+[^\n]+\n?/, '') : content
  const pairs = alignedParagraphs(body(row.before?.content), body(row.after?.content))
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

function ReviewParagraph({
  before: beforeSource,
  after: afterSource,
  mode,
}: {
  before?: string
  after?: string
  mode: 'inline' | 'side'
}) {
  const before = beforeSource ?? ''
  const after = afterSource ?? ''
  const words = formattedParagraphChanges(before, after)
  const styled = (word: FormattedChange) => {
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
  const renderWords = (side?: 'before' | 'after') =>
    words?.map((word, index) => {
      if ((side === 'before' && word.kind === 'added') || (side === 'after' && word.kind === 'removed')) return null
      const key = `${index}-${word.kind}`
      return word.kind === 'added' ? (
        <ins key={key} className="bg-positive-soft text-positive">
          {styled(word)}
        </ins>
      ) : word.kind === 'removed' ? (
        <del key={key} className="bg-failure-soft text-failure">
          {styled(word)}
        </del>
      ) : (
        <span key={key}>{styled(word)}</span>
      )
    })
  if (mode === 'inline' && words)
    return (
      <div className="whitespace-pre-wrap break-words p-4 text-sm leading-7">
        {renderWords()}
        {!before && !after && 'Empty section'}
      </div>
    )
  return (
    <div>
      {!words && (
        <p className="px-4 pt-3 text-xs text-muted">
          Detailed highlighting unavailable for this block · showing complete before and after
        </p>
      )}
      <div
        className={
          mode === 'side'
            ? 'grid min-w-0 divide-y divide-border md:grid-cols-2 md:divide-x md:divide-y-0'
            : 'min-w-0 divide-y divide-border'
        }
      >
        {(['before', 'after'] as const).map((side) => (
          <div key={side} className="min-w-0 p-4">
            <p className="mb-3 text-[10px] font-semibold uppercase tracking-wider text-muted">
              {side === 'before' ? 'Base' : 'Target'}
            </p>
            {(side === 'before' ? beforeSource : afterSource) === undefined ? (
              <p className="text-sm italic text-muted">No section in this version</p>
            ) : words ? (
              <div className="whitespace-pre-wrap break-words text-sm leading-7">{renderWords(side)}</div>
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
