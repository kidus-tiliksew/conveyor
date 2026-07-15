import { useMemo, useState } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { ChevronRight, FlaskConical, Globe, MousePointerClick, Square } from 'lucide-react'
import type { AcceptanceCriterion, SpecVersion } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'

// Machine-owned fenced blocks (§4.1) are stripped from the prose and
// rendered structurally by the acceptance checklist below.
const machineBlock = /```conveyor:(?:acceptance|decomposition)\n[\s\S]*?```\n?/g

const verifyIcons: Record<AcceptanceCriterion['verify'], typeof FlaskConical> = {
  test: FlaskConical,
  playwright: Globe,
  'computer-use': MousePointerClick,
  human: Square,
}

// The spec review card (spec §13.3 element 3): rendered markdown plus the
// acceptance-criteria checklist. Human-verify criteria surface as explicit
// checkboxes rather than being pretend-verified (§4.1 rule 2).
export function SpecCard({ spec, collapsible = true }: { spec: SpecVersion; collapsible?: boolean }) {
  const [collapsed, setCollapsed] = useState(false)
  const expanded = !collapsible || !collapsed
  const prose = useMemo(() => spec.content.replace(machineBlock, ''), [spec.content])
  const criteria = spec.acceptance ?? []

  return (
    <Card>
      <CardHeader className="items-center">
        {collapsible ? (
          <button
            type="button"
            onClick={() => setCollapsed((value) => !value)}
            aria-expanded={expanded}
            className="flex items-center gap-2 text-left"
          >
            <ChevronRight className={cn('size-3.5 text-faint transition-transform', expanded && 'rotate-90')} />
            <CardTitle>Specification</CardTitle>
            <Badge variant="mono">v{spec.version}</Badge>
          </button>
        ) : (
          <div className="flex items-center gap-2">
            <CardTitle>Specification</CardTitle>
            <Badge variant="mono">v{spec.version}</Badge>
          </div>
        )}
        <div className="flex items-center gap-2">
          <span className="text-[11px] text-faint">
            {spec.approved && spec.approved_at ? `approved ${absoluteTime(spec.approved_at)}` : `drafted ${absoluteTime(spec.created_at)}`}
          </span>
          <Badge variant={spec.approved ? 'positive' : 'attention'}>
            {spec.approved ? 'Approved' : 'Awaiting approval'}
          </Badge>
        </div>
      </CardHeader>
      {expanded && (
        <CardContent className="py-4">
          <div className="markdown">
            <Markdown remarkPlugins={[remarkGfm]}>{prose}</Markdown>
          </div>
          {criteria.length > 0 && (
            <div className="mt-5 border-t border-border pt-4">
              <h4 className="mb-3 text-xs font-semibold uppercase tracking-[0.12em] text-muted">
                Acceptance criteria
                <span className="ml-2 font-mono text-faint">{criteria.length}</span>
              </h4>
              <ul className="space-y-2">
                {criteria.map((criterion) => (
                  <CriterionRow key={criterion.id} criterion={criterion} />
                ))}
              </ul>
            </div>
          )}
          {(spec.decomposition?.length ?? 0) > 0 && (
            <div className="mt-5 border-t border-border pt-4">
              <h4 className="mb-3 text-xs font-semibold uppercase tracking-[0.12em] text-muted">Decomposition</h4>
              <ul className="space-y-2">
                {spec.decomposition!.map((item) => (
                  <li key={item.id} className="flex items-baseline gap-2 text-sm">
                    <Badge variant="mono">{item.id}</Badge>
                    <span className="text-foreground/85">{item.summary}</span>
                    <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">
                      {item.repo}
                      {item.depends_on?.length ? ` ← ${item.depends_on.join(', ')}` : ''}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </CardContent>
      )}
    </Card>
  )
}

function CriterionRow({ criterion }: { criterion: AcceptanceCriterion }) {
  const Icon = verifyIcons[criterion.verify] ?? FlaskConical
  const human = criterion.verify === 'human'
  return (
    <li className={cn('flex items-start gap-2.5 rounded-md border px-3 py-2', human ? 'border-edge bg-raised/40' : 'border-border')}>
      <Icon className={cn('mt-0.5 size-4 shrink-0', human ? 'text-foreground' : 'text-faint')} />
      <div className="min-w-0">
        <p className="text-sm leading-6 text-foreground/90">{criterion.criterion}</p>
        <p className="mt-0.5 font-mono text-[11px] text-faint">
          {criterion.id} · {criterion.verify}
          {criterion.ref ? ` · ${criterion.ref}` : ''}
          {human ? ' — checked by the reviewer at the gate' : ''}
        </p>
      </div>
    </li>
  )
}
