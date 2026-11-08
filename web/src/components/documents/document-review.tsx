import { type ReactNode, useEffect, useId, useRef } from 'react'
import { actorName, fullTime } from '../../lib/utils'
import { useWorkspaceMembers } from '../app-shell'
import { Button } from '../ui/button'
import { Select } from '../ui/input'
import { Tabs, tabPanelProps } from '../ui/tabs'
import type { ReviewSource } from './document-review-model'
import { DocumentComparison } from './version-diff'

export type ReviewSearch = { tab?: 'document' | 'changes' | 'history'; base?: number; target?: number }
export type ReviewVersion = ReviewSource & {
  version: number
  confirmed: boolean
  retired?: boolean
  dismissed?: boolean
  retired_by_version?: number
  created_at: string
  origin: string
  origin_task_id?: string
  origin_session_id?: string
  confirmed_by?: string
  confirmed_at?: string
  retired_by?: string
  retired_at?: string
  dismissed_by?: string
  dismissed_at?: string
  dismissal_note?: string
}
export function validateReviewSearch(search: Record<string, unknown>): ReviewSearch {
  const version = (value: unknown, minimum: number) => {
    if (typeof value !== 'number' && typeof value !== 'string') return undefined
    if (value === '') return undefined
    const n = Number(value)
    return Number.isSafeInteger(n) && n >= minimum ? n : undefined
  }
  return {
    tab: search.tab === 'document' || search.tab === 'changes' || search.tab === 'history' ? search.tab : undefined,
    base: version(search.base, 0),
    target: version(search.target, 1),
  }
}
export function versionState(version: ReviewVersion) {
  return version.retired_by_version
    ? `Superseded by v${version.retired_by_version}`
    : version.retired || version.dismissed
      ? 'Dismissed'
      : version.confirmed
        ? 'Confirmed'
        : 'Proposed'
}
// Lifecycle identity is neutral metadata; pending actions stay in AttentionSurface.
export function reviewVersionLabel(version: ReviewVersion, hasConfirmedVersion: boolean) {
  return `v${version.version}${hasConfirmedVersion ? '' : ` · ${versionState(version)}`}`
}

export const dismissalArchiveConsequence = 'Dismissing this version archives the document.'

export function archivedByDismissal(document: { archived?: boolean; archive_reason?: string }) {
  return document.archived && document.archive_reason === 'only_proposal_dismissed'
}

export function dismissalArchiveDescription(document: {
  archived?: boolean
  archive_reason?: string
  archive_note?: string
}) {
  if (!archivedByDismissal(document)) return undefined
  return `Archived because its only proposal was dismissed.${document.archive_note ? ` Dismissal note: ${document.archive_note}` : ''}`
}

export function selectedReviewVersion<T extends ReviewVersion>(
  versions: T[],
  current: T | undefined,
  search: ReviewSearch,
) {
  return search.target === undefined
    ? (current ?? versions.find((v) => !v.retired && !v.dismissed) ?? versions.at(-1))
    : versions.find((v) => v.version === search.target)
}

// Changes opens on something worth reading: the newest proposal against the
// confirmed version, or the confirmed version against the one before it.
export function changesTargetVersion<T extends ReviewVersion>(
  versions: T[],
  current: T | undefined,
  search: ReviewSearch,
) {
  const pending = versions
    .filter((v) => !v.confirmed && !v.retired && !v.dismissed && v.version !== current?.version)
    .sort((a, b) => b.version - a.version)[0]
  return search.target === undefined
    ? (pending ?? selectedReviewVersion(versions, current, search))
    : selectedReviewVersion(versions, current, search)
}

// req-document-operating-surfaces REQ-1/REQ-2 and component-web-document-surfaces:
// the caller supplies exactly one attention surface, never duplicated by tabs.
export function DocumentReview({
  search,
  onChange,
  versions,
  current,
  attention,
  canActOnPending = false,
  document,
  history,
  loading,
  error,
}: {
  search: ReviewSearch
  onChange: (next: ReviewSearch) => void | Promise<void>
  versions: ReviewVersion[]
  current?: ReviewVersion
  attention: ReactNode
  canActOnPending?: boolean
  document: ReactNode
  history: ReactNode
  loading?: boolean
  error?: string
}) {
  const id = useId()
  const { data: members = [] } = useWorkspaceMembers()
  const tab = search.tab ?? 'document'
  const target =
    tab === 'changes'
      ? changesTargetVersion(versions, current, search)
      : selectedReviewVersion(versions, current, search)
  const previous = versions.filter((v) => target && v.version < target.version).sort((a, b) => b.version - a.version)[0]
  const baseNumber =
    search.base ??
    (target && current && target.version !== current.version
      ? current.version
      : (previous?.version ?? current?.version ?? 0))
  const base = versions.find((v) => v.version === baseNumber)
  const invalid = (search.target !== undefined && !target) || (tab === 'changes' && baseNumber !== 0 && !base)
  const tabs = ['document', 'changes', 'history'] as const
  const select = (next: ReviewSearch) => onChange({ ...search, ...next })
  // The band stays in reach only while reading a version it can act on.
  const pinned =
    tab === 'changes' &&
    canActOnPending &&
    Boolean(target) &&
    !invalid &&
    !target?.confirmed &&
    !target?.retired &&
    !target?.dismissed
  // The comparison's section list sits below the pinned band, whatever its height.
  const root = useRef<HTMLDivElement>(null)
  const band = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const host = root.current
    const element = band.current
    if (!host || !element) return
    const update = () => {
      const stuck = getComputedStyle(element).position === 'sticky'
      host.style.setProperty('--review-sticky-offset', `${stuck ? element.offsetHeight : 0}px`)
    }
    update()
    const observer = new ResizeObserver(update)
    observer.observe(element)
    return () => observer.disconnect()
  }, [pinned])
  return (
    <div ref={root} className="min-w-0">
      <Tabs
        id={id}
        aria-label="Document views"
        className="mb-5"
        items={tabs.map((name) => ({ id: name, label: <span className="capitalize">{name}</span> }))}
        value={tab}
        onChange={(name) => onChange({ ...search, tab: name })}
      />
      <div
        ref={band}
        className={
          pinned ? 'md:sticky md:top-0 md:z-10 md:max-h-[40vh] md:overflow-y-auto md:bg-background md:pb-3' : undefined
        }
      >
        {attention}
      </div>
      <div {...tabPanelProps(id, tab)} className="mt-6 min-w-0">
        {error && (
          <p role="alert" className="mb-4 text-sm text-failure">
            {error}
          </p>
        )}
        {loading ? (
          <p role="status" className="text-sm text-muted">
            Loading versions…
          </p>
        ) : invalid ? (
          <div role="alert" className="space-y-3">
            <p>The selected version is unavailable for this document.</p>
            <Button variant="secondary" onClick={() => onChange({ tab })}>
              Reset version selection
            </Button>
          </div>
        ) : (
          <>
            {tab === 'document' && document}
            {tab === 'changes' && (
              <>
                <h3 className="mb-4 text-lg font-semibold">
                  {target ? `Review version ${target.version}` : 'Version comparison'}
                </h3>
                <div className="grid min-w-0 gap-3 sm:grid-cols-2">
                  <label htmlFor={`${id}-base`} className="min-w-0 text-xs text-muted">
                    Base version
                    <Select
                      id={`${id}-base`}
                      aria-label="Base version"
                      className="mt-1.5"
                      value={baseNumber}
                      onChange={(e) => select({ base: Number(e.target.value) })}
                    >
                      <option value={0}>Empty base</option>
                      {versions.map((v) => (
                        <option key={v.version} value={v.version}>
                          v{v.version} · {versionState(v)}
                        </option>
                      ))}
                    </Select>
                  </label>
                  <label htmlFor={`${id}-target`} className="min-w-0 text-xs text-muted">
                    Target version
                    <Select
                      id={`${id}-target`}
                      aria-label="Target version"
                      className="mt-1.5"
                      value={target?.version ?? ''}
                      onChange={(e) => select({ target: Number(e.target.value) })}
                    >
                      {!target && <option value="">No version</option>}
                      {versions.map((v) => (
                        <option key={v.version} value={v.version}>
                          v{v.version} · {versionState(v)}
                        </option>
                      ))}
                    </Select>
                  </label>
                </div>
                {baseNumber === 0 && (
                  <p className="mt-3 text-xs text-muted">
                    Comparing against an empty base{!current ? ': no confirmed version exists yet' : ''}.
                  </p>
                )}
                <DocumentComparison
                  key={`${baseNumber}:${target?.version}`}
                  before={base ?? { content: '' }}
                  after={target ?? { content: '' }}
                />
              </>
            )}
            {tab === 'history' && (
              <>
                <section aria-label="Version history">
                  <h3 className="mb-4 text-lg font-semibold">Version history</h3>
                  <ol className="space-y-3">
                    {[...versions]
                      .sort((a, b) => b.version - a.version)
                      .map((v) => (
                        <li key={v.version} className="rounded-lg border border-border p-4 text-sm">
                          <h4 className="font-semibold">
                            v{v.version} · {versionState(v)}
                          </h4>
                          <p className="mt-1 break-words text-xs text-muted">
                            {v.origin.replaceAll('_', ' ')}
                            {v.origin_task_id && ` · Task ${v.origin_task_id}`}
                            {v.origin_session_id && ` · Session ${v.origin_session_id}`} · {fullTime(v.created_at)}
                          </p>
                          {v.confirmed_by && (
                            <p className="mt-1 text-xs text-muted">
                              Confirmed by {actorName(v.confirmed_by, members)}
                              {v.confirmed_at && ` on ${fullTime(v.confirmed_at)}`}
                            </p>
                          )}
                          {(v.dismissed_by || v.retired_by) && (
                            <p className="mt-1 text-xs text-muted">
                              Dismissed by {v.dismissed_by ?? v.retired_by}
                              {(v.dismissed_at || v.retired_at) &&
                                ` on ${fullTime(v.dismissed_at ?? v.retired_at ?? '')}`}
                            </p>
                          )}
                          {v.dismissal_note && (
                            <p className="mt-2 whitespace-pre-wrap break-words text-xs text-muted">
                              Operator's reason: {v.dismissal_note}
                            </p>
                          )}
                          <div className="mt-3 flex flex-wrap gap-2">
                            <Button
                              size="sm"
                              variant="secondary"
                              onClick={() => select({ tab: 'document', target: v.version })}
                            >
                              Read version {v.version}
                            </Button>
                            <Button
                              size="sm"
                              variant="secondary"
                              onClick={() =>
                                select({
                                  tab: 'changes',
                                  target: v.version,
                                  base:
                                    versions
                                      .filter((candidate) => candidate.confirmed && candidate.version < v.version)
                                      .sort((a, b) => b.version - a.version)[0]?.version ?? 0,
                                })
                              }
                            >
                              Compare version {v.version}
                            </Button>
                          </div>
                        </li>
                      ))}
                  </ol>
                </section>
                {history}
              </>
            )}
          </>
        )}
      </div>
    </div>
  )
}
