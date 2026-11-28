import { hashKey, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { MessageSquareWarning } from 'lucide-react'
import { type Dispatch, type SetStateAction, useCallback, useEffect, useRef, useState } from 'react'
import {
  type CreateTaskInput,
  FeedbackReportActError,
  fetchFeedbackReport,
  setFeedbackReportFixTask,
} from '../../lib/api'
import type { FeedbackReport } from '../../lib/types'
import { withQuerySignal } from '../task/use-task-detail'
import { Button } from '../ui/button'
import { feedbackReportQueryKey } from './query-keys'
import { Untrusted } from './report-card'
import { fixTaskDescription, MAX_CAUSE_TASKS } from './report-text'
import {
  type ReportActState,
  readReportActState,
  refreshAfterWrite,
  reportActBlocked,
  reportActKey,
  reportActMessage,
  staleMessage,
  writeReportActState,
} from './use-report-act'

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

// The record request takes part in the act state of its report
// (`reportActKey`), under a target of its own, so it blocks every card of the
// report while it is on its way or its outcome is unknown, and it only ever
// changes a state that it wrote itself.
function fixActTarget(target: FixTaskTarget) {
  return `fix:${flowOf(target)}`
}

// The fresh read of each report that started last, by its query key. One
// entry stays for each report read on this page.
const latestFreshReads = new Map<string, Promise<FeedbackReport | undefined>>()

// Changes the report's act state only while it is still `expected`: the state
// that this request wrote, or the state read before this read started. A
// newer state, such as the record request of the same report sent again after
// the sheet closed and opened, stays.
function replaceFixState(target: FixTaskTarget, expected: ReportActState, next: ReportActState) {
  const key = reportActKey(target.workspace, target.reportId)
  if (readReportActState(key) === expected) writeReportActState(key, next)
}

// A state that the record request of this report wrote.
function ownFixState(current: ReportActState, target: FixTaskTarget) {
  return 'target' in current && current.target === fixActTarget(target)
}

function setOwnFixState(target: FixTaskTarget, next: ReportActState) {
  const key = reportActKey(target.workspace, target.reportId)
  const current = readReportActState(key)
  if (current.phase === 'idle' || ('target' in current && current.target === fixActTarget(target)))
    writeReportActState(key, next)
}

const blockedRecordMessage =
  'Another act on this report is not settled yet. Read the report again before you record the task.'

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

function intakeStorageKey(target: FixTaskTarget) {
  return `conveyor-fix-task-intake:${target.workspace}:${target.reportId}`
}

/**
 * A fix-task intake request as Create sent it: its intake key, the request,
 * and the causes ticked for it. It is saved before the request is sent and
 * kept until the report records the task, so a Create after a reload or a
 * lost answer sends this same request again, with its own description and
 * causes, and the server answers with the same task instead of a second one
 * or a refusal of a changed request. Each report has its own.
 */
export interface SentFixTaskIntake {
  key: string
  request: CreateTaskInput
  causeTaskIds: string[]
  // The files sent with the request. Only this page holds them: session
  // storage keeps no file, so it keeps their names and sizes, and after a
  // reload Create waits until the form holds the same files again.
  files?: File[]
  fileNames: { name: string; size: number }[]
  // False when session storage refused to keep this request, so it does not
  // survive a reload.
  saved?: boolean
}

// The sent request of each report target while this page is open. It keeps a
// retry on the same request when session storage refuses to hold it.
const memoryIntakes = new Map<string, SentFixTaskIntake>()

function parseSentIntake(raw: string | null): SentFixTaskIntake | null {
  if (!raw) return null
  try {
    const value = JSON.parse(raw) as Partial<SentFixTaskIntake>
    const request = value.request as Partial<CreateTaskInput> | undefined
    if (typeof value.key !== 'string' || !value.key) return null
    if (!request || typeof request.body !== 'string' || typeof request.repo !== 'string') return null
    const causes = Array.isArray(value.causeTaskIds)
      ? value.causeTaskIds.filter((id): id is string => typeof id === 'string')
      : []
    const fileNames = Array.isArray(value.fileNames)
      ? value.fileNames.filter(
          (file): file is { name: string; size: number } =>
            typeof file?.name === 'string' && typeof file?.size === 'number',
        )
      : []
    return { key: value.key, request: request as CreateTaskInput, causeTaskIds: causes, fileNames, saved: true }
  } catch {
    return null
  }
}

/**
 * True when the files of a request sent before are on this page, or when the
 * form holds the same files again after a reload.
 */
export function sentFilesPresent(sent: SentFixTaskIntake, files: File[]) {
  if (sent.files) return true
  const held = files.map((file) => `${file.name}\u0000${file.size}`).sort()
  const wanted = sent.fileNames.map((file) => `${file.name}\u0000${file.size}`).sort()
  return held.length === wanted.length && held.every((name, index) => name === wanted[index])
}

/** The request sent before for this report whose answer did not arrive, if any. */
export function sentFixTaskIntake(target: FixTaskTarget): SentFixTaskIntake | null {
  const name = intakeStorageKey(target)
  const remembered = memoryIntakes.get(name)
  if (remembered) return remembered
  try {
    return parseSentIntake(window.sessionStorage.getItem(name))
  } catch {
    return null
  }
}

/**
 * The intake request to send for this report: the one sent before when there
 * is one, else this new one, which is saved before it is sent.
 */
export function fixTaskIntakeToSend(
  target: FixTaskTarget,
  request: CreateTaskInput,
  causeTaskIds: string[],
  files: File[],
): SentFixTaskIntake {
  const before = sentFixTaskIntake(target)
  const sent: SentFixTaskIntake = before
    ? { ...before, files: before.files ?? files }
    : {
        key: crypto.randomUUID(),
        request,
        causeTaskIds,
        files,
        fileNames: files.map((file) => ({ name: file.name, size: file.size })),
      }
  const name = intakeStorageKey(target)
  try {
    window.sessionStorage.setItem(
      name,
      JSON.stringify({
        key: sent.key,
        request: sent.request,
        causeTaskIds: sent.causeTaskIds,
        fileNames: sent.fileNames,
      }),
    )
    sent.saved = true
  } catch {
    // Session storage refused, for example when it is full: the request
    // stays in memory only, and the sheet says that it does not survive a
    // reload.
    sent.saved = false
  }
  memoryIntakes.set(name, sent)
  return sent
}

/**
 * Forgets the sent request of this report: when the report records the task,
 * or when the server refused the request and so stored nothing under its key.
 */
export function clearFixTaskIntake(target: FixTaskTarget) {
  const name = intakeStorageKey(target)
  memoryIntakes.delete(name)
  try {
    window.sessionStorage.removeItem(name)
  } catch {
    // Nothing was saved.
  }
}

/** False when session storage refuses to keep a value, so nothing survives a reload. */
export function fixTaskStorageWorks() {
  try {
    const probe = 'conveyor-fix-task-probe'
    window.sessionStorage.setItem(probe, '1')
    window.sessionStorage.removeItem(probe)
    return true
  } catch {
    return false
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
  restoreRequest,
  onRecorded,
}: {
  workspace: string
  reportId: string | undefined
  setBody: Dispatch<SetStateAction<string>>
  // Puts the fields of a request sent before back into the form.
  restoreRequest?: (request: CreateTaskInput) => void
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
  // stays, after the report block. A request sent before for this report,
  // whose answer did not arrive, fills the form instead, with its own
  // description and causes, because Create sends that request again.
  const data = report.data
  useEffect(() => {
    if (!data || data.id !== reportId || prefilledFor === data.id || created) return
    if (data.state !== 'open') return
    const sent = sentFixTaskIntake({ workspace, reportId })
    if (sent) {
      lastPrefill.current = sent.request.body
      setPrefilledFor(data.id)
      setBody(sent.request.body)
      setCauses(sent.causeTaskIds)
      restoreRequest?.(sent.request)
      return
    }
    const text = fixTaskDescription(data)
    const previous = lastPrefill.current
    lastPrefill.current = text
    setPrefilledFor(data.id)
    setBody((current) => (current.trim() === '' || current === previous ? text : `${text}\n${current}`))
  }, [data, workspace, reportId, prefilledFor, created, setBody, restoreRequest])

  // A fresh read of one report that started after the moment it is asked
  // for. A read of the report already on its way started earlier, so it is
  // cancelled and never answers this question; the answer of the new read is
  // then stored for the page (docs/feedback-reports.md, Acts).
  const readOnce = useCallback(
    async (target: FixTaskTarget): Promise<{ report: FeedbackReport | undefined; stored: boolean }> => {
      const key = feedbackReportQueryKey(target.workspace, target.reportId)
      await queryClient.cancelQueries({ queryKey: key })
      // A read of the report that starts after this one and answers first is
      // newer: then that answer stands, and this one neither writes the cache
      // nor decides.
      let newer = false
      let started = false
      const keyHash = hashKey(key)
      const unsubscribe = queryClient.getQueryCache().subscribe((event) => {
        if (event.type !== 'updated' || event.query.queryHash !== keyHash) return
        if (event.action.type === 'fetch') started = true
        else if (event.action.type === 'success' && !event.action.manual && started) newer = true
      })
      try {
        const fresh = await fetchFeedbackReport(target.workspace, target.reportId)
        if (newer) return { report: queryClient.getQueryData<FeedbackReport>(key), stored: false }
        return { report: fresh, stored: true }
      } catch {
        return { report: newer ? queryClient.getQueryData<FeedbackReport>(key) : undefined, stored: false }
      } finally {
        unsubscribe()
      }
    },
    [queryClient],
  )

  const freshRead = useCallback(
    (target: FixTaskTarget): Promise<FeedbackReport | undefined> => {
      const keyHash = hashKey(feedbackReportQueryKey(target.workspace, target.reportId))
      // Of two fresh reads of one report, from this sheet or from a sheet
      // opened again, only the one that started last stores and decides. An
      // earlier one hands over to it.
      const mine: Promise<FeedbackReport | undefined> = readOnce(target).then((answer) => {
        // The entry stays after the newest read answers, so an older read that
        // answers later still finds it and hands over.
        const latest = latestFreshReads.get(keyHash)
        if (latest !== undefined && latest !== mine) return latest
        if (answer.stored)
          queryClient.setQueryData(feedbackReportQueryKey(target.workspace, target.reportId), answer.report)
        return answer.report
      })
      latestFreshReads.set(keyHash, mine)
      return mine
    },
    [queryClient, readOnce],
  )

  // The task is recorded on its report. The saved task of that report is
  // cleared, and only the flow that shows that report moves on. The report's
  // act state goes only when `mayClear` accepts it: the request or read that
  // found the record names the state it wrote or read first. A refresh that
  // finds the record may hold data from before an unknown outcome, so it
  // leaves every state of a record request to that request and to a read
  // that starts later.
  const finish = useCallback(
    (
      entry: CreatedFixTask,
      target: FixTaskTarget,
      mayClear: (current: ReportActState) => boolean = (current) => !ownFixState(current, target),
    ) => {
      writeCreatedFixTask(target.workspace, target.reportId, null)
      clearFixTaskIntake(target)
      if (mayClear(readReportActState(reportActKey(target.workspace, target.reportId))))
        setOwnFixState(target, { phase: 'idle' })
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
  // While the record request of this report is on its way, its own answer
  // finishes the flow, so the shared state stays until that answer arrives.
  // The check runs again when the record state changes, so a record request
  // that ends after the report already named the task finishes the flow.
  const recordPhase = record.phase
  useEffect(() => {
    if (!created || !data || !reportId || data.id !== reportId || data.fix_task?.id !== created.taskId) return
    const target = { workspace, reportId }
    if (recordPhase === 'sending' || sending.current.has(flowOf(target))) return
    finish(created, target)
  }, [created, data, reportId, workspace, finish, recordPhase])

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
      const fixTarget = fixActTarget(target)
      // Another act of the report is on its way or unresolved: the record
      // waits for a read that answers.
      if (reportActBlocked(readReportActState(actKey))) {
        sending.current.delete(flow)
        save('not_recorded')
        if (live(flow)) setRecord({ phase: 'failed', canSend: false, message: blockedRecordMessage })
        return
      }
      const sendingState: ReportActState = { phase: 'sending', act: 'fix', target: fixTarget }
      writeReportActState(actKey, sendingState)
      save('sending')
      if (live(flow)) setRecord({ phase: 'sending' })
      try {
        await setFeedbackReportFixTask(target.workspace, target.reportId, entry.taskId, entry.causeTaskIds)
        finish(entry, target, (current) => current === sendingState)
      } catch (error) {
        if (error instanceof FeedbackReportActError && error.outcome === 'unknown') {
          // The record may be stored. Every copy of the report waits, and a
          // read that starts now decides the next step.
          save('unknown')
          const checkingState: ReportActState = { phase: 'checking', act: 'fix', target: fixTarget }
          replaceFixState(target, sendingState, checkingState)
          if (live(flow)) setRecord({ phase: 'checking' })
          const fresh = await freshRead(target)
          if (fresh?.fix_task?.id === entry.taskId) {
            finish(entry, target, (current) => current === checkingState)
            return
          }
          replaceFixState(
            target,
            checkingState,
            fresh
              ? { phase: 'idle' }
              : { phase: 'stale', act: 'fix', target: fixTarget, message: staleMessage, reading: false },
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
          replaceFixState(target, sendingState, { phase: 'idle' })
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
      // Only a state that was already blocked by a failed read when this read
      // started can be settled by it. A state that turns stale during the
      // read waits for the next read.
      const key = reportActKey(workspace, reportId)
      const before = readReportActState(key)
      const fresh = await freshRead(target)
      // A record request on its way, perhaps from this sheet before it closed
      // and opened again, is settled only by its own answer.
      const settles = (current: ReportActState) => current === before && current.phase !== 'sending'
      if (fresh?.fix_task?.id === created.taskId) {
        finish(created, target, settles)
        return
      }
      // A fresh read of the report answered after any unknown outcome, so it
      // settles the report: its own state goes, and another act's failed read
      // becomes the note after a read.
      if (fresh) {
        const current = readReportActState(key)
        if (current.phase === 'stale' && current === before && current.target !== fixActTarget(target))
          writeReportActState(key, {
            phase: 'failed',
            act: current.act,
            target: current.target,
            field: '',
            message: reportActMessage(new FeedbackReportActError(0, '', '', '', 'unknown')),
            afterUnknown: true,
          })
        else if (settles(current)) setOwnFixState(target, { phase: 'idle' })
      }
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
  // An intake request for this report was sent before, and its answer did not
  // reach this page.
  const [, setDiscarded] = useState(0)
  // Read when the form renders, not when the flow does: the request is saved
  // by the form's Create, which renders the form alone.
  const readSentRequest = () => (reportId && created === null ? sentFixTaskIntake({ workspace, reportId }) : null)
  const ready = Boolean(reportId) && data?.state === 'open' && prefilledFor === reportId && created === null
  return {
    report,
    ready,
    get intakeSentBefore() {
      return readSentRequest() !== null
    },
    get sentRequest() {
      return readSentRequest()
    },
    // Forgets the request sent before, so Create sends the form as it is. The
    // person chooses this after looking for the task on the Board.
    discardSentRequest: () => {
      if (!reportId) return
      clearFixTaskIntake({ workspace, reportId })
      setDiscarded((count) => count + 1)
    },
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
      {(!fixTaskStorageWorks() || flow.sentRequest?.saved === false) && (
        <p role="status" data-fix-task-no-storage className="text-xs text-attention">
          This browser does not keep this request across a reload. If you reload the page before the task opens, look
          for the task on the Board before you create it again.
        </p>
      )}
      {flow.intakeSentBefore && (
        <p role="status" data-fix-task-intake-sent className="text-xs text-attention">
          A request to create this task was sent before, and its answer did not arrive. The form shows that request.
          Create sends it again unchanged, so it does not make a second task. Changes that you make now are not sent.{' '}
          <button
            type="button"
            data-fix-task-discard
            className="rounded-sm text-primary underline focus-visible:outline-2 focus-visible:outline-primary"
            onClick={flow.discardSentRequest}
          >
            Discard that request
          </button>{' '}
          only after you look for the task on the Board: if the request reached Conveyor, a new request makes a second
          task.
        </p>
      )}
      {flow.sentRequest && !flow.sentRequest.files && flow.sentRequest.fileNames.length > 0 && (
        <p role="status" data-fix-task-attach-again className="text-xs text-attention">
          That request had these files, and this page does not hold them after the reload. Attach the same files again
          before you create the task:{' '}
          {flow.sentRequest.fileNames.map((file, index) => (
            <span key={`${file.name}-${index}`}>
              {index > 0 && ', '}
              <bdi className="text-foreground [overflow-wrap:anywhere]">{file.name}</bdi>
            </span>
          ))}
          .
        </p>
      )}
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
