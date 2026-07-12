import { ExternalLink, GitBranch, GitPullRequest } from 'lucide-react'
import { parseProvenance, pullRequestURL } from '../../lib/activity'
import { stageLabels } from '../../lib/contracts'
import type { ActivityItem } from '../../lib/types'
import { absoluteTime, usd } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { CopyButton } from '../ui/copy-button'
import { Progress } from '../ui/progress'

// The task-header facts (spec §13.3): budget consumed vs. allocated, the
// verification badge fed by the §4.1 acceptance block, the PR deep link,
// and provenance — everything an operator absorbs passively during review.
export function SummaryRail({ item }: { item: ActivityItem }) {
  const totalCost = item.jobs.reduce((sum, job) => sum + job.cost_usd, 0)
  const totalBudget = item.jobs.reduce((sum, job) => sum + job.budget_usd, 0)
  const prURL = pullRequestURL(item.events)
  const provenance = parseProvenance(item.task.source)
  const criteria = item.spec?.acceptance ?? []
  const humanChecks = criteria.filter((criterion) => criterion.verify === 'human').length

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <CardTitle>Budget</CardTitle>
          <span className="font-mono text-xs tabular-nums text-foreground">
            {usd(totalCost)} <span className="text-faint">/ {usd(totalBudget)}</span>
          </span>
        </CardHeader>
        <CardContent>
          <Progress value={totalBudget > 0 ? (totalCost / totalBudget) * 100 : 0} />
          <ul className="mt-3 space-y-1.5">
            {item.jobs.map((job) => (
              <li key={job.id} className="flex items-baseline justify-between gap-2 text-xs">
                <span className="text-muted">{stageLabels[job.stage] ?? job.stage}</span>
                <span className="font-mono tabular-nums text-faint">{usd(job.cost_usd)}</span>
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Verification</CardTitle>
          {item.spec ? (
            <Badge variant={item.spec.approved ? 'positive' : 'default'}>
              {item.spec.acceptance_count} criteria
            </Badge>
          ) : (
            <Badge>No spec yet</Badge>
          )}
        </CardHeader>
        {item.spec && (
          <CardContent className="text-xs leading-5 text-muted">
            Spec v{item.spec.version} {item.spec.approved ? 'approved' : 'awaiting approval'}
            {humanChecks > 0 && (
              <>
                {' · '}
                {humanChecks} human check{humanChecks === 1 ? '' : 's'} at the gate
              </>
            )}
          </CardContent>
        )}
      </Card>

      {prURL && (
        <Card>
          <CardHeader>
            <CardTitle>Pull request</CardTitle>
          </CardHeader>
          <CardContent>
            <a
              href={prURL}
              target="_blank"
              rel="noreferrer"
              className="inline-flex max-w-full items-center gap-1.5 text-sm text-primary hover:underline"
            >
              <GitPullRequest className="size-4 shrink-0" />
              <span className="truncate">{prURL.replace(/^https:\/\/github\.com\//, '')}</span>
              <ExternalLink className="size-3.5 shrink-0" />
            </a>
            <p className="mt-1.5 text-[11px] leading-5 text-faint">
              Diff review happens on GitHub; review comments convert to redirects (spec §9).
            </p>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>Task</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="space-y-2 text-xs">
            <Fact label="Repo" value={item.task.repo} />
            <Fact
              label="Branch"
              value={
                <span className="inline-flex max-w-full items-center gap-1 font-mono">
                  <GitBranch className="size-3 shrink-0 text-faint" />
                  <span className="truncate">{item.task.branch}</span>
                </span>
              }
            />
            <Fact label="Base" value={<span className="font-mono">{item.task.base_branch}</span>} />
            <Fact
              label="Source"
              value={
                provenance.href ? (
                  <a href={provenance.href} target="_blank" rel="noreferrer" className="text-primary hover:underline">
                    {provenance.label}
                  </a>
                ) : (
                  provenance.label
                )
              }
            />
            <Fact label="Level" value={item.task.level || 'L2'} />
            {item.task.class && <Fact label="Class" value={item.task.class} />}
            <Fact label="Created" value={absoluteTime(item.task.created_at)} />
          </dl>
          <div className="mt-3 flex items-center gap-1 rounded-md border border-border bg-background px-2.5 py-1">
            <code className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted">{item.checkout_command}</code>
            <CopyButton value={item.checkout_command} label="Copy checkout command" />
          </div>
        </CardContent>
      </Card>
    </div>
  )
}

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="shrink-0 text-faint">{label}</dt>
      <dd className="min-w-0 truncate text-right text-foreground/85">{value}</dd>
    </div>
  )
}
