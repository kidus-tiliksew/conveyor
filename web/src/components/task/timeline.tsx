import { AlertTriangle, ExternalLink, UserRound } from 'lucide-react'
import { buildTimeline, type TimelineEntry } from '../../lib/activity'
import { stageLabels } from '../../lib/contracts'
import type { ActivityItem, Job } from '../../lib/types'
import { absoluteTime, cn, compactTokens, duration, usd } from '../../lib/utils'
import { Badge } from '../ui/badge'

// The costed event timeline (spec §13.3 element 2): the audit log rendered
// as a story — one entry per stage execution, with cost, duration, and the
// harness/model/auth mode that ran it, interleaved with pipeline incidents
// and every recorded decision.
export function Timeline({ item }: { item: ActivityItem }) {
  const entries = buildTimeline(item)
  return (
    <section aria-label="Costed event timeline">
      <div className="mb-4 flex items-baseline justify-between">
        <h2 className="text-sm font-semibold tracking-tight">Event history</h2>
        <span className="font-mono text-[11px] text-faint">{item.events.length} audit events</span>
      </div>
      <ol className="relative space-y-4 before:absolute before:bottom-4 before:left-[7px] before:top-4 before:w-px before:bg-border">
        {entries.map((entry) => (
          <TimelineRow key={keyFor(entry)} entry={entry} />
        ))}
        {entries.length === 0 && (
          <li className="pl-7 text-sm text-muted">Waiting for the first job to start.</li>
        )}
      </ol>
    </section>
  )
}

function keyFor(entry: TimelineEntry) {
  if (entry.type === 'job') return `job-${entry.job.id}`
  if (entry.type === 'intervention') return `intervention-${entry.intervention.id}`
  return entry.key
}

function TimelineRow({ entry }: { entry: TimelineEntry }) {
  if (entry.type === 'job') return <JobEntry job={entry.job} summary={entry.summary} />
  if (entry.type === 'intervention') {
    const { intervention } = entry
    return (
      <li className="relative pl-7">
        <TimelineDot className="bg-foreground" />
        <div className="rounded-lg border border-edge bg-raised/40 px-4 py-3">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <UserRound className="size-3.5 text-muted" />
            <strong className="font-semibold capitalize">{intervention.action.replaceAll('_', ' ')}</strong>
            <Badge variant="mono">{intervention.reason_code}</Badge>
            <span className="text-xs text-faint">
              {intervention.actor_id} · {intervention.actor_role}
            </span>
            <time className="ml-auto text-[11px] text-faint">{absoluteTime(intervention.at)}</time>
          </div>
          {intervention.comment && <p className="mt-2 text-sm leading-6 text-foreground/85">{intervention.comment}</p>}
        </div>
      </li>
    )
  }
  return (
    <li className="relative pl-7">
      <TimelineDot className={entry.alarm ? 'bg-attention-dot' : 'bg-edge'} />
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 px-1 py-1.5">
        {entry.alarm && <AlertTriangle className="size-3.5 self-center text-attention" />}
        <span className={cn('text-sm', entry.alarm ? 'font-medium text-attention' : 'text-foreground/90')}>
          {entry.href ? (
            <a href={entry.href} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
              {entry.title}
              <ExternalLink className="size-3.5" />
            </a>
          ) : (
            entry.title
          )}
        </span>
        {entry.detail && !entry.href && <span className="text-xs text-muted">{entry.detail}</span>}
        <time className="ml-auto text-[11px] text-faint">{absoluteTime(entry.at)}</time>
      </div>
    </li>
  )
}

function JobEntry({ job, summary }: { job: Job; summary: string }) {
  const running = job.state === 'running' || job.state === 'booting'
  return (
    <li className="relative pl-7">
      <TimelineDot
        className={cn(
          'bg-edge',
          job.state === 'done' && 'bg-positive',
          job.state === 'paused' && 'bg-attention-dot',
          (job.state === 'failed' || job.state === 'sandbox_boot_failed') && 'bg-failure',
          running && 'animate-pulse bg-primary',
        )}
      />
      <article className="rounded-lg border border-border bg-card">
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <span className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">
            {stageLabels[job.stage] ?? job.stage}
          </span>
          <Badge
            variant={
              job.state === 'done' ? 'positive'
              : job.state === 'failed' || job.state === 'sandbox_boot_failed' ? 'failure'
              : 'default'
            }
          >
            {job.state.replaceAll('_', ' ')}
          </Badge>
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(job.started_at)}</time>
        </div>
        <p className="whitespace-pre-line px-4 py-3 text-sm leading-6 text-foreground/85">{summary}</p>
        {job.boot_diagnostics && <BootDiagnostics job={job} />}
        <div className="flex flex-wrap gap-x-4 gap-y-1 border-t border-border px-4 py-2.5 font-mono text-[11px] tabular-nums text-muted">
          <span>{duration(job.started_at, job.ended_at)}</span>
          <span>{usd(job.cost_usd)}</span>
          <span>
            {compactTokens(job.tokens_in)} in / {compactTokens(job.tokens_out)} out
          </span>
          <span className="text-faint">
            {[job.harness, job.model_tier, job.auth_mode].filter(Boolean).join(' / ')}
          </span>
          <span className="text-faint">{job.confinement}</span>
        </div>
      </article>
    </li>
  )
}

// Structured sandbox boot failure diagnostics (spec §6.2).
function BootDiagnostics({ job }: { job: Job }) {
  const diag = job.boot_diagnostics
  if (!diag) return null
  const lines = [
    diag.validation_error && `Validation: ${diag.validation_error}`,
    diag.runtime_error && `Runtime: ${diag.runtime_error}`,
    diag.missing_env_vars?.length && `Missing env vars: ${diag.missing_env_vars.join(', ')}`,
  ].filter(Boolean) as string[]
  return (
    <div className="mx-4 mb-3 rounded-md bg-failure-soft px-3 py-2">
      <p className="text-xs font-semibold text-failure">Sandbox boot diagnostics</p>
      {lines.map((line) => (
        <p key={line} className="mt-1 text-xs leading-5 text-failure/90">
          {line}
        </p>
      ))}
      {diag.image_build_log && (
        <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap font-mono text-[11px] leading-5 text-failure/80">
          {diag.image_build_log}
        </pre>
      )}
    </div>
  )
}

function TimelineDot({ className }: { className?: string }) {
  return <span className={cn('absolute left-0 top-3 size-[15px] rounded-full border-4 border-background', className)} />
}
