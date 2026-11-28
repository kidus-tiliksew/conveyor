import { type QueryClient, useQueryClient } from '@tanstack/react-query'
import { useCallback, useSyncExternalStore } from 'react'
import { FeedbackReportActError } from '../../lib/api'
import type { FeedbackReport } from '../../lib/types'
import { feedbackReportsQueryKey } from './query-keys'

/** The acts a person records on a report (component-feedback-reports invariant 17). */
export type ReportActName = 'confirm' | 'remove' | 'link' | 'close' | 'reopen' | 'fix'

export type ReportActState =
  | { phase: 'idle' }
  | { phase: 'sending'; act: ReportActName; target: string }
  | { phase: 'checking'; act: ReportActName; target: string }
  // `afterUnknown` marks the note after an unknown answer whose fresh read
  // answered. The card shows it, not the row that started the act.
  | { phase: 'failed'; act: ReportActName; target: string; message: string; field: string; afterUnknown?: boolean }
  // An unknown answer whose fresh read failed: every copy of the report can
  // be out of date, so every act waits for a read that succeeds.
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

export const staleMessage =
  'Conveyor does not know whether it recorded this, and the report did not load again. Read the report again before you act.'
const unknownAnswer = new FeedbackReportActError(0, '', '', '', 'unknown')

// The act state of each report, by workspace and report ID. It lives outside
// every card, so all copies of one report on the page share it, and it stays
// when a refresh removes the link row or the card that started the act.
const idle: ReportActState = { phase: 'idle' }
const actStates = new Map<string, ReportActState>()
const actListeners = new Set<() => void>()

export function reportActKey(workspace: string, reportId: string) {
  return `${workspace}\u0000${reportId}`
}

export function readReportActState(key: string) {
  return actStates.get(key) ?? idle
}

export function writeReportActState(key: string, next: ReportActState) {
  if (next.phase === 'idle') actStates.delete(key)
  else actStates.set(key, next)
  for (const listener of actListeners) listener()
}

function subscribeActStates(listener: () => void) {
  actListeners.add(listener)
  return () => actListeners.delete(listener)
}

/** True while an act of the report is on its way or its outcome is unknown. */
export function reportActBlocked(state: ReportActState) {
  return state.phase === 'sending' || state.phase === 'checking' || state.phase === 'stale'
}

// Resolves once no report read of the workspace is fetching.
function noneFetching(queryClient: QueryClient, scope: readonly unknown[]) {
  return new Promise<void>((resolve) => {
    let unsubscribe = () => {}
    let done = false
    const check = () => {
      if (done || queryClient.isFetching({ queryKey: scope }) > 0) return
      done = true
      unsubscribe()
      resolve()
    }
    unsubscribe = queryClient.getQueryCache().subscribe(check)
    check()
  })
}

// Resolves once no report read of the workspace is fetching, and none starts
// in the next task either. A refresh that cancels a read starts its
// replacement a few promise steps later, so a quiet moment between the two
// never ends the wait.
async function readsSettled(queryClient: QueryClient, scope: readonly unknown[]) {
  for (;;) {
    await noneFetching(queryClient, scope)
    await new Promise((resolve) => window.setTimeout(resolve, 0))
    if (queryClient.isFetching({ queryKey: scope }) === 0) return
  }
}

/**
 * Refreshes the report reads and the crate reads after a write. A report read
 * already on its way started before the write, so it is cancelled first and
 * read again: no read from before a write decides what the page shows after
 * it.
 */
export async function refreshAfterWrite(queryClient: QueryClient, workspace: string) {
  const scope = feedbackReportsQueryKey(workspace)
  await queryClient.cancelQueries({ queryKey: scope })
  void queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
  await queryClient.invalidateQueries({ queryKey: scope })
}

/**
 * Reads every shown report read of the workspace again and says whether each
 * one answered with a report. Every report read on its way is cancelled
 * first, because it can hold a report from before the act and TanStack Query
 * reuses such a read for a query that has no data yet. Only a success of a
 * read that started after that point counts, and the check waits until no
 * report read is fetching, so a read that replaces a cancelled one counts
 * too. A read that failed, or that waits offline in the paused queue, did not
 * answer. When no page shows a report read, nothing is left to read: the
 * next page that shows one reads it fresh.
 */
export async function readReportsAgain(queryClient: QueryClient, workspace: string) {
  const scope = feedbackReportsQueryKey(workspace)
  await queryClient.cancelQueries({ queryKey: scope })
  // A report read that no page shows now holds data from before the act. It
  // is dropped, so a page that shows it later reads it fresh first.
  queryClient.removeQueries({ queryKey: scope, type: 'inactive' })
  const started = new Set<string>()
  const answered = new Set<string>()
  const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
    const [name, eventWorkspace] = event.query.queryKey
    if (event.type !== 'updated' || name !== scope[0] || eventWorkspace !== workspace) return
    if (event.action.type === 'fetch') started.add(event.query.queryHash)
    else if (event.action.type === 'success' && !event.action.manual && started.has(event.query.queryHash))
      answered.add(event.query.queryHash)
  })
  try {
    void queryClient.invalidateQueries({ queryKey: ['crates', workspace] })
    await queryClient.invalidateQueries({ queryKey: scope }).catch(() => undefined)
    await readsSettled(queryClient, scope)
  } finally {
    unsubscribe()
  }
  // A page left while the read was on its way keeps its data from before
  // the act. Every report read that did not answer after the act and that no
  // page shows now is dropped, so a page that shows it later reads it fresh.
  queryClient.removeQueries({
    queryKey: scope,
    type: 'inactive',
    predicate: (query) => !answered.has(query.queryHash),
  })
  // With no report read on a page, no read answered after the act, so the
  // act stays unresolved until a page reads the report again.
  const reads = queryClient.getQueryCache().findAll({ queryKey: scope, type: 'active' })
  return reads.length > 0 && reads.every((query) => answered.has(query.queryHash))
}

/**
 * The act runner of one report. Its state is shared by every copy of the
 * report on the page (`reportActKey`): while an act of the report is on its
 * way, or its outcome is unknown, no copy can send another act.
 *
 * - 200: the act is recorded or changed nothing. The reads refresh.
 * - 503 `retryable`: nothing was recorded. The person may send it again.
 * - 500 or no answer: the act may be recorded. The page reads the reports
 *   again, and only a read that started after the answer and answered lets
 *   any copy act again. A failed read leaves the report blocked, with a
 *   control on every copy to read it again. The page never sends the act
 *   again by itself.
 * - A refusal records nothing. A refusal that says the report or link changed
 *   refreshes the reads too.
 */
export function useReportAct(workspace: string, reportId: string) {
  const queryClient = useQueryClient()
  const key = reportActKey(workspace, reportId)
  const state = useSyncExternalStore(
    subscribeActStates,
    () => readReportActState(key),
    () => idle,
  )

  const run = useCallback(
    async (act: ReportActName, target: string, send: () => Promise<FeedbackReport>) => {
      if (reportActBlocked(readReportActState(key))) return false
      writeReportActState(key, { phase: 'sending', act, target })
      try {
        await send()
        writeReportActState(key, idle)
        void refreshAfterWrite(queryClient, workspace)
        return true
      } catch (error) {
        const message = reportActMessage(error)
        const field = error instanceof FeedbackReportActError ? error.field : ''
        if (error instanceof FeedbackReportActError && error.outcome === 'unknown') {
          writeReportActState(key, { phase: 'checking', act, target })
          if (!(await readReportsAgain(queryClient, workspace))) {
            writeReportActState(key, { phase: 'stale', act, target, message: staleMessage, reading: false })
            return false
          }
          writeReportActState(key, { phase: 'failed', act, target, message, field, afterUnknown: true })
          return false
        }
        if (
          error instanceof FeedbackReportActError &&
          ['report_closed', 'report_not_found', 'link_not_found'].includes(error.code)
        )
          void refreshAfterWrite(queryClient, workspace)
        writeReportActState(key, { phase: 'failed', act, target, message, field })
        return false
      }
    },
    [key, queryClient, workspace],
  )

  // After a failed fresh read, any copy of the report reads it again. Only a
  // read that answers lets the copies act again.
  const reread = useCallback(async () => {
    const current = readReportActState(key)
    if (current.phase !== 'stale' || current.reading) return
    writeReportActState(key, { ...current, reading: true })
    if (await readReportsAgain(queryClient, workspace))
      writeReportActState(key, {
        phase: 'failed',
        act: current.act,
        target: current.target,
        field: '',
        message: reportActMessage(unknownAnswer),
        afterUnknown: true,
      })
    else writeReportActState(key, { ...current, reading: false })
  }, [key, queryClient, workspace])

  // Clears a note of the last act. A blocked state stays until a read answers.
  const clear = useCallback(() => {
    if (!reportActBlocked(readReportActState(key))) writeReportActState(key, idle)
  }, [key])
  return { state, run, clear, reread, pending: reportActBlocked(state) }
}
