import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { Check, Link2, Package, RotateCcw, Unlink, Wrench, X } from 'lucide-react'
import { type ReactNode, useDeferredValue, useId, useState } from 'react'
import {
  closeFeedbackReport,
  fetchCrates,
  fetchMergedTasks,
  linkFeedbackReport,
  removeFeedbackReportLink,
  reopenFeedbackReport,
} from '../../lib/api'
import type { FeedbackReport, FeedbackReportEvidence, FeedbackReportLink, FeedbackReportNote } from '../../lib/types'
import { absoluteTime, cn, relativeTime } from '../../lib/utils'
import { useWorkspaceCapability, useWorkspaceSelection } from '../app-shell'
import { WatcherState } from '../feedback/watcher-state'
import { withQuerySignal } from '../task/use-task-detail'
import { Badge } from '../ui/badge'
import { Button } from '../ui/button'
import { Input, Textarea } from '../ui/input'
import {
  CLOSE_REASON_MAX,
  closeReasonLength,
  closeReasonProblem,
  deployText,
  noteNamesDeploy,
  noteSentence,
  reportKindLabel,
} from './report-text'
import { type ReportActState, useReportAct } from './use-report-act'

/**
 * Report text is untrusted data (component-feedback-reports invariant 5). It
 * is printed as a React text node, never as Markdown or HTML. `bdi` keeps a
 * right-to-left mark inside the value from reordering the words around it,
 * and the wrap rule breaks a very long word so it never widens the page.
 */
export function Untrusted({ children, className }: { children: string; className?: string }) {
  return <bdi className={cn('[overflow-wrap:anywhere]', className)}>{children}</bdi>
}

function ServedTime({ value }: { value: string }) {
  return (
    <time dateTime={value} title={absoluteTime(value)}>
      {relativeTime(value)}
    </time>
  )
}

function countText(count: number) {
  return `${count.toLocaleString('en')} ${count === 1 ? 'time' : 'times'}`
}

/**
 * The one card that prints a report wherever it shows
 * (component-feedback-reports invariant 25; req-production-feedback AC-5.3
 * to AC-5.5, AC-6.1): the kind, the summary, the environment, the service,
 * the count, the first-seen and last-seen times, each link with its state and
 * evidence, the notes, the fix task, and the acts for a user who may create
 * tasks. It prints what the served report holds and builds no check of its
 * own (invariant 21). `extra` is a line a page adds above the links, such as
 * the crate member a report belongs to through.
 */
export function ReportCard({ report, extra }: { report: FeedbackReport; extra?: ReactNode }) {
  const { workspace } = useWorkspaceSelection()
  const canAct = useWorkspaceCapability('create_tasks')
  const act = useReportAct(workspace)
  const summaryId = useId()
  const open = report.state === 'open'
  const deploysWithNoTask = report.notes.filter((note) => note.kind === 'no_task_deploy')
  const otherNotes = report.notes.filter((note) => note.kind !== 'no_task_deploy')
  return (
    <article
      aria-labelledby={summaryId}
      data-report-card={report.id}
      data-report-state={report.state}
      className="min-w-0 space-y-3 rounded-lg border border-border bg-card px-3.5 py-3 text-sm"
    >
      <header className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
        <Badge variant={report.kind === 'error' ? 'failure' : report.kind === 'alert' ? 'attention' : 'default'}>
          {reportKindLabel(report.kind)}
        </Badge>
        {!open && <Badge variant="outline">Closed</Badge>}
        <span data-report-count className="text-xs tabular-nums text-muted">
          Seen {countText(report.count)}
        </span>
      </header>
      <p id={summaryId} data-report-summary className="whitespace-pre-wrap font-medium leading-snug">
        <Untrusted>{report.summary}</Untrusted>
      </p>
      <dl className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs">
        <dt className="text-faint">Environment</dt>
        <dd data-report-environment className="min-w-0">
          <Untrusted>{report.environment.name}</Untrusted>
        </dd>
        <dt className="text-faint">Service</dt>
        <dd data-report-service className="min-w-0">
          {report.service === null ? (
            <span className="text-muted">Not stated</span>
          ) : (
            <Untrusted>{report.service}</Untrusted>
          )}
        </dd>
        <dt className="text-faint">First seen</dt>
        <dd data-report-first-seen>
          <ServedTime value={report.first_seen} />
        </dd>
        <dt className="text-faint">Last seen</dt>
        <dd data-report-last-seen>
          <ServedTime value={report.last_seen} />
        </dd>
        <dt className="text-faint">Key</dt>
        <dd data-report-key className="min-w-0 font-mono">
          <Untrusted>{report.key}</Untrusted>
        </dd>
        {report.file !== null && (
          <>
            <dt className="text-faint">File</dt>
            <dd data-report-file className="min-w-0 font-mono">
              <Untrusted>{report.file}</Untrusted>
              {report.line !== null && <span className="text-muted">, line {report.line}</span>}
            </dd>
          </>
        )}
      </dl>
      {/* Only the served false says that no watcher is listening, with the
          served last signal time (component-feedback-reports invariant 22;
          req-production-feedback AC-2.2). */}
      {report.environment.listening === false && (
        <WatcherState listening={false} lastSignalAt={report.environment.last_signal_at} className="text-xs" />
      )}
      {!open && report.closed && (
        <p data-report-closed className="text-xs text-muted">
          Closed <ServedTime value={report.closed.at} />. Reason:{' '}
          <span className="whitespace-pre-wrap text-foreground">
            <Untrusted>{report.closed.reason}</Untrusted>
          </span>
        </p>
      )}
      {open && report.reopened_at && (
        <p data-report-reopened className="text-xs text-muted">
          Reopened <ServedTime value={report.reopened_at} />.
        </p>
      )}
      {extra}
      <section aria-label="Links" className="min-w-0 space-y-2">
        {report.links_pending && (
          <p data-report-links-pending className="text-xs text-muted">
            Conveyor still looks for tasks to link to this report.
          </p>
        )}
        {report.links.length === 0 ? (
          <div data-report-no-link className="space-y-1 text-xs">
            <p>No task explains this report.</p>
            {deploysWithNoTask.length > 0 && (
              <ul className="space-y-0.5 text-muted">
                {deploysWithNoTask.map((note, index) => (
                  <NoteLine key={`no-task-${index}`} note={note} />
                ))}
              </ul>
            )}
          </div>
        ) : (
          <>
            <p className="text-xs text-muted">
              A possible link is evidence only. It does not say that the task caused the problem.
            </p>
            <ul className="space-y-2">
              {report.links.map((link) => (
                <LinkRow key={link.id} report={report} link={link} canAct={canAct && open} act={act} />
              ))}
            </ul>
            {deploysWithNoTask.length > 0 && (
              <ul className="space-y-0.5 text-xs text-muted">
                {deploysWithNoTask.map((note, index) => (
                  <NoteLine key={`no-task-${index}`} note={note} />
                ))}
              </ul>
            )}
          </>
        )}
        {otherNotes.length > 0 && (
          <ul data-report-notes className="space-y-0.5 text-xs text-muted">
            {otherNotes.map((note, index) => (
              <NoteLine key={`${note.kind}-${index}`} note={note} />
            ))}
          </ul>
        )}
      </section>
      {report.fix_task && (
        <p data-report-fix-task className="min-w-0 text-xs">
          <span className="text-faint">Fix task: </span>
          <Link
            to="/w/$workspaceId/tasks/$taskId"
            params={{ workspaceId: workspace, taskId: report.fix_task.id }}
            className="rounded-sm text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary"
          >
            <Untrusted>{report.fix_task.title}</Untrusted>
          </Link>
        </p>
      )}
      {canAct && <ReportActs report={report} act={act} />}
    </article>
  )
}

function NoteLine({ note }: { note: FeedbackReportNote }) {
  if (noteNamesDeploy(note) && note.deploy) {
    return (
      <li data-report-note={note.kind}>
        The deploy of <Untrusted>{deployText(note.deploy)}</Untrusted> <ServedTime value={note.deploy.deployed_at} />{' '}
        {noteSentence(note)}
      </li>
    )
  }
  return (
    <li data-report-note={note.kind}>
      {noteSentence(note)}
      {note.kind === 'file_not_found' && note.file && (
        <>
          {' '}
          File: <Untrusted className="font-mono">{note.file}</Untrusted>.
        </>
      )}
      {(note.kind === 'commit_unresolved' || note.kind === 'not_checked') && note.reason && (
        <>
          {' '}
          Reason: <Untrusted>{note.reason}</Untrusted>.
        </>
      )}
    </li>
  )
}

function EvidenceLine({ evidence }: { evidence: FeedbackReportEvidence }) {
  if (evidence.kind === 'line') {
    return (
      <li data-report-evidence="line">
        Line evidence: <Untrusted className="font-mono">{evidence.file ?? ''}</Untrusted>, line {evidence.line}, last
        changed in commit <span className="font-mono">{(evidence.commit ?? '').slice(0, 12)}</span>.
      </li>
    )
  }
  if (evidence.kind === 'timing') {
    return (
      <li data-report-evidence="timing">
        Timing evidence: a deploy of <Untrusted>{evidence.service ?? ''}</Untrusted>{' '}
        <Untrusted>
          {evidence.version?.version ?? evidence.version?.commit?.slice(0, 12) ?? 'an unknown version'}
        </Untrusted>{' '}
        {evidence.deployed_at && <ServedTime value={evidence.deployed_at} />}
        {evidence.prior_signal_at ? (
          <>
            . The watcher first saw the problem after the signal of <ServedTime value={evidence.prior_signal_at} /> and
            before Conveyor recorded this deploy.
          </>
        ) : (
          <>
            , {evidence.elapsed_minutes ?? 0} {evidence.elapsed_minutes === 1 ? 'minute' : 'minutes'} before the watcher
            first saw the problem.
          </>
        )}
      </li>
    )
  }
  return <li data-report-evidence={evidence.kind}>Evidence of an unknown kind.</li>
}

function linkName(link: FeedbackReportLink) {
  return link.task?.title ?? link.crate?.name ?? link.id
}

function LinkRow({
  report,
  link,
  canAct,
  act,
}: {
  report: FeedbackReport
  link: FeedbackReportLink
  canAct: boolean
  act: ReturnType<typeof useReportAct>
}) {
  const { workspace } = useWorkspaceSelection()
  const [confirmingRemoval, setConfirmingRemoval] = useState(false)
  const possible = link.state === 'possible'
  const name = linkName(link)
  const sending = (act.state.phase === 'sending' || act.state.phase === 'checking') && act.state.target === link.id
  return (
    <li
      data-report-link={link.id}
      data-link-state={link.state}
      className="min-w-0 rounded-md border border-border p-2.5"
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
        <Badge variant={possible ? 'default' : 'accent'}>{possible ? 'Possible link' : 'Confirmed link'}</Badge>
        {link.task ? (
          <span className="min-w-0 text-xs">
            <span className="text-faint">Task </span>
            <Link
              to="/w/$workspaceId/tasks/$taskId"
              params={{ workspaceId: workspace, taskId: link.task.id }}
              className="rounded-sm text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary"
            >
              <Untrusted>{link.task.title}</Untrusted>
            </Link>
            {link.task.crate && (
              <>
                <span className="text-faint"> in the crate </span>
                <Link
                  to="/w/$workspaceId/crates/$crateId"
                  params={{ workspaceId: workspace, crateId: link.task.crate.id }}
                  className="rounded-sm text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary"
                >
                  <Untrusted>{link.task.crate.name}</Untrusted>
                </Link>
              </>
            )}
          </span>
        ) : link.crate ? (
          <span className="min-w-0 text-xs">
            <span className="text-faint">Crate </span>
            <Link
              to="/w/$workspaceId/crates/$crateId"
              params={{ workspaceId: workspace, crateId: link.crate.id }}
              className="rounded-sm text-primary hover:underline focus-visible:outline-2 focus-visible:outline-primary"
            >
              <Untrusted>{link.crate.name}</Untrusted>
            </Link>
          </span>
        ) : null}
        {!possible && link.confirmed_at && (
          <span className="text-xs text-faint">
            Confirmed <ServedTime value={link.confirmed_at} />
          </span>
        )}
      </div>
      {link.evidence.length > 0 && (
        <ul className="mt-1.5 space-y-0.5 text-xs text-muted">
          {link.evidence.map((evidence, index) => (
            <EvidenceLine key={`${evidence.kind}-${index}`} evidence={evidence} />
          ))}
        </ul>
      )}
      {canAct && (
        <div className="mt-2 flex flex-wrap items-center gap-2">
          {possible && !confirmingRemoval && (
            <Button
              size="sm"
              variant="secondary"
              aria-label={`Confirm the link to ${name}`}
              disabled={act.pending}
              onClick={() =>
                void act.run('confirm', link.id, () =>
                  linkFeedbackReport(
                    workspace,
                    report.id,
                    link.task ? { task_id: link.task.id } : { crate_id: link.crate?.id ?? '' },
                  ),
                )
              }
            >
              <Check aria-hidden />
              {sending && act.state.phase === 'sending' && act.state.act === 'confirm' ? 'Confirming…' : 'Confirm'}
            </Button>
          )}
          {confirmingRemoval ? (
            <>
              <span className="text-xs">Remove this link? Conveyor will not link this task again by itself.</span>
              <Button
                size="sm"
                variant="destructive"
                aria-label={`Remove the link to ${name}`}
                disabled={act.pending}
                onClick={() =>
                  void act
                    .run('remove', link.id, () => removeFeedbackReportLink(workspace, report.id, link.id))
                    .then((done) => done && setConfirmingRemoval(false))
                }
              >
                <Unlink aria-hidden />
                {sending && act.state.phase === 'sending' && act.state.act === 'remove' ? 'Removing…' : 'Remove link'}
              </Button>
              <Button size="sm" variant="ghost" disabled={act.pending} onClick={() => setConfirmingRemoval(false)}>
                Keep it
              </Button>
            </>
          ) : (
            <Button
              size="sm"
              variant="ghost"
              aria-label={`Remove the link to ${name}…`}
              disabled={act.pending}
              onClick={() => {
                act.clear()
                setConfirmingRemoval(true)
              }}
            >
              <Unlink aria-hidden />
              Remove
            </Button>
          )}
        </div>
      )}
      <ActStatus state={act.state} target={link.id} />
    </li>
  )
}

/** The answer of the last act on `target`: a failure, or a fresh read after an unknown answer. */
function ActStatus({ state, target }: { state: ReportActState; target: string }) {
  if (state.phase === 'checking' && state.target === target)
    return (
      <p role="status" className="mt-1.5 text-xs text-muted">
        Conveyor does not know whether it recorded this. Conveyor reads the report again…
      </p>
    )
  if (state.phase === 'failed' && state.target === target)
    return (
      <p role="alert" data-report-act-error className="mt-1.5 text-xs text-failure">
        {state.message}
      </p>
    )
  return null
}

type Form = 'link' | 'close' | null

/**
 * The acts on the whole report for a user who may create tasks
 * (component-feedback-reports invariant 17; req-production-feedback REQ-7).
 * An open report offers a link to a merged task or a crate, closing with a
 * reason, and starting a fix task. A closed report offers only reopening. No
 * shortcut key records an act (invariant 26).
 */
function ReportActs({ report, act }: { report: FeedbackReport; act: ReturnType<typeof useReportAct> }) {
  const { workspace } = useWorkspaceSelection()
  const [form, setForm] = useState<Form>(null)
  const open = report.state === 'open'
  const target = `report:${report.id}`
  if (!open) {
    return (
      <div className="border-t border-border pt-3">
        <Button
          size="sm"
          variant="secondary"
          disabled={act.pending}
          onClick={() => void act.run('reopen', target, () => reopenFeedbackReport(workspace, report.id))}
        >
          <RotateCcw aria-hidden />
          {act.state.phase === 'sending' && act.state.act === 'reopen' ? 'Reopening…' : 'Reopen report'}
        </Button>
        <ActStatus state={act.state} target={target} />
      </div>
    )
  }
  return (
    <div className="space-y-3 border-t border-border pt-3">
      <div className="flex flex-wrap gap-2">
        <Button
          size="sm"
          variant={form === 'link' ? 'outline' : 'secondary'}
          aria-expanded={form === 'link'}
          onClick={() => {
            act.clear()
            setForm(form === 'link' ? null : 'link')
          }}
        >
          <Link2 aria-hidden />
          Link a task or crate
        </Button>
        <Button
          size="sm"
          variant={form === 'close' ? 'outline' : 'secondary'}
          aria-expanded={form === 'close'}
          onClick={() => {
            act.clear()
            setForm(form === 'close' ? null : 'close')
          }}
        >
          <X aria-hidden />
          Close report
        </Button>
        <Link
          to="/w/$workspaceId/"
          params={{ workspaceId: workspace }}
          search={{ create: true, report: report.id }}
          data-report-start-fix-task
          className="inline-flex h-7 items-center gap-1.5 rounded-md border border-border bg-card px-2.5 text-xs font-medium shadow-xs hover:border-edge hover:bg-raised/60 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary [&_svg]:size-3.5"
        >
          <Wrench aria-hidden />
          Start a fix task
        </Link>
      </div>
      {form === 'link' && <LinkForm report={report} act={act} target={target} onDone={() => setForm(null)} />}
      {form === 'close' && <CloseForm report={report} act={act} target={target} onDone={() => setForm(null)} />}
      {form === null && <ActStatus state={act.state} target={target} />}
    </div>
  )
}

function CloseForm({
  report,
  act,
  target,
  onDone,
}: {
  report: FeedbackReport
  act: ReturnType<typeof useReportAct>
  target: string
  onDone: () => void
}) {
  const { workspace } = useWorkspaceSelection()
  const [reason, setReason] = useState('')
  const [tried, setTried] = useState(false)
  const id = useId()
  const length = closeReasonLength(reason)
  const problem = closeReasonProblem(reason)
  const shownProblem = length > CLOSE_REASON_MAX || (tried && problem) ? problem : ''
  const sending = act.state.phase === 'sending' && act.state.act === 'close'
  return (
    <form
      aria-label="Close the report"
      className="space-y-2"
      onSubmit={(event) => {
        event.preventDefault()
        setTried(true)
        if (problem || act.pending) return
        void act
          .run('close', target, () => closeFeedbackReport(workspace, report.id, reason))
          .then((done) => {
            if (done) onDone()
          })
      }}
    >
      <label htmlFor={`${id}-reason`} className="block text-xs font-medium text-muted">
        Reason to close the report
      </label>
      <Textarea
        id={`${id}-reason`}
        value={reason}
        onChange={(event) => setReason(event.target.value)}
        aria-describedby={`${id}-rule`}
        aria-invalid={shownProblem ? true : undefined}
        className="min-h-16 text-xs"
      />
      <p id={`${id}-rule`} className="text-xs text-faint">
        Write 1 to {CLOSE_REASON_MAX} characters. <span className="tabular-nums">{length}</span> of {CLOSE_REASON_MAX}{' '}
        used. A closed report leaves the open lists and the Inbox.
      </p>
      {shownProblem && (
        <p role="alert" className="text-xs text-failure">
          {shownProblem}
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" type="submit" disabled={act.pending || length > CLOSE_REASON_MAX}>
          {sending ? 'Closing…' : 'Close report'}
        </Button>
        <Button size="sm" type="button" variant="ghost" disabled={act.pending} onClick={onDone}>
          Cancel
        </Button>
      </div>
      <ActStatus state={act.state} target={target} />
    </form>
  )
}

function LinkForm({
  report,
  act,
  target,
  onDone,
}: {
  report: FeedbackReport
  act: ReturnType<typeof useReportAct>
  target: string
  onDone: () => void
}) {
  const { workspace } = useWorkspaceSelection()
  const [kind, setKind] = useState<'task' | 'crate'>('task')
  const [query, setQuery] = useState('')
  const deferred = useDeferredValue(query)
  const id = useId()
  const tasks = useQuery({
    queryKey: ['tasks', workspace, 'merged-search', deferred.trim()],
    queryFn: (context) => withQuerySignal(context, (signal) => fetchMergedTasks(workspace, deferred, signal)),
    enabled: Boolean(workspace) && kind === 'task',
    retry: false,
  })
  const crates = useQuery({
    queryKey: ['crates', workspace, 'list', 'all'],
    queryFn: () => fetchCrates(workspace, 'all'),
    enabled: Boolean(workspace) && kind === 'crate',
    retry: false,
  })
  const needle = deferred.trim().toLowerCase()
  const options =
    kind === 'task'
      ? (tasks.data ?? []).map((task) => ({ id: task.id, name: task.title }))
      : (crates.data ?? [])
          .filter((crate) => !needle || crate.name.toLowerCase().includes(needle) || crate.id.includes(needle))
          .slice(0, 20)
          .map((crate) => ({ id: crate.id, name: crate.name }))
  const read = kind === 'task' ? tasks : crates
  const linked = new Set(report.links.map((link) => link.task?.id ?? link.crate?.id ?? ''))
  return (
    <section aria-label="Link a task or crate" className="space-y-2">
      <fieldset className="flex flex-wrap gap-3 text-xs">
        <legend className="mb-1 text-xs font-medium text-muted">Link this report to</legend>
        <label className="inline-flex items-center gap-1.5">
          <input type="radio" name={`${id}-kind`} checked={kind === 'task'} onChange={() => setKind('task')} />A merged
          task
        </label>
        <label className="inline-flex items-center gap-1.5">
          <input type="radio" name={`${id}-kind`} checked={kind === 'crate'} onChange={() => setKind('crate')} />A crate
        </label>
      </fieldset>
      <Input
        aria-label={kind === 'task' ? 'Search merged tasks' : 'Search crates'}
        aria-describedby={`${id}-status`}
        value={query}
        onChange={(event) => setQuery(event.target.value)}
        placeholder={kind === 'task' ? 'Search merged tasks by title or ID' : 'Search crates by name'}
        className="text-xs"
      />
      <p id={`${id}-status`} aria-live="polite" className="text-xs text-faint">
        {read.isPending
          ? 'Loading…'
          : read.error != null
            ? 'The list did not load. Try again.'
            : options.length === 0
              ? kind === 'task'
                ? 'No merged task matches.'
                : 'No crate matches.'
              : kind === 'task'
                ? `${options.length === 20 ? 'The first 20' : options.length} merged ${options.length === 1 ? 'task matches' : 'tasks match'}. You can link only a merged task.`
                : `${options.length} ${options.length === 1 ? 'crate matches' : 'crates match'}.`}
      </p>
      <ul className="max-h-48 space-y-1 overflow-y-auto">
        {options.map((option) => {
          const sending = act.state.phase === 'sending' && act.state.target === target && act.state.act === 'link'
          return (
            <li
              key={option.id}
              className="flex min-w-0 items-center gap-2 rounded px-1.5 py-1 text-xs hover:bg-raised/50"
            >
              {kind === 'task' ? (
                <span className="min-w-0 flex-1 [overflow-wrap:anywhere]">
                  <Untrusted>{option.name}</Untrusted> <span className="font-mono text-faint">{option.id}</span>
                </span>
              ) : (
                <span className="inline-flex min-w-0 flex-1 items-center gap-1 [overflow-wrap:anywhere]">
                  <Package aria-hidden className="size-3 shrink-0 text-faint" />
                  <Untrusted>{option.name}</Untrusted>
                </span>
              )}
              {linked.has(option.id) && <span className="shrink-0 text-faint">Linked</span>}
              <Button
                size="sm"
                variant="secondary"
                className="shrink-0"
                aria-label={`Link ${option.name}`}
                disabled={act.pending}
                onClick={() =>
                  void act
                    .run('link', target, () =>
                      linkFeedbackReport(
                        workspace,
                        report.id,
                        kind === 'task' ? { task_id: option.id } : { crate_id: option.id },
                      ),
                    )
                    .then((done) => done && onDone())
                }
              >
                {sending ? 'Linking…' : 'Link'}
              </Button>
            </li>
          )
        })}
      </ul>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" type="button" variant="ghost" disabled={act.pending} onClick={onDone}>
          Cancel
        </Button>
      </div>
      <ActStatus state={act.state} target={target} />
    </section>
  )
}
