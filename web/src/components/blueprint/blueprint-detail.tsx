import { Link } from '@tanstack/react-router'
import { Blocks, ChevronRight, ExternalLink, FileText } from 'lucide-react'
import { parseProvenance } from '../../lib/activity'
import { childRollup, childStateLabel, deliveryLabel, deliveryTone } from '../../lib/blueprint'
import type { ActivityItem, BlueprintChild, BlueprintView } from '../../lib/types'
import { absoluteTime } from '../../lib/utils'
import { AttachmentsCard } from '../task/attachments-card'
import { SpecCard } from '../task/spec-card'
import { Timeline } from '../task/timeline'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { MarkdownProse } from '../ui/markdown-prose'

// The blueprint anchor detail, rendered only at its canonical
// route. An anchor is an intent artifact: no order will ever be claimed for it,
// nothing will land on its branch, and it will never move through a stage
// column. So it leads with the approved blueprint, reports delivery in
// blueprint vocabulary, and shows none of the checkout, assigned-branch, or
// hold or lifecycle affordances that would imply otherwise.
export function BlueprintDetail({ view, item }: { view: BlueprintView; item: ActivityItem }) {
  return (
    <>
      <div className="shrink-0 border-b border-border px-6 py-4">
        <BlueprintHeader view={view} />
      </div>
      <div className="grid grid-cols-1 lg:grid-cols-2">
        <section
          aria-label="Blueprint"
          className="space-y-4 border-b border-border px-6 py-4 lg:border-b-0 lg:border-r"
        >
          <BlueprintSpec view={view} />
          <BlueprintChildren view={view} />
          <OriginalRequest body={view.task.body} />
        </section>
        <section aria-label="Delivery activity" className="space-y-4 px-6 py-4">
          <AttachmentsCard attachments={view.artifacts} title="Lineage and artifacts" />
          <Timeline item={item} executionActions={false} />
        </section>
      </div>
    </>
  )
}

function BlueprintHeader({ view }: { view: BlueprintView }) {
  const provenance = parseProvenance(view.task.source)
  const specVersion = view.governing_version || view.spec?.version

  return (
    <div>
      <div className="mb-2 flex flex-wrap items-center gap-1.5">
        <Badge variant="accent" className="gap-1">
          <Blocks aria-hidden="true" /> Historical blueprint
        </Badge>
        <Badge variant={deliveryTone(view.delivery)}>{deliveryLabel(view.delivery)}</Badge>
        {specVersion ? <Badge variant="mono">Blueprint v{specVersion}</Badge> : null}
        {view.task.class && <Badge>{view.task.class}</Badge>}
        <Badge variant="accent">{provenance.label}</Badge>
      </div>
      <h1 className="text-xl font-semibold leading-snug tracking-tight">{view.task.title}</h1>

      <dl className="mt-4 grid grid-cols-2 gap-x-8 gap-y-2.5 text-xs lg:grid-cols-4">
        <Fact label="Repo" value={view.task.repo} />
        <Fact label="Delivery" value={childRollup(view.delivery)} />
        <Fact label="Planned" value={absoluteTime(view.task.created_at)} />
        <Fact
          label="Serves"
          value={
            view.serves.length === 0 ? (
              <span className="text-faint">No historical requirement link</span>
            ) : (
              <span className="flex flex-wrap gap-x-2 gap-y-1">
                {view.serves.map((requirement) => (
                  <Link
                    key={requirement.id}
                    to="/requirements"
                    search={{ requirement: requirement.id }}
                    className="text-primary hover:underline"
                  >
                    {requirement.title}
                  </Link>
                ))}
              </span>
            )
          }
        />
        {view.task.github?.issue_url && (
          <Fact
            label="GitHub issue"
            value={
              <a
                href={view.task.github.issue_url}
                target="_blank"
                rel="noreferrer"
                className="inline-flex max-w-full items-center gap-1 text-primary hover:underline"
              >
                <span className="truncate">
                  {view.task.github.repository}#{view.task.github.issue_number}
                </span>
                <ExternalLink className="size-3 shrink-0" />
              </a>
            }
          />
        )}
      </dl>
    </div>
  )
}

// The approved blueprint leads: it is what the anchor *is*, and the child
// list below is only its delivery.
function BlueprintSpec({ view }: { view: BlueprintView }) {
  if (!view.spec) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Blueprint</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted">This blueprint has no recorded spec version.</p>
        </CardContent>
      </Card>
    )
  }
  return (
    <SpecCard
      key={`${view.spec.task_id}-${view.spec.version}`}
      spec={view.spec}
      collapsible={false}
      routeVariant="full"
    />
  )
}

// Children in dependency order, so a task never renders above something it
// waits on. Every state renders through taskStateLabels — never a raw state.
// A child *is* work, so its link stays on the ordinary task route.
function BlueprintChildren({ view }: { view: BlueprintView }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Delivery</CardTitle>
        <span className="text-[11px] text-faint">{childRollup(view.delivery)}</span>
      </CardHeader>
      <CardContent className="space-y-1.5">
        {view.children.length === 0 && <p className="text-sm text-muted">No child tasks have been materialized yet.</p>}
        <ol aria-label="Blueprint tasks" className="space-y-1.5">
          {view.children.map((child) => (
            <li key={child.id} className="flex items-center gap-2 text-xs">
              {child.origin_sub_id && <Badge variant="mono">{child.origin_sub_id}</Badge>}
              <Link
                to="/tasks/$taskId/full"
                params={{ taskId: child.id }}
                className="min-w-0 flex-1 truncate text-primary hover:underline"
              >
                {child.title || child.summary || child.id}
              </Link>
              <ChildDependencyHint child={child} />
              <span className="shrink-0 text-faint">{childStateLabel(child.state)}</span>
            </li>
          ))}
        </ol>
      </CardContent>
    </Card>
  )
}

// The intake body is provenance, not the headline. What the
// anchor promises is the approved blueprint above; the request that started it
// stays one disclosure away instead of leading with a wall of markdown.
function OriginalRequest({ body }: { body?: string }) {
  if (!body?.trim()) return null
  return (
    <details className="group/request rounded-lg border border-border bg-card">
      <summary className="flex cursor-pointer list-none items-center gap-1.5 px-4 py-3 text-sm font-medium">
        <ChevronRight className="size-3.5 shrink-0 text-faint transition-transform group-open/request:rotate-90" />
        Original request
      </summary>
      <div className="border-t border-border px-4 py-3">
        <MarkdownProse className="text-sm text-muted">{body}</MarkdownProse>
      </div>
    </details>
  )
}

function ChildDependencyHint({ child }: { child: BlueprintChild }) {
  if (!child.depends_on?.length) return null
  const explanation = `Starts after ${child.depends_on.join(', ')}`
  return (
    <span role="img" className="group/dependency relative inline-flex shrink-0" aria-label={explanation}>
      <Badge variant="mono">after {child.depends_on.join(', ')}</Badge>
      <span
        role="tooltip"
        className="pointer-events-none absolute bottom-full right-0 z-10 mb-1.5 w-52 rounded-md bg-foreground px-2.5 py-1.5 text-[11px] leading-4 text-background opacity-0 shadow-md transition-opacity group-hover/dependency:opacity-100"
      >
        {explanation}
      </span>
    </span>
  )
}

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-faint">{label}</dt>
      <dd className="mt-0.5 truncate text-foreground/85">{value}</dd>
    </div>
  )
}

// Rendered while the blueprint projection has not yet returned the anchor the
// canonical route was opened for.
export function BlueprintDetailFallback() {
  return (
    <p className="rounded-md border border-border bg-surface p-3 text-sm text-muted">
      <FileText className="mr-1.5 inline size-3.5" aria-hidden="true" />
      Loading this blueprint…
    </p>
  )
}
