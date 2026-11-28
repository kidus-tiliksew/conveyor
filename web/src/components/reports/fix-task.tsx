import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { MessageSquareWarning } from 'lucide-react'
import { type Dispatch, type SetStateAction, useCallback, useEffect, useRef, useState } from 'react'
import { FeedbackReportActError, fetchFeedbackReport, setFeedbackReportFixTask } from '../../lib/api'
import { withQuerySignal } from '../task/use-task-detail'
import { Button } from '../ui/button'
import { feedbackReportQueryKey } from './query-keys'
import { Untrusted } from './report-card'
import { fixTaskDescription, MAX_CAUSE_TASKS } from './report-text'
import { refreshAfterWrite, reportActKey, staleMessage, writeReportActState } from './use-report-act'

/**
 * A fix task that intake created and the report has not recorded yet. It is
 * kept in the tab's session storage until the record request succeeds, so a
 * closed sheet or a reloaded page offers to record the same task again and
 * never offers to create a second one (component-feedback-reports
 * invariant 18).
 */
export interface CreatedFixTask {
  taskId: string
  taskTitle: string
  causeTaskIds: string[]
  // How the last record request for the task stands: on its way, answered
  // without a record, or answered with an unknown outcome.
  status?: 'sending' | 'not_recorded' | 'unknown'
}

/** The workspace and report a fix task is for, captured when the person presses Create. */
export interface FixTaskTarget {
  workspace: string
  reportId: string
}

function flowOf(target: FixTaskTarget) {
  return `${target.workspace}\u0000${target.reportId}`
}

function storageKey(workspace: string, reportId: string) {
  return `conveyor-fix-task:${workspace}:${reportId}`
}

export function readCreatedFixTask(workspace: string, reportId: string): CreatedFixTask | null {
  try {
    const raw = window.sessionStorage.getItem(storageKey(workspace, reportId))
    if (!raw) return null
    const value = JSON.parse(raw) as Partial<CreatedFixTask>
    if (typeof value.taskId !== 'string' || !value.taskId) return null
    const causes = Array.isArray(value.causeTaskIds)
      ? value.causeTaskIds.filter((id): id is string => typeof id === 'string')
      : []
    const status =
      value.status === 'sending' || value.status === 'not_recorded' || value.status === 'unknown'
        ? value.status
        : undefined
    return {
      taskId: value.taskId,
      taskTitle: typeof value.taskTitle === 'string' ? value.taskTitle : '',
      causeTaskIds: causes,
      ...(status ? { status } : {}),
    }
  } catch {
    return null
  }
}

function writeCreatedFixTask(workspace: string, reportId: string, value: CreatedFixTask | null) {
  try {
    if (value) window.sessionStorage.setItem(storageKey(workspace, reportId), JSON.stringify(value))
    else window.sessionStorage.removeItem(storageKey(workspace, reportId))
  } catch {
    // Without session storage the sheet still holds the task while it is open.
  }
}

type RecordState =
  | { phase: 'idle' }
  | { phase: 'sending' }
  | { phase: 'checking' }
  | { phase: 'failed'; message: string; canSend: boolean }

function recordFailureMessage(error: unknown) {
  if (error instanceof FeedbackReportActError) {
    if (error.outcome === 'retryable') return 'The report recorded nothing. Try again.'
    switch (error.code) {
      case 'report_closed':
        return 'The report is closed now, so it did not record the task. Reopen the report, then record the task again.'
      case 'report_not_found':
        return 'The report is not in the workspace, so it cannot record the task.'
      case 'task_not_found':
        return 'The report did not find the new task. Try again.'
      case 'task_not_merged':
        return 'A task you ticked is not merged, so the report recorded nothing.'
      case 'invalid_report_act':
        return `The report refused the request: ${error.field || 'a field'} ${error.message}.`
      case 'workspace_not_found':
        return 'You cannot record a fix task on reports in this workspace.'
    }
    if (error.status === 413) return 'The request is too large for the report.'
    return error.message || 'The report refused the request.'
  }
  return 'The report recorded nothing. Try again.'
}

/**
 * The fix-task flow of task creation (component-feedback-reports invariant
 * 18; req-production-feedback AC-7.4; req-security-boundaries AC-4.1):
 *
 * 1. The sheet reads the report and prefills the description once, with the
 *    report's values inside one block labelled as untrusted data. A refresh
 *    of the report never replaces what the person typed.
 * 2. Nothing is preselected: no repository, no crate, and no cause. The
 *    report's linked tasks are listed unticked as possible causes, at most
 *    50 of them ticked.
 * 3. Creation is the ordinary request. When it answers, the flow keeps the new
 *    task and the ticked causes, then records them with the fix-task route.
 * 4. A failed record request is stated with a control that sends only the
 *    record request again. A 500 or a lost answer leads to a fresh read of
 *    the report, never to a repeat by itself.
 */
export function useFixTaskFlow({
  workspace,
  reportId,
  setBody,
  onRecorded,
}: {
  workspace: string
  reportId: string | undefined
  setBody: Dispatch<SetStateAction<string>>
  onRecorded: (taskId: string) => void
}) {
  const queryClient = useQueryClient()
  const queryKey = feedbackReportQueryKey(workspace, reportId ?? '')
  const report = useQuery({
    queryKey,
    queryFn: (context) => withQuerySignal(context, (signal) => fetchFeedbackReport(workspace, reportId ?? '', signal)),
    enabled: Boolean(workspace && reportId),
    retry: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  const restoredEntry = () => (workspace && reportId ? readCreatedFixTask(workspace, reportId) : null)
  const [prefilledFor, setPrefilledFor] = useState<string | null>(null)
  const [causes, setCauses] = useState<string[]>([])
  const [created, setCreated] = useState<CreatedFixTask | null>(restoredEntry)
  // A task restored from an earlier visit waits for a fresh read of the
  // report before the page offers to record it.
  const [record, setRecord] = useState<RecordState>(() => (created ? { phase: 'checking' } : { phase: 'idle' }))
  const restorePending = useRef(created !== null)
  // Another report, or another workspace, starts the flow again with that
  // report's own saved task, so no state of one report reaches another.
  const identity = `${workspace}\u0000${reportId ?? ''}`
  const [seen, setSeen] = useState(identity)
  if (seen !== identity) {
    const restored = restoredEntry()
    setSeen(identity)
    setPrefilledFor(null)
    setCauses([])
    setCreated(restored)
    setRecord(restored ? { phase: 'checking' } : { phase: 'idle' })
    restorePending.current = restored !== null
  }
  // The last prefilled text: a description that still equals it was not
  // edited, so the prefill of another report replaces it.
  const lastPrefill = useRef('')
  // The flow that a request answers. A request that started for one report
  // changes nothing on the page once the address names another report: it
  // only clears its own report's saved task when the record succeeds.
  const shown = useRef(identity)
  shown.current = identity
  // The flows whose record request is on its way, each tracked on its own,
  // so no flow's request is ever sent twice, whatever flows come between.
  const sending = useRef(new Set<string>())
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const live = useCallback((flow: string) => mounted.current && shown.current === flow, [])

  // Prefill once per report, and only while it is open: a closed report
  // takes no fix task. Text that the person typed while the report loaded
  // stays, after the report block.
  const data = report.data
  useEffect(() => {
    if (!data || data.id !== reportId || prefilledFor === data.id || created) return
    if (data.state !== 'open') return
    const text = fixTaskDescription(data)
    const previous = lastPrefill.current
    lastPrefill.current = text
    setPrefilledFor(data.id)
    setBody((current) => (current.trim() === '' || current === previous ? text : `${text}\n${current}`))
  }, [data, reportId, prefilledFor, created, setBody])

  // A fresh read of one report that started after the moment it is asked
  // for. A read of the report already on its way started earlier, so it is
  // cancelled and never answers this question; the answer of the new read is
  // then stored for the page (docs/feedback-reports.md, Acts).
  const freshRead = useCallback(
    async (target: FixTaskTarget) => {
      const key = feedbackReportQueryKey(target.workspace, target.reportId)
      await queryClient.cancelQueries({ queryKey: key })
      try {
        const fresh = await fetchFeedbackReport(target.workspace, target.reportId)
        queryClient.setQueryData(key, fresh)
        return fresh
      } catch {
        return undefined
      }
    },
    [queryClient],
  )

  // The task is recorded on its report. The saved task of that report is
  // cleared, and only the flow that shows that report moves on.
  const finish = useCallback(
    (entry: CreatedFixTask, target: FixTaskTarget) => {
      writeCreatedFixTask(target.workspace, target.reportId, null)
      writeReportActState(reportActKey(target.workspace, target.reportId), { phase: 'idle' })
      void refreshAfterWrite(queryClient, target.workspace)
      if (!live(flowOf(target))) return
      setCreated(null)
      setRecord({ phase: 'idle' })
      onRecorded(entry.taskId)
    },
    [queryClient, onRecorded, live],
  )

  // A report that already names the created task as its fix task recorded
  // it, for example when an earlier answer was lost.
  useEffect(() => {
    if (created && data && reportId && data.id === reportId && data.fix_task?.id === created.taskId)
      finish(created, { workspace, reportId })
  }, [created, data, reportId, workspace, finish])

  // Sends the record request for one report. Every value comes from the
  // target and the entry captured at Create, and the answer changes only the
  // saved task of that report and, when it is shown, its flow.
  const sendRecord = useCallback(
    async (entry: CreatedFixTask, target: FixTaskTarget) => {
      const flow = flowOf(target)
      if (sending.current.has(flow)) return
      sending.current.add(flow)
      const save = (status: CreatedFixTask['status']) =>
        writeCreatedFixTask(target.workspace, target.reportId, { ...entry, status })
      const actKey = reportActKey(target.workspace, target.reportId)
      save('sending')
      if (live(flow)) setRecord({ phase: 'sending' })
      try {
        await setFeedbackReportFixTask(target.workspace, target.reportId, entry.taskId, entry.causeTaskIds)
        finish(entry, target)
      } catch (error) {
        if (error instanceof FeedbackReportActError && error.outcome === 'unknown') {
          // The record may be stored. Every copy of the report waits, and a
          // read that starts now decides the next step.
          save('unknown')
          writeReportActState(actKey, { phase: 'checking', act: 'fix', target: `report:${target.reportId}` })
          if (live(flow)) setRecord({ phase: 'checking' })
          const fresh = await freshRead(target)
          if (fresh?.fix_task?.id === entry.taskId) {
            finish(entry, target)
            return
          }
          writeReportActState(
            actKey,
            fresh
              ? { phase: 'idle' }
              : {
                  phase: 'stale',
                  act: 'fix',
                  target: `report:${target.reportId}`,
                  message: staleMessage,
                  reading: false,
                },
          )
          if (live(flow))
            setRecord({
              phase: 'failed',
              canSend: fresh !== undefined,
              message:
                fresh === undefined
                  ? 'Conveyor does not know whether the report recorded the task, and the report did not load again. Read the report again before you record the task.'
                  : 'The report did not record the task. Record it again.',
            })
        } else {
          save('not_recorded')
          if (!live(flow)) return
          if (error instanceof FeedbackReportActError && error.code === 'report_closed')
            void queryClient.invalidateQueries({
              queryKey: feedbackReportQueryKey(target.workspace, target.reportId),
            })
          setRecord({ phase: 'failed', canSend: true, message: recordFailureMessage(error) })
        }
      } finally {
        sending.current.delete(flow)
      }
    },
    [queryClient, finish, live, freshRead],
  )

  // Intake answered for the report captured at Create. The task is saved for
  // that report and its record request is sent. Only when that report is
  // still the one shown does the flow on the page change: a task created for
  // another report never enters this report's flow.
  const onCreated = useCallback(
    (task: { id: string; title: string }, target: FixTaskTarget, causeTaskIds: string[]) => {
      const entry: CreatedFixTask = { taskId: task.id, taskTitle: task.title, causeTaskIds }
      writeCreatedFixTask(target.workspace, target.reportId, { ...entry, status: 'sending' })
      if (live(flowOf(target))) setCreated(entry)
      void sendRecord(entry, target)
    },
    [live, sendRecord],
  )

  const rereadReport = useCallback(
    async (restored = false) => {
      if (!created || !reportId) return
      const target = { workspace, reportId }
      const flow = flowOf(target)
      // While this flow's record request is on its way, its own answer
      // decides the next step.
      if (sending.current.has(flow)) return
      setRecord({ phase: 'checking' })
      const fresh = await freshRead(target)
      if (fresh?.fix_task?.id === created.taskId) {
        finish(created, target)
        return
      }
      if (fresh) writeReportActState(reportActKey(workspace, reportId), { phase: 'idle' })
      if (!live(flow)) return
      if (fresh)
        setRecord(
          restored
            ? { phase: 'idle' }
            : { phase: 'failed', canSend: true, message: 'The report did not record the task. Record it again.' },
        )
      else
        setRecord({
          phase: 'failed',
          canSend: false,
          message: 'The report did not load. Read the report again before you record the task.',
        })
    },
    [created, workspace, reportId, finish, live, freshRead],
  )

  // A restored task: read the report once before the page offers anything.
  useEffect(() => {
    if (!restorePending.current || !created) return
    restorePending.current = false
    void rereadReport(true)
  }, [created, rereadReport])

  const linkedTasks = (data?.links ?? []).flatMap((link) => (link.task ? [link.task] : []))
  const ready = Boolean(reportId) && data?.state === 'open' && prefilledFor === reportId && created === null
  return {
    report,
    ready,
    causes,
    setCauses,
    linkedTasks,
    created,
    record,
    onCreated,
    // The record request again for the report shown, with its saved task.
    recordAgain: () => {
      if (created && reportId) void sendRecord(created, { workspace, reportId })
    },
    sendRecord,
    rereadReport,
  }
}

export type FixTaskFlow = ReturnType<typeof useFixTaskFlow>

/**
 * The fix-task part of the create sheet: the report it starts from, its
 * possible causes, and the record request with its answer.
 */
export function FixTaskPanel({
  flow,
  workspace,
  onLeaveReport,
}: {
  flow: FixTaskFlow
  workspace: string
  onLeaveReport: () => void
}) {
  const { report, created, record } = flow
  const data = report.data
  return (
    <section
      aria-label="Fix task"
      data-fix-task
      className="space-y-3 rounded-lg border border-primary/25 bg-primary-soft/40 px-3.5 py-3 text-sm"
    >
      <div className="flex min-w-0 items-start gap-2.5">
        <span className="mt-0.5 grid size-7 shrink-0 place-items-center rounded-md bg-primary-soft text-primary">
          <MessageSquareWarning aria-hidden className="size-3.5" />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-xs text-muted">This task is a fix for the report</p>
          <p className="whitespace-pre-wrap text-sm font-medium" data-fix-task-summary>
            {data ? <Untrusted>{data.summary}</Untrusted> : report.isPending ? 'Loading the report…' : ''}
          </p>
        </div>
      </div>
      {created ? (
        <CreatedPanel flow={flow} created={created} workspace={workspace} />
      ) : report.error != null && !data ? (
        <div role="alert" className="space-y-2 text-xs">
          <p className="text-failure">The report did not load, so Conveyor did not fill in the description.</p>
          <div className="flex flex-wrap gap-2">
            <Button size="sm" variant="secondary" disabled={report.isFetching} onClick={() => void report.refetch()}>
              {report.isFetching ? 'Loading…' : 'Try again'}
            </Button>
            <Button size="sm" variant="ghost" onClick={onLeaveReport}>
              Create a task without the report
            </Button>
          </div>
        </div>
      ) : data && data.state !== 'open' ? (
        <div role="alert" className="space-y-2 text-xs">
          <p className="text-failure">This report is closed. Reopen it before you start a fix task from it.</p>
          <Button size="sm" variant="ghost" onClick={onLeaveReport}>
            Create a task without the report
          </Button>
        </div>
      ) : data ? (
        <CauseList flow={flow} />
      ) : null}
      {!created && record.phase === 'idle' && data?.state === 'open' && (
        <p className="text-xs text-faint">
          The description holds the report as untrusted data. Choose a repository. When you create the task, Conveyor
          records it on the report as its fix task.
        </p>
      )}
    </section>
  )
}

function CauseList({ flow }: { flow: FixTaskFlow }) {
  const { linkedTasks, causes, setCauses } = flow
  const full = causes.length >= MAX_CAUSE_TASKS
  if (linkedTasks.length === 0)
    return <p className="text-xs text-muted">The report has no linked task to tick as a possible cause.</p>
  return (
    <fieldset className="min-w-0 space-y-1.5">
      <legend className="text-xs font-medium text-muted">Possible causes</legend>
      <p id="fix-task-cause-rule" className="text-xs text-faint">
        Tick a linked task only if you know that it caused the problem. You can tick up to {MAX_CAUSE_TASKS} tasks.
        Conveyor confirms the link to each task you tick. {causes.length} of {MAX_CAUSE_TASKS} ticked.
      </p>
      {full && (
        <p role="status" className="text-xs text-attention">
          You ticked {MAX_CAUSE_TASKS} tasks. Untick one before you tick another.
        </p>
      )}
      <ul className="max-h-48 min-w-0 space-y-1 overflow-y-auto">
        {linkedTasks.map((task) => {
          const ticked = causes.includes(task.id)
          return (
            <li key={task.id} className="min-w-0">
              <label className="flex min-w-0 cursor-pointer items-start gap-2 rounded px-1.5 py-1 text-xs hover:bg-raised/50">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={ticked}
                  disabled={!ticked && full}
                  aria-describedby="fix-task-cause-rule"
                  onChange={(event) =>
                    setCauses((current) =>
                      event.target.checked
                        ? current.length >= MAX_CAUSE_TASKS || current.includes(task.id)
                          ? current
                          : [...current, task.id]
                        : current.filter((id) => id !== task.id),
                    )
                  }
                />
                <span className="min-w-0 flex-1">
                  <Untrusted>{task.title}</Untrusted> <span className="font-mono text-faint">{task.id}</span>
                </span>
              </label>
            </li>
          )
        })}
      </ul>
    </fieldset>
  )
}

function CreatedPanel({ flow, created, workspace }: { flow: FixTaskFlow; created: CreatedFixTask; workspace: string }) {
  const { record } = flow
  const busy = record.phase === 'sending' || record.phase === 'checking'
  return (
    <div data-fix-task-created className="space-y-2 text-xs">
      <p>
        The task{' '}
        <Link
          to="/w/$workspaceId/tasks/$taskId"
          params={{ workspaceId: workspace, taskId: created.taskId }}
          className="rounded-sm text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary"
        >
          {created.taskTitle ? <Untrusted>{created.taskTitle}</Untrusted> : created.taskId}
        </Link>{' '}
        exists.{' '}
        {record.phase === 'sending'
          ? 'Conveyor records it on the report now…'
          : record.phase === 'checking'
            ? 'Conveyor reads the report again to find out if it recorded the task…'
            : record.phase === 'failed'
              ? 'The report did not record it as the fix task.'
              : 'The report does not record it yet.'}
      </p>
      {record.phase === 'failed' && (
        <p role="alert" data-fix-task-error className="text-failure">
          {record.message}
        </p>
      )}
      {!busy && (
        <div className="flex flex-wrap gap-2">
          {(record.phase !== 'failed' || record.canSend) && (
            <Button size="sm" onClick={flow.recordAgain}>
              Record it on the report
            </Button>
          )}
          {record.phase === 'failed' && !record.canSend && (
            <Button size="sm" variant="secondary" onClick={() => void flow.rereadReport()}>
              Read the report again
            </Button>
          )}
        </div>
      )}
      <p className="text-faint">
        A new attempt sends only the record request. It does not create another task.
        {created.causeTaskIds.length > 0 &&
          ` It confirms the links to the ${created.causeTaskIds.length} ${created.causeTaskIds.length === 1 ? 'task' : 'tasks'} you ticked.`}
      </p>
    </div>
  )
}
