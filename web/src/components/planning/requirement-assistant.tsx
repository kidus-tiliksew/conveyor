import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { ArrowUpRight, GitBranch, MessageCircleQuestion, PenLine, Sparkles } from 'lucide-react'
import { Button } from '../ui/button'
import { fetchPlanningSession, fetchReferenceDocuments, fetchReferenceDocumentVersions } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { PlanningSession, PlanningSessionGoal, RequirementDerivation, RequirementView } from '../../lib/types'
import { PlanningChat } from './planning-chat'
import { Dialog } from '../ui/dialog'
import { Select } from '../ui/input'

export type GuidedAction = {
  id: 'draft' | 'revise' | 'promote' | 'qa' | 'plan'
  label: string
  hint: string
  goal: PlanningSessionGoal
  icon: typeof PenLine
  /** Draft is the blank-page flow, so it carries no requirement context. */
  contextual: boolean
}

// The four guided actions replace the blank prompt (spec §21.57 change 1).
// Each declares the goal its session finalizes toward. Q&A is goal `open`,
// which carries no finalize expectation — it does not forbid one, so the hint
// promises exploration rather than immunity.
export const guidedActions: GuidedAction[] = [
  {
    id: 'draft',
    label: 'Draft',
    hint: 'Start a new requirement document',
    goal: 'requirement',
    icon: Sparkles,
    contextual: false,
  },
  {
    id: 'revise',
    label: 'Revise',
    hint: 'Propose the next version of this document',
    goal: 'requirement',
    icon: PenLine,
    contextual: true,
  },
  {
    id: 'promote',
    label: 'Promote overview',
    hint: 'Turn an enforceable overview passage into a proposed REQ or AC with provenance',
    goal: 'requirement',
    icon: ArrowUpRight,
    contextual: false,
  },
  {
    id: 'qa',
    label: 'Q&A',
    hint: 'Explore this requirement with no artifact in mind',
    goal: 'open',
    icon: MessageCircleQuestion,
    contextual: true,
  },
  {
    id: 'plan',
    label: 'Plan work',
    hint: 'Open a blueprint at the spec gate, serving this requirement',
    goal: 'blueprint',
    icon: GitBranch,
    contextual: true,
  },
]

export const draftAction = guidedActions.find((action) => action.id === 'draft') as GuidedAction

/**
 * The planning assistant docked beside the document canvas. It is the only
 * authoring path for requirement content: every revision arrives as a proposed
 * version the operator confirms on the canvas (spec §21.57 change 2).
 */
export function RequirementAssistant({
  selected,
  sessionId,
  token,
  workspace,
  onStart,
  starting,
  startError,
  onFinalized,
}: {
  selected?: RequirementView
  sessionId: string
  token: string
  workspace: string
  onStart: (action: GuidedAction, promotion?: RequirementDerivation) => void
  starting: boolean
  startError: unknown
  onFinalized: (session: PlanningSession) => void
}) {
  const { data: session, error: sessionError } = useQuery({
    queryKey: ['planning-session', workspace, sessionId],
    queryFn: () => fetchPlanningSession(sessionId),
    enabled: Boolean(sessionId),
  })
  const [promotionOpen, setPromotionOpen] = useState(false)
  const actions = guidedActions.filter((action) => !action.contextual || Boolean(selected))

  return (
    <aside
      aria-label="Planning assistant"
      className="flex w-[380px] shrink-0 flex-col border-l border-border bg-surface/40"
    >
      <div className="shrink-0 border-b border-border px-4 py-3">
        <p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Assistant</p>
        <p className="mt-0.5 truncate text-xs text-muted">
          {session
            ? session.title || 'Untitled planning session'
            : selected
              ? selected.requirement.title
              : 'No document open'}
        </p>
      </div>

      <div className="flex shrink-0 flex-wrap gap-1.5 border-b border-border px-4 py-3">
        {actions.map((action) => (
          <Button
            key={action.id}
            variant="secondary"
            size="sm"
            title={action.hint}
            disabled={!token || !workspace || starting}
            onClick={() => (action.id === 'promote' ? setPromotionOpen(true) : onStart(action))}
          >
            <action.icon /> {action.label}
          </Button>
        ))}
      </div>
      {startError != null && (
        <p className="shrink-0 border-b border-border px-4 py-2 text-xs text-failure">
          {errorMessage(startError, 'Could not start this planning session.')}
        </p>
      )}

      <div className="flex min-h-0 flex-1 flex-col">
        {/* A session id that no longer resolves — a stale bookmark, or one
            belonging to the workspace we just switched away from — must not
            strand the sidebar: say so, then offer the guided actions. */}
        {sessionError && (
          <p className="shrink-0 px-4 py-3 text-xs text-muted">
            {errorMessage(sessionError, 'That planning session is not available in this workspace.')}
          </p>
        )}
        {session && !sessionError ? (
          <PlanningChat
            key={`${workspace}:${session.id}`}
            summary={session}
            token={token}
            workspace={workspace}
            variant="sidebar"
            onFinalized={onFinalized}
          />
        ) : (
          <div className="px-5 py-8 text-center">
            <Sparkles className="mx-auto size-6 text-primary" />
            <h3 className="mt-3 text-sm font-semibold">The assistant is the editor</h3>
            <p className="mx-auto mt-1.5 max-w-xs text-xs leading-5 text-muted">
              {selected
                ? 'Revise this document, ask about it, or plan the work that delivers it. Every revision arrives as a version you confirm.'
                : 'Draft a requirement in conversation. Its first version lands here for you to confirm.'}
            </p>
          </div>
        )}
      </div>
      {promotionOpen && (
        <PromotionDialog
          selected={selected}
          starting={starting}
          onClose={() => setPromotionOpen(false)}
          onStart={(promotion) => {
            onStart(guidedActions.find((action) => action.id === 'promote') as GuidedAction, promotion)
            setPromotionOpen(false)
          }}
        />
      )}
    </aside>
  )
}

function headingAnchor(value: string) {
  return value
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, '-')
    .replace(/^-|-$/g, '')
}

export function markdownHeadings(content: string) {
  const headings: Array<{ label: string; anchor: string }> = []
  const seen = new Map<string, number>()
  let fenceCharacter = ''
  let fenceLength = 0
  for (const line of content.split('\n')) {
    const trimmedLeft = line.replace(/^ +/, '')
    const indent = line.length - trimmedLeft.length
    const fence = indent <= 3 ? trimmedLeft.match(/^(`+|~+)/)?.[1] : undefined
    if (fence && fence.length >= 3) {
      if (!fenceCharacter) {
        fenceCharacter = fence[0]
        fenceLength = fence.length
        continue
      }
      if (fence[0] === fenceCharacter && fence.length >= fenceLength && trimmedLeft.slice(fence.length).trim() === '') {
        fenceCharacter = ''
        fenceLength = 0
        continue
      }
    }
    if (fenceCharacter) continue
    const match = line.match(/^ {0,3}#{1,6}[ \t]+(.+?)\s*$/)
    if (!match) continue
    const label = match[1].trim()
    const base = headingAnchor(label)
    if (!base) continue
    const ordinal = seen.get(base) ?? 0
    seen.set(base, ordinal + 1)
    headings.push({ label, anchor: `#${base}${ordinal > 0 ? `-${ordinal}` : ''}` })
  }
  return headings
}

function PromotionDialog({
  selected,
  starting,
  onClose,
  onStart,
}: {
  selected?: RequirementView
  starting: boolean
  onClose: () => void
  onStart: (promotion: RequirementDerivation) => void
}) {
  const { data: documents = [], error: documentsError } = useQuery({
    queryKey: ['reference-documents', 'promotion'],
    queryFn: fetchReferenceDocuments,
  })
  const [documentID, setDocumentID] = useState('')
  const chosenDocument = documents.find((document) => document.id === documentID) ?? documents[0]
  const { data: versions = [], error: versionsError } = useQuery({
    queryKey: ['reference-document-versions', chosenDocument?.id, 'promotion'],
    queryFn: () => fetchReferenceDocumentVersions(chosenDocument?.id ?? ''),
    enabled: Boolean(chosenDocument),
  })
  const [versionNumber, setVersionNumber] = useState(0)
  const chosenVersion = versions.find((version) => version.version === versionNumber) ?? versions.at(-1)
  const headings = useMemo(() => markdownHeadings(chosenVersion?.content ?? ''), [chosenVersion])
  const [sectionAnchor, setSectionAnchor] = useState('')
  const targetIDs = useMemo(
    () =>
      selected?.current_version?.statements.flatMap((statement) => [
        statement.id,
        ...(statement.acceptance_criteria ?? []).map((criterion) => criterion.id),
      ]) ?? ['REQ-1'],
    [selected],
  )
  const [targetID, setTargetID] = useState('')
  const effectiveAnchor = headings.some((heading) => heading.anchor === sectionAnchor)
    ? sectionAnchor
    : (headings[0]?.anchor ?? '')
  const effectiveTarget = targetIDs.includes(targetID) ? targetID : (targetIDs[0] ?? '')

  return (
    <Dialog label="Promote product overview" onClose={onClose}>
      <div className="border-b border-border px-5 py-4">
        <h2 className="text-sm font-semibold">Promote product overview</h2>
        <p className="mt-1 text-xs leading-5 text-muted">
          Choose an immutable source passage and the REQ or AC it should propose. The source becomes normative only
          after confirmation.
        </p>
      </div>
      <div className="space-y-4 px-5 py-4">
        <label className="block text-xs font-medium" htmlFor="promotion-document">
          Document
          <Select
            id="promotion-document"
            value={chosenDocument?.id ?? ''}
            onChange={(event) => {
              setDocumentID(event.target.value)
              setVersionNumber(0)
              setSectionAnchor('')
            }}
          >
            {documents.map((document) => (
              <option key={document.id} value={document.id}>
                {document.name}
              </option>
            ))}
          </Select>
        </label>
        <label className="block text-xs font-medium" htmlFor="promotion-version">
          Immutable version
          <Select
            id="promotion-version"
            value={chosenVersion?.version ?? ''}
            onChange={(event) => {
              setVersionNumber(Number(event.target.value))
              setSectionAnchor('')
            }}
          >
            {versions.map((version) => (
              <option key={version.version} value={version.version}>
                v{version.version} · {version.filename}
              </option>
            ))}
          </Select>
        </label>
        <label className="block text-xs font-medium" htmlFor="promotion-section">
          Section
          <Select
            id="promotion-section"
            value={effectiveAnchor}
            onChange={(event) => setSectionAnchor(event.target.value)}
          >
            {headings.map((heading) => (
              <option key={heading.anchor} value={heading.anchor}>
                {heading.label}
              </option>
            ))}
          </Select>
        </label>
        <label className="block text-xs font-medium" htmlFor="promotion-target">
          Promotion target
          <Select id="promotion-target" value={effectiveTarget} onChange={(event) => setTargetID(event.target.value)}>
            {targetIDs.map((id) => (
              <option key={id} value={id}>
                {id}
              </option>
            ))}
          </Select>
          <span className="mt-1 block font-normal text-muted">
            {selected
              ? `Propose a pending revision of ${selected.requirement.title}.`
              : 'Start a new requirement with REQ-1.'}
          </span>
        </label>
        {(documentsError || versionsError) && (
          <p className="text-xs text-failure">
            {errorMessage(documentsError ?? versionsError, 'Could not load promotion sources.')}
          </p>
        )}
      </div>
      <div className="flex justify-end gap-2 border-t border-border px-5 py-4">
        <Button variant="secondary" onClick={onClose}>
          Cancel
        </Button>
        <Button
          disabled={starting || !chosenDocument || !chosenVersion || !effectiveAnchor || !effectiveTarget}
          onClick={() =>
            chosenDocument &&
            chosenVersion &&
            onStart({
              document_id: chosenDocument.id,
              version: chosenVersion.version,
              section_anchor: effectiveAnchor,
              target_id: effectiveTarget,
            })
          }
        >
          Start promotion
        </Button>
      </div>
    </Dialog>
  )
}
