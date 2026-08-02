import { useQuery } from '@tanstack/react-query'
import { GitBranch, MessageCircleQuestion, PenLine, Sparkles } from 'lucide-react'
import { Button } from '../ui/button'
import { fetchPlanningSession } from '../../lib/api'
import { errorMessage } from '../../lib/errors'
import type { PlanningSession, PlanningSessionGoal, RequirementView } from '../../lib/types'
import { PlanningChat } from './planning-chat'

export type GuidedAction = {
  id: 'draft' | 'revise' | 'qa' | 'plan'
  label: string
  hint: string
  goal: PlanningSessionGoal
  /** Draft is the blank-page flow, so it carries no requirement context. */
  contextual: boolean
}

// The four guided actions replace the blank prompt (spec §21.57 change 1).
// Each declares the goal its session finalizes toward.
export const guidedActions: GuidedAction[] = [
  { id: 'draft', label: 'Draft', hint: 'Start a new requirement document', goal: 'requirement', contextual: false },
  { id: 'revise', label: 'Revise', hint: 'Propose the next version of this document', goal: 'requirement', contextual: true },
  { id: 'qa', label: 'Q&A', hint: 'Ask about this requirement without finalizing', goal: 'open', contextual: true },
  { id: 'plan', label: 'Plan work', hint: 'Open a blueprint at the spec gate, serving this requirement', goal: 'blueprint', contextual: true },
]

export const draftAction = guidedActions[0]

const actionIcons: Record<GuidedAction['id'], typeof PenLine> = {
  draft: Sparkles, revise: PenLine, qa: MessageCircleQuestion, plan: GitBranch,
}

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
  onStart: (action: GuidedAction) => void
  starting: boolean
  startError: unknown
  onFinalized: (session: PlanningSession) => void
}) {
  const { data: session, error: sessionError } = useQuery({
    queryKey: ['planning-session', workspace, sessionId],
    queryFn: () => fetchPlanningSession(sessionId),
    enabled: Boolean(sessionId),
  })
  const actions = guidedActions.filter((action) => !action.contextual || Boolean(selected))

  return (
    <aside
      aria-label="Planning assistant"
      className="flex w-[380px] shrink-0 flex-col border-l border-border bg-surface/40"
    >
      <div className="shrink-0 border-b border-border px-4 py-3">
        <p className="text-[10px] font-semibold uppercase tracking-[0.12em] text-faint">Assistant</p>
        <p className="mt-0.5 truncate text-xs text-muted">
          {selected ? selected.requirement.title : 'No document open'}
        </p>
      </div>

      <div className="flex shrink-0 flex-wrap gap-1.5 border-b border-border px-4 py-3">
        {actions.map((action) => {
          const Icon = actionIcons[action.id]
          return (
            <Button
              key={action.id}
              variant="secondary"
              size="sm"
              title={action.hint}
              disabled={!token || starting}
              onClick={() => onStart(action)}
            >
              <Icon /> {action.label}
            </Button>
          )
        })}
      </div>
      {startError != null && (
        <p className="shrink-0 border-b border-border px-4 py-2 text-xs text-failure">
          {errorMessage(startError, 'Could not start this planning session.')}
        </p>
      )}

      <div className="flex min-h-0 flex-1 flex-col">
        {sessionError && (
          <p className="px-4 py-3 text-xs text-failure">{errorMessage(sessionError, 'Could not restore this planning session.')}</p>
        )}
        {session
          ? (
              <PlanningChat
                key={`${workspace}:${session.id}`}
                summary={session}
                token={token}
                workspace={workspace}
                variant="sidebar"
                onFinalized={onFinalized}
              />
            )
          : !sessionError && (
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
    </aside>
  )
}
