import { useQueryClient } from '@tanstack/react-query'
import { useCallback, useRef, useState } from 'react'
import { FeedbackReportActError } from '../../lib/api'
import type { FeedbackReport } from '../../lib/types'
import { feedbackReportsQueryKey } from './query-keys'

/** The acts a person records on a report (component-feedback-reports invariant 17). */
export type ReportActName = 'confirm' | 'remove' | 'link' | 'close' | 'reopen'

export type ReportActState =
  | { phase: 'idle' }
  | { phase: 'sending'; act: ReportActName; target: string }
  | { phase: 'checking'; act: ReportActName; target: string }
  | { phase: 'failed'; act: ReportActName; target: string; message: string; field: string }

const fieldNames: Record<string, string> = {
  reason: 'the reason',
  task_id: 'the task',
  crate_id: 'the crate',
  link_id: 'the link',
  cause_task_ids: 'the tasks you ticked',
  body: 'the request',
}

/**
 * What the page tells a person after an act fails. A refusal records
 * nothing (docs/feedback-reports.md, Acts), so typed input stays in place
 * and the person can send it again. A 500 or a lost answer is told apart:
 * the act may be recorded.
 */
export function reportActMessage(error: unknown) {
  if (!(error instanceof FeedbackReportActError)) return 'Something went wrong. Nothing changed on this page.'
  if (error.outcome === 'retryable') return 'Conveyor recorded nothing. Try again.'
  if (error.outcome === 'unknown')
    return 'Conveyor does not know whether it recorded this. Look at the report again before you try again.'
  switch (error.code) {
    case 'report_closed':
      return 'This report is closed now. Reopen it first.'
    case 'report_not_found':
      return 'This report is not in the workspace.'
    case 'link_not_found':
      return 'This link is not on the report any more.'
    case 'task_not_found':
      return 'That task is not in this workspace.'
    case 'crate_not_found':
      return 'That crate is not in this workspace.'
    case 'task_not_merged':
      return 'That task is not merged. You can link only a merged task.'
    case 'workspace_not_found':
      return 'You cannot record acts on reports in this workspace.'
    case 'invalid_report_act':
      return `Conveyor refused the request: ${fieldNames[error.field] ?? 'a field'} ${error.message}.`
  }
  if (error.status === 413) return 'The request is too large. Make it shorter.'
  return error.message || 'Conveyor refused the request.'
}

/**
 * One act runner for a report card. It sends at most one act at a time: a
 * second press while an act is on its way sends nothing. After an answer it
 * refreshes the report reads and the crate reads, whose open-report counts can
 * change (invariant 23).
 *
 * - 200: the act is recorded or changed nothing. The reads refresh.
 * - 503 `retryable`: nothing was recorded. The person may send it again.
 * - 500 or no answer: the act may be recorded. The page reads the report
 *   again, and only after that read settles can the person act again. It
 *   never sends the act again by itself.
 * - A refusal records nothing. A refusal that says the report or link changed
 *   refreshes the reads too.
 */
export function useReportAct(workspace: string) {
  const queryClient = useQueryClient()
  const [state, setState] = useState<ReportActState>({ phase: 'idle' })
  const busy = useRef(false)

  const refresh = useCallback(async () => {
    void queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
    await queryClient.invalidateQueries({ queryKey: feedbackReportsQueryKey(workspace) })
  }, [queryClient, workspace])

  const run = useCallback(
    async (act: ReportActName, target: string, send: () => Promise<FeedbackReport>) => {
      if (busy.current) return false
      busy.current = true
      setState({ phase: 'sending', act, target })
      try {
        await send()
        setState({ phase: 'idle' })
        void refresh()
        return true
      } catch (error) {
        const message = reportActMessage(error)
        const field = error instanceof FeedbackReportActError ? error.field : ''
        if (error instanceof FeedbackReportActError && error.outcome === 'unknown') {
          setState({ phase: 'checking', act, target })
          await refresh().catch(() => undefined)
        } else if (
          error instanceof FeedbackReportActError &&
          ['report_closed', 'report_not_found', 'link_not_found'].includes(error.code)
        ) {
          void refresh()
        }
        setState({ phase: 'failed', act, target, message, field })
        return false
      } finally {
        busy.current = false
      }
    },
    [refresh],
  )

  const clear = useCallback(() => setState({ phase: 'idle' }), [])
  const pending = state.phase === 'sending' || state.phase === 'checking'
  return { state, run, clear, pending }
}
