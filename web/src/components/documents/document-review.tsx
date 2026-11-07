import { type ReactNode, useId } from 'react'
import { Button } from '../ui/button'
import type { ReviewSource } from './document-review-model'
import { DocumentComparison } from './version-diff'
import { actorName } from '../../lib/activity'
import { dateTimeLabel } from '../../lib/utils'
import { useWorkspaceMembers } from '../app-shell'
import { Select } from '../ui/input'

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

// req-document-operating-surfaces REQ-1/REQ-2 and component-web-document-surfaces:
// the caller supplies exactly one attention surface, never duplicated by tabs.
export function DocumentReview({
  search,
  onChange,
  versions,
  current,
  attention,
  document,
  history,
  actions,
  loading,
  error,
}: {
  search: ReviewSearch
  onChange: (next: ReviewSearch) => void | Promise<void>
  versions: ReviewVersion[]
  current?: ReviewVersion
  attention: ReactNode
  document: ReactNode
  history: ReactNode
  actions?: ReactNode
  loading?: boolean
  error?: string
}) {
  const id = useId()
  const members = useWorkspaceMembers().data ?? []
  const tab = search.tab ?? 'document'
  const target = selectedReviewVersion(versions, current, search)
  // Comparing the current version against itself says nothing, so it opens
  // against the version it replaced.
  const previous = [...versions]
    .filter((v) => v.version < (target?.version ?? 0) && v.confirmed_at)
    .sort((a, b) => b.version - a.version)[0]
  const baseNumber =
    search.base ?? (target && target.version === current?.version ? (previous?.version ?? 0) : (current?.version ?? 0))
  const base = versions.find((v) => v.version === baseNumber)
  const invalid = (search.target !== undefined && !target) || (tab === 'changes' && baseNumber !== 0 && !base)
  const tabs = ['document', 'changes', 'history'] as const
  const select = (next: ReviewSearch) => onChange({ ...search, ...next })
  return (
    <div className="min-w-0">
      <div className="mb-4 flex flex-wrap items-end justify-between gap-x-4 border-b border-border">
        <div role="tablist" aria-label="Document views" className="flex gap-4">
          {tabs.map((name, index) => (
            <button
              key={name}
              type="button"
              id={`${id}-${name}`}
              role="tab"
              aria-selected={tab === name}
              aria-controls={`${id}-panel`}
              tabIndex={tab === name ? 0 : -1}
              className={`-mb-px border-b-2 pb-2.5 text-[13px] font-medium capitalize transition-colors focus-visible:outline-2 focus-visible:outline-primary ${tab === name ? 'border-primary text-foreground' : 'border-transparent text-muted hover:text-foreground'}`}
              onClick={() => select({ tab: name })}
              onKeyDown={(event) => {
                let next = index
                if (event.key === 'ArrowRight') next = (index + 1) % tabs.length
                else if (event.key === 'ArrowLeft') next = (index + tabs.length - 1) % tabs.length
                else if (event.key === 'Home') next = 0
                else if (event.key === 'End') next = tabs.length - 1
                else return
                event.preventDefault()
                const name = tabs[next]
                void Promise.resolve(onChange({ ...search, tab: name })).then(() =>
                  window.document.querySelector<HTMLButtonElement>(`[role=tab][aria-selected=true]`)?.focus(),
                )
              }}
            >
              {name}
            </button>
          ))}
        </div>
        {actions && <div className="-mr-2.5 flex flex-wrap items-center gap-1 pb-1.5">{actions}</div>}
      </div>
      <div className={tab === 'changes' ? 'sticky top-0 z-10 max-h-[40vh] overflow-y-auto bg-card' : undefined}>
        {attention}
      </div>
      <div role="tabpanel" id={`${id}-panel`} aria-labelledby={`${id}-${tab}`} className="mt-6 min-w-0">
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
                <div className="grid min-w-0 grid-cols-2 gap-3 rounded-lg border border-border bg-surface p-3 sm:p-4">
                  <label className="min-w-0 text-xs text-muted" htmlFor={`${id}-base`}>
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
                  <label className="min-w-0 text-xs text-muted" htmlFor={`${id}-target`}>
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
                  <ol className="divide-y divide-border rounded-lg border border-border bg-card">
                    {[...versions]
                      .sort((a, b) => b.version - a.version)
                      .map((v) => (
                        <li key={v.version} className="px-4 py-3 text-sm">
                          <h4 className="font-semibold">
                            v{v.version} · {versionState(v)}
                          </h4>
                          <p className="mt-1 break-words text-xs text-muted">
                            {v.origin.replaceAll('_', ' ')}
                            {v.origin_task_id && ` · Task ${v.origin_task_id}`}
                            {v.origin_session_id && ` · Session ${v.origin_session_id}`} · {dateTimeLabel(v.created_at)}
                          </p>
                          {v.confirmed_by && (
                            <p className="mt-1 text-xs text-muted">
                              Confirmed by {actorName(v.confirmed_by, members)}
                              {v.confirmed_at && ` on ${dateTimeLabel(v.confirmed_at)}`}
                            </p>
                          )}
                          {(v.dismissed_by || v.retired_by) && (
                            <p className="mt-1 text-xs text-muted">
                              Dismissed by {actorName(v.dismissed_by ?? v.retired_by ?? '', members)}
                              {(v.dismissed_at || v.retired_at) &&
                                ` on ${dateTimeLabel(v.dismissed_at ?? v.retired_at ?? '')}`}
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
