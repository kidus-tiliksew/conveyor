import { useMemo } from 'react'

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
    return bounded
      ? boundedLineChanges(left.content, right.content)
      : { limited: false, lines: lineChanges(left.content, right.content) }
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
        {lines.map((line, index) => (
          <span key={`${index}-${line.text}`} className={line.changed ? `block ${changedClassName}` : 'block'}>
            {line.text || ' '}
          </span>
        ))}
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
