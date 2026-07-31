import { Link } from '@tanstack/react-router'
import { Blocks, ExternalLink, FileText } from 'lucide-react'
import { parseProvenance } from '../../lib/activity'
import { childRollup, childStateLabel, deliveryLabel, deliveryTone } from '../../lib/blueprint'
import { relatedTaskRoute, type TaskRouteVariant } from '../../lib/task-route'
import type { ActivityItem, BlueprintChild, BlueprintView } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { AttachmentsCard } from '../task/attachments-card'
import { SpecCard } from '../task/spec-card'
import { CancelControl } from '../task/task-header'
import { Timeline } from '../task/timeline'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { MarkdownProse } from '../ui/markdown-prose'

// The blueprint anchor detail (spec §21.49). An anchor is an intent artifact:
// no order will ever be claimed for it, nothing will land on its branch, and
// it will never move through a stage column. So it leads with the approved
// blueprint, reports delivery in blueprint vocabulary, and shows none of the
// checkout, assigned-branch, or hold affordances that would imply otherwise.
// Cancel stays — that is lifecycle, and it behaves exactly as it does today.
export function BlueprintDetail({
  view,
  item,
  variant,
}: {
  view: BlueprintView
  item: ActivityItem
  variant: TaskRouteVariant
}) {
  if (variant === 'full') {
    return (
      <>
        <div className="shrink-0 border-b border-border px-6 py-4">
          <BlueprintHeader view={view} item={item} variant="full" />
        </div>
        <div className="grid grid-cols-1 lg:grid-cols-2">
          <section aria-label="Blueprint" className="space-y-4 border-b border-border px-6 py-4 lg:border-b-0 lg:border-r">
            <BlueprintSpec view={view} variant="full" />
            <BlueprintChildren view={view} variant="full" />
          </section>
          <section aria-label="Delivery activity" className="space-y-4 px-6 py-4">
            <AttachmentsCard attachments={view.artifacts} title="Lineage and artifacts" />
            <Timeline item={item} executionActions={false} />
          </section>
        </div>
      </>
    )
  }
  return (
    <div className="space-y-4">
      <BlueprintHeader view={view} item={item} variant="sheet" />
      <BlueprintSpec view={view} variant="sheet" />
      <BlueprintChildren view={view} variant="sheet" />
      <AttachmentsCard attachments={view.artifacts} title="Lineage and artifacts" />
      <Timeline item={item} executionActions={false} />
    </div>
  )
}

function BlueprintHeader({
  view,
  item,
  variant,
}: {
  view: BlueprintView
  item: ActivityItem
  variant: TaskRouteVariant
}) {
  const provenance = parseProvenance(view.task.source)
  const Heading = variant === 'full' ? 'h1' : 'h2'
  const specVersion = view.governing_version || view.spec?.version

  return (
    <div>
      <div className="mb-2 flex flex-wrap items-center gap-1.5">
        <Badge variant="accent" className="gap-1"><Blocks aria-hidden="true" /> Blueprint</Badge>
        <Badge variant={deliveryTone(view.delivery)}>{deliveryLabel(view.delivery)}</Badge>
        <CancelControl item={item} />
        {view.task.class && <Badge>{view.task.class}</Badge>}
        <Badge variant="accent">{provenance.label}</Badge>
      </div>
      <Heading className={cn('font-semibold leading-snug tracking-tight', variant === 'full' ? 'text-xl' : 'text-base')}>
        {view.task.title}
      </Heading>
      {view.task.body && (
        <div className="mt-2 max-w-3xl">
          <MarkdownProse className="text-sm text-muted">{view.task.body}</MarkdownProse>
        </div>
      )}

      <dl className={cn('mt-4 grid gap-x-8 gap-y-2.5 text-xs', variant === 'full' ? 'grid-cols-2 lg:grid-cols-4' : 'grid-cols-2')}>
        <Fact label="Repo" value={view.task.repo} />
        {specVersion ? <Fact label="Governing blueprint" value={`v${specVersion}`} /> : null}
        <Fact label="Delivery" value={childRollup(view.delivery)} />
        <Fact label="Planned" value={absoluteTime(view.task.created_at)} />
        <Fact
          label="Serves"
          value={
            view.serves.length === 0 ? (
              <span className="text-faint">No requirement linked yet</span>
            ) : (
              <span className="flex flex-wrap gap-x-2 gap-y-1">
                {view.serves.map((requirement) => (
                  <Link key={requirement.id} to="/requirements" className="text-primary hover:underline">
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
              <a href={view.task.github.issue_url} target="_blank" rel="noreferrer" className="inline-flex max-w-full items-center gap-1 text-primary hover:underline">
                <span className="truncate">{view.task.github.repository}#{view.task.github.issue_number}</span>
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
function BlueprintSpec({ view, variant }: { view: BlueprintView; variant: TaskRouteVariant }) {
  if (!view.spec) {
    return (
      <Card>
        <CardHeader><CardTitle>Blueprint</CardTitle></CardHeader>
        <CardContent><p className="text-sm text-muted">This blueprint has no recorded spec version.</p></CardContent>
      </Card>
    )
  }
  return (
    <SpecCard
      key={`${view.spec.task_id}-${view.spec.version}`}
      spec={view.spec}
      collapsible={variant === 'sheet'}
      overflowExpandable={variant === 'sheet'}
      routeVariant={variant}
    />
  )
}

// Children in dependency order, so a task never renders above something it
// waits on. Every state renders through taskStateLabels — never a raw state.
function BlueprintChildren({ view, variant }: { view: BlueprintView; variant: TaskRouteVariant }) {
  const relatedRoute = relatedTaskRoute(variant)
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
                to={relatedRoute}
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

function ChildDependencyHint({ child }: { child: BlueprintChild }) {
  if (!child.depends_on?.length) return null
  const explanation = `Starts after ${child.depends_on.join(', ')}`
  return (
    <span className="group/dependency relative inline-flex shrink-0" aria-label={explanation}>
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

// Rendered when a child's parent reference resolves to an anchor the
// blueprint projection has not returned yet.
export function BlueprintDetailFallback() {
  return (
    <p className="rounded-md border border-border bg-surface p-3 text-sm text-muted">
      <FileText className="mr-1.5 inline size-3.5" aria-hidden="true" />
      Loading this blueprint…
    </p>
  )
}
