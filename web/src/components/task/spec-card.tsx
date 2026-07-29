import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react'
import mermaid from 'mermaid'
import { Link } from '@tanstack/react-router'
import { ChevronDown, ChevronRight, ChevronUp, FlaskConical, Globe, MousePointerClick, Square } from 'lucide-react'
import type { AcceptanceCriterion, SpecVersion } from '../../lib/types'
import { absoluteTime, cn } from '../../lib/utils'
import { Badge } from '../ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { MarkdownProse } from '../ui/markdown-prose'

// Machine-owned fenced blocks (§4.1) are stripped from the prose and
// rendered structurally by the acceptance checklist below.
const machineBlock = /```conveyor:(?:acceptance|decomposition)\n[\s\S]*?```\n?/g

const verifyIcons: Record<AcceptanceCriterion['verify'], typeof FlaskConical> = {
  test: FlaskConical,
  playwright: Globe,
  'computer-use': MousePointerClick,
  human: Square,
}

mermaid.initialize({ startOnLoad: false, securityLevel: 'strict' })

export function MermaidBlock({ source }: { source: string }) {
  const id = `mermaid-${useId().replace(/:/g, '')}`
  const [svg, setSvg] = useState<string>()
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    let active = true
    setSvg(undefined)
    setFailed(false)
    mermaid.render(id, source).then(({ svg: rendered }) => {
      if (active) setSvg(rendered)
    }).catch(() => {
      if (active) setFailed(true)
    })
    return () => { active = false }
  }, [id, source])

  if (failed) return <pre><code className="language-mermaid">{source}</code></pre>
  if (!svg) return <pre><code className="language-mermaid">{source}</code></pre>
  return <div className="my-4 overflow-x-auto" data-mermaid dangerouslySetInnerHTML={{ __html: svg }} />
}

// The spec review card (spec §13.3 element 3): rendered markdown plus the
// acceptance-criteria checklist. Human-verify criteria surface as explicit
// checkboxes rather than being pretend-verified (§4.1 rule 2).
export function SpecCard({
  spec,
  collapsible = true,
  overflowExpandable = false,
}: {
  spec: SpecVersion
  collapsible?: boolean
  overflowExpandable?: boolean
}) {
  const [collapsed, setCollapsed] = useState(false)
  const [contentExpanded, setContentExpanded] = useState(false)
  const [hasOverflow, setHasOverflow] = useState(false)
  const viewportRef = useRef<HTMLDivElement>(null)
  const viewportID = useId()
  const expanded = !collapsible || !collapsed
  const overflowExpanded = !overflowExpandable || contentExpanded
  // Agent-authored prose sometimes runs a sentence straight into a heading with
  // a single newline; markdown needs a blank line for the `#` to render as one.
  const prose = useMemo(
    () => spec.content.replace(machineBlock, '').replace(/([^\n])\n(#{1,6} )/g, '$1\n\n$2'),
    [spec.content],
  )
  const criteria = spec.acceptance ?? []

  useLayoutEffect(() => {
    if (!overflowExpandable || !expanded) {
      setHasOverflow(false)
      return
    }

    const viewport = viewportRef.current
    if (!viewport || contentExpanded) return
    const measure = () => setHasOverflow(viewport.scrollHeight > viewport.clientHeight + 1)
    measure()

    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    if (viewport.firstElementChild) observer.observe(viewport.firstElementChild)
    return () => observer.disconnect()
  }, [contentExpanded, expanded, overflowExpandable, prose, spec.acceptance, spec.decomposition])

  const toggleCollapsed = () => {
    if (!collapsed) setContentExpanded(false)
    setCollapsed(!collapsed)
  }

  return (
    <Card>
      <CardHeader className="items-center">
        {collapsible ? (
          <button
            type="button"
            onClick={toggleCollapsed}
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
          <div className="relative">
            <div
              id={viewportID}
              ref={viewportRef}
              className={cn(overflowExpandable && !contentExpanded && 'max-h-96 overflow-hidden')}
            >
              <div>
                <MarkdownProse
                  components={{
                    code({ className, children, ...props }) {
                      const source = String(children).replace(/\n$/, '')
                      if (className === 'language-mermaid') return <MermaidBlock source={source} />
                      return <code className={className} {...props}>{children}</code>
                    },
                  }}
                >{prose}</MarkdownProse>
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
                          {spec.materialized_children?.find((child) => child.origin_sub_id === item.id) ? (
                            <Link to="/tasks/$taskId" params={{ taskId: spec.materialized_children!.find((child) => child.origin_sub_id === item.id)!.id }} className="text-primary hover:underline">
                              {item.summary}
                            </Link>
                          ) : <span className="text-foreground/85">{item.summary}</span>}
                          <span className="ml-auto shrink-0 font-mono text-[11px] text-faint">
                            {item.repo}
                            {item.depends_on?.length ? ` ← ${item.depends_on.join(', ')}` : ''}
                            {spec.materialized_children?.find((child) => child.origin_sub_id === item.id) && ` · ${spec.materialized_children.find((child) => child.origin_sub_id === item.id)!.state}`}
                          </span>
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
              </div>
            </div>
            {hasOverflow && !overflowExpanded && (
              <div
                aria-hidden="true"
                className="spec-overflow-shadow pointer-events-none absolute inset-x-0 bottom-0 h-24"
                data-spec-overflow-shadow
              />
            )}
          </div>
          {hasOverflow && (
            <div className="-mx-4 -mb-4 mt-4 border-t border-border">
              <button
                type="button"
                aria-controls={viewportID}
                aria-expanded={overflowExpanded}
                className="flex w-full items-center justify-center gap-1.5 rounded-b-md py-2 text-xs font-medium text-primary hover:bg-primary-soft focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-primary"
                onClick={() => setContentExpanded((value) => !value)}
              >
                {overflowExpanded ? <ChevronUp aria-hidden="true" className="size-3.5" /> : <ChevronDown aria-hidden="true" className="size-3.5" />}
                {overflowExpanded ? 'Show less' : 'Show more'}
              </button>
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
