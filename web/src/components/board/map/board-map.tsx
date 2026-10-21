// The Board's map view (req-board-map; component-web-task-surfaces, Board
// map). Arranged by requirement, it opens on a map of the requirements with
// open work; opening one shows its tasks flowing into it, with neighbours
// elsewhere as faint links. Arranged by dependency, it is the whole
// workspace's dependency graph. Board loads this module lazily, so React Flow
// and ELK stay out of the initial route chunk (DEC-76).
import '@xyflow/react/dist/style.css'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import {
  Background,
  BackgroundVariant,
  Controls,
  type Edge,
  MarkerType,
  MiniMap,
  type Node,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  useStore,
  useUpdateNodeInternals,
} from '@xyflow/react'
import { BookText, ExternalLink, Eye, EyeOff, Focus, Maximize2, Search, Workflow, X } from 'lucide-react'
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { fetchTaskMap } from '../../../lib/api'
import type { RequirementInsightTab } from '../../../lib/types'
import { cn, relativeTime } from '../../../lib/utils'
import { useWorkspace, useWorkspaceSelection } from '../../app-shell'
import { InsightPanel } from '../../insights/insight-panel'
import { Button } from '../../ui/button'
import { Skeleton } from '../../ui/skeleton'
import type { BoardSearch } from '../board-search'
import { BoardToolbar } from '../board-toolbar'
import { docCards } from './doc-map'
import { buildDocIndex, type DocIndex, type Grouping } from './docs'
import {
  buildModel,
  DONE,
  type GraphStatus,
  type GraphTask,
  labelOf,
  statusMeta,
  statusOrder,
  type TaskGraphModel,
  walk,
} from './graph-model'
import { type HubFlowNode, HubNode } from './hub-node'
import { CARD_HEIGHT, CARD_WIDTH, HUB_HEIGHT, HUB_WIDTH, type LayoutEdge, layoutGraph } from './layout'
import { type InsightEmphasis, MapView } from './map-view'
import { DependencyMark, DOT_ZOOM, statusIcon, type TaskFlowNode, TaskNode } from './task-node'

const nodeTypes = { task: TaskNode, hub: HubNode }
const UNATTACHED = '__none'

export function BoardMap() {
  return (
    <ReactFlowProvider>
      <BoardMapView />
    </ReactFlowProvider>
  )
}

// What the canvas draws: tasks, the document they deliver into (when one is
// open), neighbours outside it, and the edges between them.
interface Scene {
  taskIds: string[]
  hub?: string
  external: Map<string, string>
  edges: LayoutEdge[]
}

function BoardMapView() {
  const { workspace } = useWorkspaceSelection()
  const search = useSearch({ strict: false }) as BoardSearch
  const navigate = useNavigate()
  const { focus, scope: chosenScope, insight: chosenInsight } = search
  // The arrangement lives in the address with the requirement and the task
  // (req-board-map AC-1.1).
  const grouping: Grouping = search.arrangement ?? 'requirements'
  const [showDone, setShowDone] = useState(false)
  const [repo, setRepo] = useState('')
  // One read, filtered by the server (AC-3.4). The last answer stays on the
  // canvas while a changed filter loads.
  const { data, isLoading, error } = useQuery({
    queryKey: ['task-map', workspace, repo, showDone],
    queryFn: ({ signal }) =>
      fetchTaskMap(workspace, { repositories: repo ? [repo] : [], includeFinished: showDone }, signal),
    enabled: Boolean(workspace),
    refetchInterval: 60_000,
    placeholderData: keepPreviousData,
  })
  const model = useMemo(() => buildModel(data?.tasks ?? []), [data])

  const [chainOnly, setChainOnly] = useState(false)
  const [highlight, setHighlight] = useState<Set<string>>(new Set())
  const [requirement, setRequirement] = useState('')
  const [query, setQuery] = useState('')
  const [hover, setHover] = useState<string>()
  // The tasks a hovered insight panel row names (req-requirement-insights
  // AC-1.3).
  const [rowHover, setRowHover] = useState<InsightEmphasis>()

  const go = useCallback(
    (patch: Partial<BoardSearch>) =>
      void navigate({ to: '.', replace: true, search: (prev: BoardSearch) => ({ ...prev, ...patch }) }),
    [navigate],
  )
  const setFocus = useCallback((id: string | undefined) => go({ focus: id }), [go])
  // The task sheet opens over the mounted map, so closing it returns to the
  // same position and selection (AC-2.5).
  const openTask = useCallback(
    (id: string) =>
      void navigate({
        to: '/tasks/$taskId',
        params: { taskId: id },
        search: (prev: BoardSearch) => ({ ...prev, view: 'map' as const, focus: id }),
      }),
    [navigate],
  )
  const docIndex = useMemo<DocIndex | undefined>(
    () => (grouping === 'dependencies' ? undefined : buildDocIndex(model, 'requirement', data?.requirements ?? [])),
    [model, grouping, data],
  )

  // Open work narrowed by the filters; finished work only on request.
  const visible = useMemo(() => {
    const keep = new Set<string>()
    const chain = grouping === 'dependencies' && chainOnly && focus ? chainOf(focus, model) : undefined
    for (const task of model.tasks.values()) {
      if (!showDone && DONE.has(task.status)) continue
      if (repo && task.repo !== repo) continue
      if (grouping === 'dependencies' && requirement && !task.requirements.some((entry) => entry.id === requirement))
        continue
      if (chain && !chain.has(task.id)) continue
      keep.add(task.id)
    }
    return keep
  }, [model, showDone, repo, requirement, chainOnly, focus, grouping])

  const cards = useMemo(() => (docIndex ? docCards(docIndex, model, visible) : []), [docIndex, model, visible])
  // The selected document on the map, if any; nothing selected is the whole
  // map.
  const mapped = grouping !== 'dependencies'
  const scope = mapped && chosenScope && cards.some((card) => card.id === chosenScope) ? chosenScope : undefined
  // The insight tab is kept only while scope names a requirement group on the
  // map (req-requirement-insights AC-1.2); once the read shows it names none,
  // the address drops it, as an unknown arrangement is dropped.
  const insight = scope && scope !== UNATTACHED ? chosenInsight : undefined
  useEffect(() => {
    if (data && chosenInsight && !insight) go({ insight: undefined })
  }, [data, chosenInsight, insight, go])
  const openInsight = useCallback(
    (id: string, tab: RequirementInsightTab | undefined) => go({ scope: id, focus: undefined, insight: tab }),
    [go],
  )

  // The dependency view draws the whole workspace; the map draws itself.
  const scene = useMemo<Scene | undefined>(() => {
    if (mapped) return undefined
    const ids = new Set(visible)
    // Finished dependencies stay as context so no edge dangles.
    for (const id of visible) {
      for (const dependency of model.tasks.get(id)?.dependsOn ?? []) {
        if (DONE.has(model.tasks.get(dependency)?.status ?? 'ready')) ids.add(dependency)
      }
    }
    const edges: LayoutEdge[] = []
    for (const id of ids) {
      for (const dependency of model.tasks.get(id)?.dependsOn ?? []) {
        if (ids.has(dependency)) edges.push({ id: `${dependency}->${id}`, source: dependency, target: id })
      }
    }
    return { taskIds: [...ids], external: new Map(), edges }
  }, [mapped, visible, model])

  // Layout runs when what is drawn changes, not on every status refresh, so a
  // poll never reshuffles the canvas.
  const layoutKey = useMemo(
    () =>
      scene
        ? `${grouping}|${scope ?? ''}|${[...scene.taskIds].sort().join(',')}|${scene.edges
            .map((edge) => edge.id)
            .sort()
            .join(',')}`
        : '',
    [scene, grouping, scope],
  )
  const [positions, setPositions] = useState<Map<string, { x: number; y: number }>>()
  const [laidOutKey, setLaidOutKey] = useState('')
  const latestKey = useRef(layoutKey)
  useEffect(() => {
    latestKey.current = layoutKey
    if (!scene || scene.taskIds.length === 0) return
    const nodes = scene.taskIds.map((id) => ({ id, width: CARD_WIDTH, height: CARD_HEIGHT }))
    if (scene.hub) nodes.push({ id: scene.hub, width: HUB_WIDTH, height: HUB_HEIGHT })
    void layoutGraph(nodes, scene.edges, scene.hub ? 'DOWN' : 'RIGHT').then((next) => {
      if (latestKey.current !== layoutKey) return
      setPositions(next)
      setLaidOutKey(layoutKey)
    })
    // biome-ignore lint/correctness/useExhaustiveDependencies: the key names everything laid out
  }, [layoutKey])

  // Only a selected task traces and dims; hovering lights the hovered task's
  // own edges (AC-2.6).
  const active = focus
  const chain = useMemo(() => {
    if (!active || !scene) return undefined
    if (active === scene.hub) return new Set([active, ...scene.taskIds.filter((id) => !scene.external.has(id))])
    if (!model.tasks.has(active)) return undefined
    const set = chainOf(active, model)
    if (scene.hub) set.add(scene.hub)
    return set
  }, [active, model, scene])
  const needle = query.trim().toLowerCase()
  const matches = useMemo(() => {
    if (!needle) return []
    return [...visible].filter((id) => {
      const task = model.tasks.get(id)
      return task && (task.title.toLowerCase().includes(needle) || task.id.includes(needle))
    })
  }, [needle, visible, model])
  const matchSet = useMemo(() => new Set(matches), [matches])

  const nodes = useMemo<Node[]>(() => {
    if (!positions || !scene) return []
    const list: Node[] = []
    if (scene.hub && docIndex && scope) {
      const position = positions.get(scene.hub)
      const doc = docIndex.docs.get(scope)
      if (position && doc) {
        list.push({
          id: scene.hub,
          type: 'hub',
          position,
          data: {
            doc: { ...doc, kind: docIndex.kind },
            stats: docIndex.stats.get(scope) ?? { total: 0, merged: 0, open: 0, byStatus: {} },
            emphasis: active === scene.hub ? 'focus' : chain && !chain.has(scene.hub) ? 'dim' : 'normal',
            vertical: true,
          },
        } satisfies HubFlowNode)
      }
    }
    for (const id of scene.taskIds) {
      const task = model.tasks.get(id)
      const position = positions.get(id)
      if (!task || !position) continue
      const highlighted = highlight.size === 0 || highlight.has(labelOf(task))
      const inChain = !chain || chain.has(id)
      list.push({
        id,
        type: 'task',
        position,
        data: {
          task,
          unblocks: model.unblocks.get(id) ?? 0,
          emphasis: id === active ? 'focus' : !inChain || !highlighted ? 'dim' : chain ? 'chain' : 'normal',
          match: matchSet.has(id),
          external: scene.external.get(id),
          vertical: Boolean(scene.hub),
        },
      } satisfies TaskFlowNode)
    }
    return list
  }, [positions, scene, docIndex, scope, model, highlight, chain, active, matchSet])

  const flowEdges = useMemo<Edge[]>(() => {
    if (!scene) return []
    const focusTask = active && model.tasks.has(active) ? active : undefined
    const upstream = focusTask ? walk(focusTask, 'up', model.tasks, model.dependents) : undefined
    const downstream = focusTask ? walk(focusTask, 'down', model.tasks, model.dependents) : undefined
    const touches = (edge: { source: string; target: string }, id?: string) =>
      Boolean(id) && (edge.source === id || edge.target === id)
    return scene.edges.map((edge) => {
      const source = model.tasks.get(edge.source)
      const target = model.tasks.get(edge.target)
      const delivers = edge.target === scene.hub
      let up = false
      let down = false
      if (focusTask && upstream && downstream) {
        if (delivers) down = edge.source === focusTask || downstream.has(edge.source)
        else {
          up = (edge.target === focusTask || upstream.has(edge.target)) && upstream.has(edge.source)
          down = (edge.source === focusTask || downstream.has(edge.source)) && downstream.has(edge.target)
        }
      } else if (active && active === scene.hub) {
        down = delivers
      }
      const lit = up || down
      const hovered = !lit && touches(edge, hover)
      const dim = Boolean(active) && !lit && !hovered
      const outside = scene.external.has(edge.source) || scene.external.has(edge.target)
      const satisfied = source && DONE.has(source.status)
      const color = hovered
        ? 'var(--color-muted)'
        : lit
          ? up
            ? 'var(--color-primary)'
            : 'var(--color-attention-dot)'
          : target?.status === 'blocked'
            ? 'var(--color-failure)'
            : delivers
              ? 'var(--color-faint)'
              : 'var(--color-edge)'
      return {
        id: edge.id,
        source: edge.source,
        target: edge.target,
        // Work in flight flows: edges out of tasks an agent is working, and
        // the traced path of a selected task (AC-2.7).
        animated: !dim && (source?.status === 'running' || lit),
        markerEnd: delivers ? undefined : { type: MarkerType.ArrowClosed, color, width: 14, height: 14 },
        style: {
          stroke: color,
          strokeWidth: lit || hovered ? 2.25 : delivers ? 1.5 : 1.25,
          strokeDasharray: delivers || satisfied || outside ? '4 5' : undefined,
          opacity: dim ? 0.12 : outside && !lit && !hovered ? 0.6 : 1,
        },
        zIndex: lit || hovered ? 1 : 0,
      }
    })
  }, [scene, model, active, hover])

  // The server applies the repository filter, so the choices come from the
  // workspace rather than from what the filtered read returned.
  const { data: workspaceInfo } = useWorkspace()
  const repos = useMemo(() => (workspaceInfo?.repos ?? []).map((entry) => entry.name).sort(), [workspaceInfo])
  const requirements = useMemo(() => {
    const counts = new Map<string, { title: string; count: number }>()
    for (const task of model.tasks.values()) {
      if (DONE.has(task.status)) continue
      for (const entry of task.requirements) {
        const current = counts.get(entry.id)
        counts.set(entry.id, { title: entry.title, count: (current?.count ?? 0) + 1 })
      }
    }
    return [...counts.entries()].sort((a, b) => b[1].count - a[1].count)
  }, [model])
  const counted = useMemo(
    () =>
      scene
        ? scene.taskIds.filter((id) => !scene.external.has(id))
        : scope
          ? (cards.find((entry) => entry.id === scope)?.tasks.map((task) => task.id) ?? [])
          : [...visible].filter((id) => !DONE.has(model.tasks.get(id)?.status ?? 'closed')),
    [scene, scope, grouping, visible, cards, model],
  )
  // Counted by the stated label, each with its treatment (DEC-61).
  const counts = useMemo(() => {
    const result = new Map<string, { status: GraphStatus; count: number }>()
    for (const id of counted) {
      const task = model.tasks.get(id)
      if (!task) continue
      const label = labelOf(task)
      result.set(label, { status: task.status, count: (result.get(label)?.count ?? 0) + 1 })
    }
    return [...result].sort(
      ([a, x], [b, y]) => statusOrder.indexOf(x.status) - statusOrder.indexOf(y.status) || a.localeCompare(b),
    )
  }, [counted, model])

  // The shared toolbar renders in every state, so the view switch stays
  // reachable while the map's read is loading or has failed.
  if (isLoading)
    return (
      <>
        <BoardToolbar />
        <Skeleton className="m-6 h-96" />
      </>
    )
  if (error)
    return (
      <>
        <BoardToolbar />
        <p className="m-6 text-sm text-failure">{String(error)}</p>
      </>
    )

  const card = cards.find((entry) => entry.id === scope)
  const stats = scope && docIndex ? docIndex.stats.get(scope) : undefined
  const focused = focus ? model.tasks.get(focus) : undefined
  // A selected task's panel takes the insight panel's place; clearing the
  // task restores the tab the address still names (AC-1.4).
  const shownInsight = focused ? undefined : insight
  const insightDoc = shownInsight && scope ? docIndex?.docs.get(scope) : undefined
  const jump = () => {
    if (matches[0]) setFocus(matches[0])
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <BoardToolbar>
        <fieldset aria-label="Arrange by" className="m-0 flex rounded-md border border-border p-0.5">
          {(
            [
              ['requirements', 'Requirements', BookText],
              ['dependencies', 'Dependencies', Workflow],
            ] as const
          ).map(([value, label, Icon]) => (
            <button
              key={value}
              type="button"
              aria-pressed={grouping === value}
              onClick={() =>
                go({
                  arrangement: value === 'requirements' ? undefined : value,
                  scope: undefined,
                  focus: undefined,
                  insight: undefined,
                })
              }
              className={cn(
                'inline-flex items-center gap-1.5 rounded px-2.5 py-1 text-xs font-medium transition-colors',
                grouping === value ? 'bg-raised text-foreground' : 'text-muted hover:text-foreground',
              )}
            >
              <Icon aria-hidden className="size-3.5" />
              {label}
            </button>
          ))}
        </fieldset>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <label className="relative">
            <Search
              aria-hidden
              className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-faint"
            />
            <input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') jump()
                if (event.key === 'Escape') setQuery('')
              }}
              placeholder="Find a task"
              aria-label="Find a task"
              className="h-8 w-48 rounded-md border border-border bg-background pl-7 pr-2 text-xs"
            />
          </label>
          {grouping === 'dependencies' && (
            <Select value={requirement} onChange={setRequirement} label="Requirement">
              <option value="">All requirements</option>
              {requirements.map(([id, { title, count }]) => (
                <option key={id} value={id}>
                  {title} ({count})
                </option>
              ))}
            </Select>
          )}
          {repos.length > 1 && (
            <Select value={repo} onChange={setRepo} label="Repository">
              <option value="">All repositories</option>
              {repos.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </Select>
          )}
          <Button size="sm" variant="ghost" onClick={() => setShowDone((value) => !value)} aria-pressed={showDone}>
            {showDone ? <Eye /> : <EyeOff />}
            Finished
          </Button>
        </div>
      </BoardToolbar>
      <div className="relative flex min-h-0 flex-1">
        <section className="flex min-w-0 flex-1 flex-col">
          <header className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b border-border px-4 py-2.5">
            <div className="min-w-0">
              <h2 className="truncate text-sm font-semibold text-foreground">
                {grouping === 'dependencies' ? 'All open work' : (card?.title ?? `All ${grouping}`)}
              </h2>
              {stats && (
                <p className="text-xs text-muted">
                  {stats.merged} of {stats.total} delivered
                  {scope && scope !== UNATTACHED && (
                    <>
                      {' · '}
                      <a
                        href={
                          docIndex?.kind === 'requirement'
                            ? `/requirements?requirement=${scope}`
                            : `/system-design?document=${scope}`
                        }
                        className="hover:text-foreground hover:underline"
                      >
                        Open document
                      </a>
                    </>
                  )}
                </p>
              )}
            </div>
            <div className="ml-auto flex flex-wrap items-center gap-1.5">
              {counts.map(([label, { status, count }]) => {
                const Icon = statusIcon[status]
                const on = highlight.has(label)
                return (
                  <button
                    key={label}
                    type="button"
                    aria-pressed={on}
                    title={`Highlight ${label}`}
                    onClick={() =>
                      setHighlight((prev) => {
                        const next = new Set(prev)
                        if (next.has(label)) next.delete(label)
                        else next.add(label)
                        return next
                      })
                    }
                    className={cn(
                      'inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs',
                      on ? 'bg-raised text-foreground' : 'text-muted hover:bg-surface',
                    )}
                  >
                    <Icon aria-hidden className={cn('size-3.5', statusMeta[status].tone)} />
                    {count} {label}
                  </button>
                )
              })}
            </div>
          </header>
          <div className="relative min-h-0 flex-1">
            {mapped && docIndex ? (
              <MapView
                model={model}
                index={docIndex}
                visible={visible}
                highlight={highlight}
                matches={matchSet}
                selected={scope}
                focus={focus}
                insight={insightDoc ? shownInsight : undefined}
                emphasis={insightDoc && rowHover?.requirementId === scope ? rowHover : undefined}
                // Another requirement's group closes the insight panel.
                onSelectDoc={(id) =>
                  go({ scope: id, focus: undefined, ...(id === scope ? {} : { insight: undefined }) })
                }
                onInsight={openInsight}
                // Clicking empty canvas clears the task, then the selection.
                onClear={() => go(focus ? { focus: undefined } : { scope: undefined, insight: undefined })}
                onFocus={setFocus}
                onOpen={openTask}
              />
            ) : scene && scene.taskIds.length > 0 ? (
              <Canvas
                nodes={nodes}
                edges={flowEdges}
                layoutKey={laidOutKey}
                focus={focus}
                onHover={setHover}
                onSelect={(id) => {
                  if (id === scene.hub) return setFocus(undefined)
                  setFocus(id === focus ? undefined : id)
                }}
                onOpen={openTask}
                onClear={() => setFocus(undefined)}
                minFitZoom={scene.hub ? 0.45 : 0.08}
              />
            ) : (
              <p className="m-6 text-sm text-muted">No tasks match these filters.</p>
            )}
            {grouping === 'dependencies' && focus && (
              <div className="absolute left-3 top-3 flex items-center gap-1 rounded-md border border-border bg-card p-1 shadow-sm">
                <Button
                  size="sm"
                  variant={chainOnly ? 'default' : 'ghost'}
                  aria-pressed={chainOnly}
                  onClick={() => setChainOnly((value) => !value)}
                >
                  <Focus />
                  Only this chain
                </Button>
              </div>
            )}
          </div>
        </section>
        {focused && (
          <aside className="hidden w-80 shrink-0 overflow-y-auto border-l border-border bg-surface md:block">
            <div className="flex justify-end px-2 pt-2">
              <Button size="icon" variant="ghost" aria-label="Close" onClick={() => setFocus(undefined)}>
                <X />
              </Button>
            </div>
            <FocusPanel
              task={focused}
              model={model}
              onFocus={setFocus}
              onOpen={openTask}
              onRequirement={(id) =>
                go({
                  arrangement: undefined,
                  scope: id,
                  focus: undefined,
                  ...(id === scope ? {} : { insight: undefined }),
                })
              }
            />
          </aside>
        )}
        {shownInsight && insightDoc && scope && (
          <InsightPanel
            key={`${workspace}|${scope}`}
            workspace={workspace}
            requirement={insightDoc}
            tab={shownInsight}
            onTab={(tab) => go({ insight: tab })}
            // Closing the panel keeps the requirement selected.
            onClose={() => go({ insight: undefined })}
            onHover={(taskIds) => setRowHover(taskIds ? { requirementId: scope, taskIds } : undefined)}
            onActivateTask={setFocus}
          />
        )}
      </div>
    </div>
  )
}

function chainOf(id: string, model: TaskGraphModel) {
  return new Set([
    id,
    ...walk(id, 'up', model.tasks, model.dependents),
    ...walk(id, 'down', model.tasks, model.dependents),
  ])
}

// SVG fill attributes cannot read CSS variables, so the minimap gets the
// resolved token values, read once per theme.
function resolvedPalette() {
  const style = getComputedStyle(document.documentElement)
  const read = (name: string) => style.getPropertyValue(name).trim() || '#888'
  return {
    failure: read('--color-failure'),
    attention: read('--color-attention-dot'),
    primary: read('--color-primary'),
    positive: read('--color-positive'),
    edge: read('--color-edge'),
    foreground: read('--color-foreground'),
  }
}

function minimapColor(status: GraphStatus, palette: ReturnType<typeof resolvedPalette>) {
  switch (status) {
    case 'blocked':
      return palette.failure
    case 'plan_gate':
    case 'merge_gate':
    case 'needs_you':
      return palette.attention
    case 'running':
      return palette.primary
    case 'ready':
      return palette.positive
    default:
      return palette.edge
  }
}

function Canvas({
  nodes,
  edges,
  layoutKey,
  focus,
  onHover,
  onSelect,
  onOpen,
  onClear,
  minFitZoom = 0.08,
}: {
  nodes: Node[]
  edges: Edge[]
  layoutKey: string
  focus?: string
  onHover: (id: string | undefined) => void
  onSelect: (id: string) => void
  onOpen: (id: string) => void
  onClear: () => void
  // The least zoom a fresh layout is framed at, so a document's graph opens
  // readable even when it does not fit.
  minFitZoom?: number
}) {
  const flow = useReactFlow()
  // Hover lights a task's own edges after a short pause, as on the map.
  const hoverTimer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const dots = useStore((state) => state.transform[2] < DOT_ZOOM)
  const updateNodeInternals = useUpdateNodeInternals()
  const theme = document.documentElement.dataset.theme
  // biome-ignore lint/correctness/useExhaustiveDependencies: the palette follows the theme
  const palette = useMemo(resolvedPalette, [theme])
  // Dots and cards anchor edges differently, so the handles are re-measured
  // whenever the zoom crosses the threshold.
  const ids = useMemo(() => nodes.map((node) => node.id), [nodes])
  // biome-ignore lint/correctness/useExhaustiveDependencies: re-measure only on a mode switch
  useEffect(() => {
    requestAnimationFrame(() => updateNodeInternals(ids))
  }, [dots])
  // A focused task is centred at a readable zoom; otherwise a new layout is
  // framed whole.
  function frame() {
    // React Flow defers a fitView until nodes exist, which would override a
    // later centring, so nothing is framed before the layout lands.
    if (nodes.length === 0) return
    const node = focus ? nodes.find((entry) => entry.id === focus) : undefined
    if (node) {
      void flow.setCenter(node.position.x + CARD_WIDTH / 2, node.position.y + CARD_HEIGHT / 2, {
        zoom: Math.max(flow.getZoom(), 0.85),
        duration: 400,
      })
    } else void flow.fitView({ duration: 400, padding: 0.08, maxZoom: 1, minZoom: minFitZoom })
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: frame on a new layout only
  useEffect(() => {
    if (layoutKey) requestAnimationFrame(frame)
  }, [layoutKey])
  // biome-ignore lint/correctness/useExhaustiveDependencies: frame on a new focus only
  useEffect(() => {
    if (focus) requestAnimationFrame(frame)
  }, [focus])
  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      colorMode={theme === 'dark' ? 'dark' : 'light'}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      minZoom={0.08}
      maxZoom={1.6}
      onNodeMouseEnter={(_, node) => {
        clearTimeout(hoverTimer.current)
        if (node.type === 'task') hoverTimer.current = setTimeout(() => onHover(node.id), 140)
      }}
      onNodeMouseLeave={() => {
        clearTimeout(hoverTimer.current)
        onHover(undefined)
      }}
      // A double-click's second click leaves the selection to the
      // double-click, which opens the task (AC-2.5).
      onNodeClick={(event, node) => event.detail < 2 && onSelect(node.id)}
      // A double-click opens the task (AC-2.5). React Flow's zoom pane would
      // otherwise take it as a zoom and stop it reaching the node.
      zoomOnDoubleClick={false}
      onNodeDoubleClick={(_, node) => node.type === 'task' && onOpen(node.id)}
      onPaneClick={onClear}
      style={
        {
          '--xy-background-color': 'var(--color-surface)',
          '--xy-minimap-background-color': 'var(--color-card)',
          '--xy-controls-button-background-color': 'var(--color-card)',
          '--xy-controls-button-border-color': 'var(--color-border)',
          '--xy-controls-button-color': 'var(--color-muted)',
        } as React.CSSProperties
      }
    >
      <Background variant={BackgroundVariant.Dots} gap={22} size={1} color={palette.edge} />
      <Controls showInteractive={false} position="bottom-left">
        <button
          type="button"
          className="react-flow__controls-button"
          title="Fit"
          onClick={() => void flow.fitView({ duration: 400 })}
        >
          <Maximize2 className="!max-h-3 !max-w-3" />
        </button>
      </Controls>
      <MiniMap
        pannable
        zoomable
        position="bottom-right"
        nodeBorderRadius={8}
        nodeColor={(node) =>
          node.type === 'task' ? minimapColor((node as TaskFlowNode).data.task.status, palette) : palette.foreground
        }
        maskColor="rgba(0, 0, 0, 0.35)"
      />
    </ReactFlow>
  )
}

function Select({
  value,
  onChange,
  label,
  children,
}: {
  value: string
  onChange: (value: string) => void
  label: string
  children: ReactNode
}) {
  return (
    <select
      aria-label={label}
      value={value}
      onChange={(event) => onChange(event.target.value)}
      className="h-8 max-w-52 rounded-md border border-border bg-background px-2 text-xs text-foreground"
    >
      {children}
    </select>
  )
}

function TaskRow({ task, model, onFocus }: { task: GraphTask; model: TaskGraphModel; onFocus: (id: string) => void }) {
  const Icon = statusIcon[task.status]
  const unblocks = model.unblocks.get(task.id) ?? 0
  return (
    <button
      type="button"
      onClick={() => onFocus(task.id)}
      className="flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-raised"
    >
      <Icon aria-hidden className={cn('mt-0.5 size-3.5 shrink-0', statusMeta[task.status].tone)} />
      <span className="min-w-0 flex-1">
        <span className="line-clamp-2 text-xs font-medium text-foreground">{task.title}</span>
        <span className="font-mono text-[10.5px] text-faint">
          {task.id} · {labelOf(task)}
          {unblocks > 0 && ` · unblocks ${unblocks}`}
        </span>
      </span>
    </button>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="border-b border-border px-4 py-3 last:border-b-0">
      <h3 className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-faint">{title}</h3>
      {children}
    </section>
  )
}

function FocusPanel({
  task,
  model,
  onFocus,
  onOpen,
  onRequirement,
}: {
  task: GraphTask
  model: TaskGraphModel
  onFocus: (id: string) => void
  onOpen: (id: string) => void
  onRequirement: (id: string) => void
}) {
  const Icon = statusIcon[task.status]
  const upstream = walk(task.id, 'up', model.tasks, model.dependents)
  const openUpstream = [...upstream].filter((id) => !DONE.has(model.tasks.get(id)?.status ?? 'ready'))
  const direct = task.dependsOn.map((id) => model.tasks.get(id)).filter(Boolean) as GraphTask[]
  const dependents = (model.dependents.get(task.id) ?? [])
    .map((id) => model.tasks.get(id))
    .filter(Boolean) as GraphTask[]
  const unblocks = model.unblocks.get(task.id) ?? 0
  return (
    <div>
      <div className="border-b border-border px-4 py-3">
        <div className={cn('mb-1 flex items-center gap-1.5 text-xs font-medium', statusMeta[task.status].tone)}>
          <Icon aria-hidden className="size-3.5" />
          {labelOf(task)}
          {task.blocking.length > 0 && <DependencyMark task={task} />}
        </div>
        <h2 className="text-sm font-semibold leading-snug text-foreground">{task.title}</h2>
        <p className="mt-0.5 font-mono text-[11px] text-faint">
          {task.id}
          {task.repo && ` · ${task.repo}`}
        </p>
        {task.actionSummary && (
          <p className="mt-2 text-xs text-muted">
            <span className="font-medium text-foreground">{task.actionLabel}.</span> {task.actionSummary}
          </p>
        )}
        {task.lastEventAt && <p className="mt-1 text-[11px] text-faint">Updated {relativeTime(task.lastEventAt)}</p>}
        {!task.stub && (
          <Button size="sm" variant="outline" className="mt-3" onClick={() => onOpen(task.id)}>
            <ExternalLink />
            Open task
          </Button>
        )}
      </div>
      <Section title={`Waits on · ${openUpstream.length} open of ${upstream.size} upstream`}>
        {direct.length ? (
          direct.map((entry) => <TaskRow key={entry.id} task={entry} model={model} onFocus={onFocus} />)
        ) : (
          <p className="text-xs text-muted">No dependencies.</p>
        )}
      </Section>
      <Section title={`Unblocks · ${unblocks} open downstream`}>
        {dependents.length ? (
          dependents.map((entry) => <TaskRow key={entry.id} task={entry} model={model} onFocus={onFocus} />)
        ) : (
          <p className="text-xs text-muted">Nothing waits on this task.</p>
        )}
      </Section>
      <Section title="Requirements">
        <div className="flex flex-wrap gap-1.5">
          {task.requirements.length ? (
            task.requirements.map((entry) => (
              <button
                key={entry.id}
                type="button"
                onClick={() => onRequirement(entry.id)}
                title="Show only this requirement's tasks"
                className="rounded-md border border-border bg-card px-2 py-0.5 text-[11px] text-foreground hover:border-edge"
              >
                {entry.title}
              </button>
            ))
          ) : (
            <p className="text-xs text-muted">None attached.</p>
          )}
        </div>
      </Section>
    </div>
  )
}
