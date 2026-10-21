import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowDown, ArrowUp, X } from 'lucide-react'
import { useEffect, useMemo, useRef } from 'react'
import { fetchRequirement, fetchSystemDesign } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { NextActionContextProposal, RequirementVersion, RequirementView, SystemDesignView } from '../../lib/types'
import { absoluteTime, cn, relativeTime } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { reviewBlocks } from '../documents/document-review-model'
import { Button } from '../ui/button'
import { MarkdownProse } from '../ui/markdown-prose'
import { Sheet } from '../ui/sheet'
import { Skeleton } from '../ui/skeleton'
import { citedSections } from './citation-matcher'
import type { ContextProposalSelection } from './context-proposal-rows'

// The words the panel prints for a proposal's stored source (NA-6).
const sourceWords: Record<string, string> = { triage: 'triage', planning: 'planning', operator: 'operator' }

const headingLine = /^\s{0,3}#{1,6}\s/

// The document preview panel of a context_proposal (feature-operator-next-action
// NA-6; AC-2.3). One Sheet stays mounted while the operator moves between
// proposed documents, so closing it returns focus to the control that opened
// it. The checkbox mirrors the checklist row and never posts on its own.
export function ContextDocumentPreview({
  proposals,
  index,
  onIndex,
  onClose,
  selection,
  disabled,
}: {
  proposals: NextActionContextProposal[]
  index: number
  onIndex: (index: number) => void
  onClose: () => void
  selection: ContextProposalSelection
  disabled: boolean
}) {
  const proposal = proposals[index]
  const indexRef = useRef(index)
  indexRef.current = index
  const total = proposals.length
  const move = useRef<(step: 1 | -1) => void>(() => {})
  move.current = (step) => {
    const next = indexRef.current + step
    if (next >= 0 && next < total) onIndex(next)
  }
  // ArrowUp and ArrowDown move between proposed documents while the panel is
  // open; a text field keeps its own keys.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.altKey) return
      if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return
      const target = event.target
      if (target instanceof HTMLElement && target.closest('textarea, select, input:not([type="checkbox"])')) return
      event.preventDefault()
      move.current(event.key === 'ArrowDown' ? 1 : -1)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])
  if (!proposal) return null
  const title = proposal.target_title.trim() || proposal.tier_label
  return (
    <Sheet onClose={onClose} label={`Preview: ${title}`} width="w-full md:w-[46rem]">
      <div data-context-preview={`${proposal.kind}:${proposal.target_id}`} className="flex min-h-0 flex-1 flex-col">
        <PreviewDocument key={`${proposal.kind}:${proposal.target_id}`} proposal={proposal} onClose={onClose} />
        <div className="flex shrink-0 flex-wrap items-center gap-3 border-t border-border bg-card px-4 py-2.5 text-sm">
          <label className="inline-flex cursor-pointer items-center gap-2">
            <input
              type="checkbox"
              className="size-4 cursor-pointer accent-primary disabled:cursor-default"
              checked={selection.checked(proposal)}
              disabled={disabled}
              data-preview-attach
              onChange={(event) => selection.toggle(proposal, event.target.checked)}
            />
            Attach to this task
          </label>
          <span data-preview-position className="ml-auto text-xs text-muted">
            {index + 1} of {total} proposed
          </span>
          <Button
            size="icon"
            variant="ghost"
            aria-label="Previous document"
            disabled={index === 0}
            onClick={() => move.current(-1)}
          >
            <ArrowUp />
          </Button>
          <Button
            size="icon"
            variant="ghost"
            aria-label="Next document"
            disabled={index === total - 1}
            onClick={() => move.current(1)}
          >
            <ArrowDown />
          </Button>
        </div>
      </div>
    </Sheet>
  )
}

// The confirmed version of one proposed document. The document pages' own
// query keys and reads, so the panel shares their cache; the key carries the
// workspace and the query's signal aborts on unmount. The component mounts per
// document and per opening, and content shows only from a read that succeeded
// since that mount: cached content never stands in for a pending or failed
// read, and a proposed version never renders (NA-6; AC-2.3).
function PreviewDocument({ proposal, onClose }: { proposal: NextActionContextProposal; onClose: () => void }) {
  const { workspace } = useWorkspaceSelection()
  const requirement = proposal.kind === 'requirement'
  const { data, error, refetch, isFetching, isFetchedAfterMount } = useQuery<RequirementView | SystemDesignView>({
    queryKey: [requirement ? 'requirement' : 'system-design', workspace, proposal.target_id],
    queryFn: ({ signal }) =>
      requirement
        ? fetchRequirement(proposal.target_id, workspace, signal)
        : fetchSystemDesign(proposal.target_id, workspace, signal),
    enabled: Boolean(workspace),
    refetchOnMount: 'always',
  })
  const current = data && error == null && isFetchedAfterMount ? data : undefined
  const loading = error == null && !current
  const document = current ? confirmedDocument(current) : undefined
  const title = document?.title || proposal.target_title.trim() || proposal.tier_label
  const version = document?.current
  const sections = useMemo(() => (version ? previewSections(version) : []), [version])
  const cited = useMemo(
    () =>
      citedSections(
        sections.map((section) => ({ id: section.id, title: section.title, heading: section.heading })),
        proposal.justification,
      ),
    [sections, proposal.justification],
  )
  const dimming = cited.size > 0
  const content = useRef<HTMLDivElement>(null)
  const page =
    proposal.kind === 'requirement'
      ? ({ to: '/requirements', search: { requirement: proposal.target_id } } as const)
      : ({ to: '/system-design', search: { document: proposal.target_id } } as const)
  return (
    <>
      <header className="shrink-0 space-y-2 border-b border-border px-4 pb-3 pt-3.5">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="text-[11px] font-medium uppercase tracking-[0.06em] text-muted">{proposal.tier_label}</span>
          {version && (
            <span data-preview-version className="text-xs text-faint">
              v{version.version}
              {version.confirmed_at && (
                <>
                  {' · confirmed '}
                  <time dateTime={version.confirmed_at} title={absoluteTime(version.confirmed_at)}>
                    {relativeTime(version.confirmed_at)}
                  </time>
                </>
              )}
            </span>
          )}
          <span className="ml-auto inline-flex items-center gap-2">
            <Link {...page} className="text-xs font-medium text-primary hover:underline">
              Open full page ↗
            </Link>
            <Button size="icon" variant="ghost" aria-label="Close preview" onClick={onClose}>
              <X />
            </Button>
          </span>
        </div>
        <h2 className="break-words text-base font-semibold leading-6">{title}</h2>
        {proposal.justification && (
          <div data-preview-why className="text-sm leading-6 text-muted">
            <p className="text-[11px] font-medium text-faint">
              Why {sourceWords[proposal.source] ?? proposal.source} suggests it
            </p>
            <p className="break-words">{proposal.justification}</p>
          </div>
        )}
      </header>
      <div className="flex min-h-0 flex-1" aria-busy={loading || isFetching}>
        {loading && (
          <div className="flex-1 space-y-3 p-4">
            <Skeleton className="h-6 w-1/3" />
            <Skeleton className="h-24" />
          </div>
        )}
        {error != null && (
          <div className="flex flex-1 flex-wrap items-start gap-2 p-4 text-sm">
            <span className="text-failure">{errorMessage(error, 'Could not load this document.')}</span>
            <Button size="sm" variant="secondary" disabled={isFetching} onClick={() => void refetch()}>
              Retry
            </Button>
          </div>
        )}
        {document && !version && (
          <p className="flex-1 p-4 text-sm text-muted">This document has no confirmed version yet.</p>
        )}
        {version && (
          <>
            <nav
              aria-label="Outline"
              className="hidden w-48 shrink-0 overflow-y-auto border-r border-border px-2 py-3 sm:block"
            >
              <p className="px-2 pb-1.5 text-[11px] font-medium uppercase tracking-[0.06em] text-faint">Outline</p>
              {sections.map((section) => (
                <button
                  key={section.id}
                  type="button"
                  data-outline-entry={section.id}
                  data-cited={cited.has(section.id) ? 'true' : 'false'}
                  className={cn(
                    'block w-full truncate rounded px-2 py-1 text-left text-[12.5px] hover:bg-surface hover:text-foreground',
                    section.statement && section.title.startsWith('AC-') && 'pl-5',
                    !dimming || cited.has(section.id) ? 'text-foreground' : 'text-faint',
                  )}
                  title={section.title}
                  onClick={() =>
                    content.current
                      ?.querySelector(`[data-preview-section="${CSS.escape(section.id)}"]`)
                      ?.scrollIntoView({ block: 'start' })
                  }
                >
                  {section.title}
                </button>
              ))}
            </nav>
            <div ref={content} className="min-w-0 flex-1 overflow-y-auto px-4 pb-10 pt-3">
              {dimming && (
                <p data-preview-cited-count className="mb-2 text-xs text-muted">
                  {cited.size} of {sections.length} sections cited
                </p>
              )}
              {sections.map((section) => (
                <section
                  key={section.id}
                  // biome-ignore lint/a11y/noNoninteractiveTabindex: a dimmed section must take keyboard focus to return to full strength.
                  tabIndex={0}
                  aria-label={section.title}
                  data-preview-section={section.id}
                  data-cited={cited.has(section.id) ? 'true' : 'false'}
                  className={cn(
                    'scroll-mt-2 rounded py-2 outline-none transition-opacity focus-visible:outline-2 focus-visible:outline-primary',
                    dimming &&
                      !cited.has(section.id) &&
                      'opacity-40 hover:opacity-100 focus:opacity-100 focus-within:opacity-100',
                  )}
                >
                  {section.label && <h3 className="mb-1 text-sm font-semibold">{section.label}</h3>}
                  <MarkdownProse className="text-sm">{section.content}</MarkdownProse>
                </section>
              ))}
            </div>
          </>
        )}
      </div>
    </>
  )
}

interface PreviewSection {
  id: string
  title: string
  // A heading the author wrote, which the matcher may match by its text.
  heading: boolean
  // A REQ-n or AC-n.m statement, matched by its ID.
  statement: boolean
  // A label printed above content that carries no heading line of its own.
  label?: string
  content: string
}

// The confirmed version's sections through the document surfaces' own model:
// Markdown headings for a System Design document; prose headings plus one
// section per REQ-n and AC-n.m for a requirement (NA-6).
function previewSections(version: ConfirmedVersion): PreviewSection[] {
  return reviewBlocks({ content: version.content, statements: version.statements }).map((block) => {
    const statement = !block.id.startsWith('heading:')
    const ownHeading = headingLine.test(block.content.split('\n', 1)[0] ?? '')
    return {
      id: block.id,
      title: block.title,
      heading: !statement && ownHeading,
      statement,
      label: statement ? block.title : undefined,
      content: block.content,
    }
  })
}

type ConfirmedVersion = {
  version: number
  content: string
  confirmed_at?: string
  statements?: RequirementVersion['statements']
}

// The title and the current confirmed version of either document read. A read
// of an unexpected shape yields no title and no version rather than failing
// the panel.
function confirmedDocument(view: RequirementView | SystemDesignView): {
  title?: string
  current?: ConfirmedVersion
} {
  if ('requirement' in view) return { title: view.requirement?.title, current: view.current_version }
  return { title: view.document?.title, current: view.current_version }
}
