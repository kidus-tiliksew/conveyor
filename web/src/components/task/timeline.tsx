import { AlertTriangle, Cpu, ExternalLink, UserRound } from 'lucide-react'
import claudeIcon from '@lobehub/icons-static-svg/icons/claude-color.svg?raw'
import geminiIcon from '@lobehub/icons-static-svg/icons/gemini-color.svg?raw'
import openaiIcon from '@lobehub/icons-static-svg/icons/openai.svg?raw'
import { buildTimeline, type TimelineEntry } from '../../lib/activity'
import { defaultReasonCode, stageLabels } from '../../lib/contracts'
import type { ActivityItem, InterventionAction, Job } from '../../lib/types'
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

// Audit entries keep the wire action; the display label matches the gate UI
// ("redirect" surfaces as requesting changes).
const interventionLabels: Record<InterventionAction, string> = {
  approve: 'Approved',
  reject: 'Rejected',
  redirect: 'Requested changes',
  pull_to_local: 'Pulled to local',
}

function keyFor(entry: TimelineEntry) {
  if (entry.type === 'job') return `job-${entry.job.id}`
  if (entry.type === 'intervention') return `intervention-${entry.intervention.id}`
  return entry.key
}

const orderDots: Record<Extract<TimelineEntry, { type: 'order' }>['tone'], string> = {
  waiting: 'bg-edge',
  active: 'animate-pulse bg-primary',
  alarm: 'bg-attention-dot',
}

function TimelineRow({ entry }: { entry: TimelineEntry }) {
  if (entry.type === 'job') return <JobEntry job={entry.job} summary={entry.summary} model={entry.model} />
  if (entry.type === 'order') {
    return (
      <li className="relative pl-7">
        <TimelineDot className={orderDots[entry.tone]} />
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 px-1 py-1.5">
          <span className={cn('text-sm font-medium', entry.tone === 'alarm' ? 'text-attention' : 'text-foreground/90')}>
            {entry.title}
          </span>
          {entry.detail && <span className="text-xs text-muted">{entry.detail}</span>}
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(entry.at)}</time>
        </div>
      </li>
    )
  }
  if (entry.type === 'intervention') {
    const { intervention } = entry
    return (
      <li className="relative pl-7">
        <TimelineDot className="bg-foreground" />
        <div className="rounded-lg border border-edge bg-raised/40 px-4 py-3">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <UserRound className="size-3.5 text-muted" />
            <strong className="font-semibold">{interventionLabels[intervention.action] ?? intervention.action.replaceAll('_', ' ')}</strong>
            {intervention.reason_code !== defaultReasonCode[intervention.action] && (
              <Badge variant="mono">{intervention.reason_code}</Badge>
            )}
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

// The job footer keeps the operator-facing facts — duration and model — and
// tucks the audit numbers (tokens, cost) behind a hover on the model chip.
// Harness, auth mode, confinement, and actor plumbing stay in the API.
function JobEntry({ job, summary, model }: { job: Job; summary: string; model: string }) {
	if (!job.started_at) return null
	const running = job.state === 'running'
  return (
    <li className="relative pl-7">
      <TimelineDot
        className={cn(
          'bg-edge',
          job.state === 'done' && 'bg-positive',
          job.state === 'failed' && 'bg-failure',
          running && 'animate-pulse bg-primary',
        )}
      />
      <article className="rounded-lg border border-border bg-card">
        <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-2.5">
          <span className="text-xs font-semibold uppercase tracking-[0.1em] text-foreground">
            {stageLabels[job.stage] ?? job.stage}
          </span>
          {job.state === 'failed' && <Badge variant="failure">Failed</Badge>}
          {running && <Badge variant="accent">Running</Badge>}
          <time className="ml-auto text-[11px] text-faint">{absoluteTime(job.started_at)}</time>
        </div>
        <p className="whitespace-pre-line px-4 py-3 text-sm leading-6 text-foreground/85">{summary}</p>
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 border-t border-border px-4 py-2 font-mono text-[11px] tabular-nums text-muted">
          <span>{duration(job.started_at, job.ended_at)}</span>
          <ModelChip model={model} costUSD={job.cost_usd} tokensIn={job.tokens_in} tokensOut={job.tokens_out} />
        </div>
      </article>
    </li>
  )
}

// Provider logo keyed off the model name (bundled SVGs, no network fetch).
function providerLogo(model: string): { svg: string; className?: string } | undefined {
  const name = model.toLowerCase()
  if (/^(gpt|o\d|codex|davinci)/.test(name) || name.includes('openai')) return { svg: openaiIcon, className: 'text-[#0a0a0a]' }
  if (/claude|fable|opus|sonnet|haiku|anthropic/.test(name)) return { svg: claudeIcon }
  if (/gemini|google/.test(name)) return { svg: geminiIcon }
  return undefined
}

function ModelChip({ model, costUSD, tokensIn, tokensOut }: { model: string; costUSD: number; tokensIn: number; tokensOut: number }) {
  const logo = providerLogo(model)
  const usage = [
    tokensIn + tokensOut > 0 ? `${compactTokens(tokensIn)} in / ${compactTokens(tokensOut)} out` : undefined,
    costUSD > 0 ? usd(costUSD) : undefined,
  ]
    .filter(Boolean)
    .join(' · ')
  return (
    <span className="group/model relative inline-flex cursor-default items-center gap-1.5">
      {logo ? (
        <span
          aria-hidden
          className={cn('inline-flex shrink-0 [&_svg]:size-3.5', logo.className)}
          dangerouslySetInnerHTML={{ __html: logo.svg }}
        />
      ) : (
        <Cpu aria-hidden className="size-3.5 shrink-0 text-faint" />
      )}
      <span>{model}</span>
      {usage && (
        <span
          role="tooltip"
          className="pointer-events-none absolute bottom-full right-0 z-10 mb-1.5 whitespace-nowrap rounded-md bg-foreground px-2 py-1 font-mono text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity duration-150 after:absolute after:right-3 after:top-full after:border-4 after:border-transparent after:border-t-foreground group-hover/model:opacity-100"
        >
          {usage}
        </span>
      )}
    </span>
  )
}

function TimelineDot({ className }: { className?: string }) {
  return <span className={cn('absolute left-0 top-3 size-[15px] rounded-full border-4 border-background', className)} />
}
