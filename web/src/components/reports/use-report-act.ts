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
  // An unknown answer whose fresh read failed: the card shows a report that
  // can be out of date, so every act waits for a read that succeeds.
  | { phase: 'stale'; act: ReportActName; target: string; message: string; reading: boolean }

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
    return 'Conveyor does not know whether it recorded this. Conveyor read the report again. Look at it before you try again.'
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

const staleMessage =
  'Conveyor does not know whether it recorded this, and the report did not load again. Read the report again before you act.'
const unknownAnswer = new FeedbackReportActError(0, '', '', '', 'unknown')

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

  // Reads every shown report read again and says whether each one answered
  // with a report. Only a success that the query cache reports during this
  // read counts: a read that failed, or that waits offline in TanStack
  // Query's paused queue, did not answer, so the card stays blocked.
  const readAgain = useCallback(async () => {
    const answered = new Set<string>()
    const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
      const [scope, scopeWorkspace] = event.query.queryKey
      if (
        event.type === 'updated' &&
        event.action.type === 'success' &&
        !event.action.manual &&
        scope === 'feedback-reports' &&
        scopeWorkspace === workspace
      )
        answered.add(event.query.queryHash)
    })
    try {
      await refresh().catch(() => undefined)
    } finally {
      unsubscribe()
    }
    const reads = queryClient.getQueryCache().findAll({ queryKey: feedbackReportsQueryKey(workspace), type: 'active' })
    return reads.every((query) => answered.has(query.queryHash))
  }, [queryClient, refresh, workspace])

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
          if (!(await readAgain())) {
            setState({ phase: 'stale', act, target, message: staleMessage, reading: false })
            return false
          }
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

  // After a failed fresh read, the person reads the report again before any
  // act. Only a read that succeeds lets the card act again.
  const reread = useCallback(async () => {
    if (busy.current || state.phase !== 'stale') return
    busy.current = true
    const { act, target } = state
    setState({ ...state, reading: true })
    try {
      if (await readAgain())
        setState({ phase: 'failed', act, target, field: '', message: reportActMessage(unknownAnswer) })
      else setState({ phase: 'stale', act, target, message: staleMessage, reading: false })
    } finally {
      busy.current = false
    }
  }, [readAgain, state])

  const clear = useCallback(() => setState((current) => (current.phase === 'stale' ? current : { phase: 'idle' })), [])
  const pending = state.phase === 'sending' || state.phase === 'checking' || state.phase === 'stale'
  return { state, run, clear, reread, pending }
}
