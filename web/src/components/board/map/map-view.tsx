// Every requirement on one canvas (req-board-map REQ-1). Zoomed out, each
// requirement is one tile and nothing else is drawn. Zoomed in, the tile opens
// into its tasks flowing down into the requirement. Selecting a requirement
// flies the camera to it.
import {
  Background,
  BackgroundVariant,
  ControlButton,
  Controls,
  type Edge,
  MarkerType,
  MiniMap,
  type Node,
  type NodeProps,
  ReactFlow,
  useReactFlow,
  useStore,
  useUpdateNodeInternals,
} from '@xyflow/react'
import { Maximize2 } from 'lucide-react'
import { memo, useEffect, useMemo, useRef, useState } from 'react'
import { cn } from '../../../lib/utils'
import { byUrgency } from './doc-map'
import { type DocIndex, mapClusters, UNATTACHED } from './docs'
import { DONE, type GraphStatus, type GraphTask, labelOf, type TaskGraphModel, walk } from './graph-model'
import { type HubFlowNode, HubNode } from './hub-node'
import { CARD_HEIGHT, CARD_WIDTH, CLUSTER_HEADER, layoutClusters } from './layout'
import { type TaskFlowNode, TaskNode } from './task-node'

// Below this zoom a requirement is a tile; above it, its tasks.
export const TILE_ZOOM = 0.3

// Zoomed out, every requirement is a tile of the same size in a grid, most
// urgent first; zoomed in, its tasks are laid out where the cluster layout put
// them. Crossing the threshold swaps the two, keeping the requirement under
// the pointer in place.
const CELL_W = 1300
const CELL_H = 820
const CELL_GAP = 110

const GATES = new Set<GraphStatus>(['plan_gate', 'merge_gate', 'needs_you'])

type TileData = {
  title: string
  open: number
  total: number
  merged: number
  blocked: number
  gates: number
  selected: boolean
  dim: boolean
  // Drawn as an overview tile rather than the frame around its tasks.
  far: boolean
}
type TileFlowNode = Node<TileData, 'tile'>

const TileNode = memo(function TileNode({ data, width = 0, height = 0 }: NodeProps<TileFlowNode>) {
  // The canvas decides the mode, so tile and tasks never disagree.
  const far = data.far
  // Quantised, so a zoom gesture re-renders the tiles a few times, not on
  // every frame.
  const zoom = useStore((state) => Math.max(Math.round(state.transform[2] * 40) / 40, 0.025))
  const dot = data.blocked ? 'bg-failure' : data.gates ? 'bg-attention-dot' : undefined
  if (!far) {
    return (
      <div
        className={cn(
          'relative rounded-[28px] border-2 bg-card/35',
          data.selected ? 'border-primary' : 'border-border',
          data.dim && 'opacity-35',
        )}
        style={{ width, height }}
      >
        <div
          className="flex items-center gap-3 px-8"
          style={{ height: CLUSTER_HEADER }}
          title={`${data.open} open · ${data.merged}/${data.total} delivered`}
        >
          <span className="truncate text-[24px] font-semibold text-foreground">{data.title}</span>
          {dot && <span className={cn('size-2.5 shrink-0 rounded-full', dot)} />}
          <span className="ml-auto shrink-0 font-mono text-[15px] text-faint">{data.open} open</span>
        </div>
      </div>
    )
  }
  // One text size and one layout for every tile (13px on screen): the title,
  // then what needs a person beside a ring of how much is delivered. A tile
  // too small for that keeps the title alone.
  const screenH = height * zoom
  const size = 13 / zoom
  const detailed = screenH >= 92
  const summary = data.blocked
    ? { text: `${data.blocked} blocked`, tone: 'text-failure' }
    : data.gates
      ? { text: `${data.gates} to decide`, tone: 'text-attention' }
      : undefined
  const delivered = data.total ? data.merged / data.total : 0
  const circumference = 2 * Math.PI * 16
  return (
    <div
      className={cn(
        'flex flex-col overflow-hidden rounded-[22px] border-2 bg-card p-[1em] shadow-sm transition-colors hover:border-edge',
        detailed ? 'justify-between' : 'justify-center',
        data.selected ? 'border-primary' : 'border-border',
      )}
      style={{ width, height, fontSize: size }}
      title={`${data.title} · ${data.merged}/${data.total} delivered`}
    >
      <span className={cn('font-semibold leading-[1.2] text-foreground', detailed ? 'line-clamp-2' : 'line-clamp-1')}>
        {data.title}
      </span>
      {detailed && (
        <div className="flex items-end gap-[0.6em]">
          <div className="flex min-w-0 flex-1 flex-col gap-[0.15em] whitespace-nowrap text-[0.72em]">
            {summary && <span className={cn('truncate font-medium', summary.tone)}>{summary.text}</span>}
            <span className="truncate font-mono text-faint">{data.open} open</span>
          </div>
          <span className="relative size-[2.7em] shrink-0">
            <svg viewBox="0 0 40 40" className="size-full -rotate-90">
              <title>{`${Math.round(delivered * 100)}% delivered`}</title>
              <circle cx="20" cy="20" r="16" fill="none" className="stroke-raised" strokeWidth="4" />
              {delivered > 0 && (
                <circle
                  cx="20"
                  cy="20"
                  r="16"
                  fill="none"
                  className="stroke-positive"
                  strokeWidth="4"
                  strokeLinecap="round"
                  strokeDasharray={`${circumference * delivered} ${circumference}`}
                />
              )}
            </svg>
            <span className="absolute inset-0 grid place-items-center font-mono text-[0.58em] text-muted">
              {Math.round(delivered * 100)}%
            </span>
          </span>
        </div>
      )}
    </div>
  )
})

const nodeTypes = { tile: TileNode, hub: HubNode, task: TaskNode }

export function MapView({
  model,
  index,
  visible,
  highlight,
  matches,
  selected,
  focus,
  onSelectDoc,
  onClear,
  onFocus,
  onOpen,
}: {
  model: TaskGraphModel
  index: DocIndex
  visible: Set<string>
  highlight: Set<string>
  matches: Set<string>
  selected?: string
  focus?: string
  onSelectDoc: (id: string) => void
  onClear: () => void
  onFocus: (id: string | undefined) => void
  onOpen: (id: string) => void
}) {
  const flow = useReactFlow()
  const rawFar = useStore((state) => state.transform[2] < TILE_ZOOM)
  const paneAspect = useStore((state) => (state.height ? state.width / state.height : 1.6))
  // While the camera flies into (or out of) a cluster the canvas stays in
  // cluster mode, so the tile visibly expands into its tasks.
  const [forceNear, setForceNear] = useState(false)
  const flying = useRef(false)
  const far = rawFar && !forceNear
  const pointer = useRef<{ x: number; y: number }>(undefined)
  const updateNodeInternals = useUpdateNodeInternals()
  const [hoverTask, setHoverTask] = useState<string>()

  const clustering = useMemo(() => mapClusters(visible, model, index), [visible, model, index])
  const layoutKey = useMemo(
    () =>
      clustering.clusters
        .map((cluster) => `${cluster.id}:${[...cluster.taskIds].sort().join(',')}:${cluster.edges.length}`)
        .sort()
        .join('|'),
    [clustering],
  )
  const [layout, setLayout] = useState<Awaited<ReturnType<typeof layoutClusters>>>()
  const [laidOutKey, setLaidOutKey] = useState('')
  const latestKey = useRef(layoutKey)
  useEffect(() => {
    latestKey.current = layoutKey
    if (clustering.clusters.length === 0) return
    void layoutClusters(clustering.clusters).then((next) => {
      if (latestKey.current !== layoutKey) return
      setLayout(next)
      setLaidOutKey(layoutKey)
    })
    // biome-ignore lint/correctness/useExhaustiveDependencies: the key names everything laid out
  }, [layoutKey])

  const grid = useMemo(() => {
    const tasksOf = (cluster: (typeof clustering.clusters)[number]) => ({
      tasks: cluster.taskIds.map((id) => model.tasks.get(id)).filter((task): task is GraphTask => Boolean(task)),
    })
    const ordered = [...clustering.clusters].sort((a, b) => byUrgency(tasksOf(a), tasksOf(b)))
    const cols = Math.max(1, Math.round(Math.sqrt((ordered.length * paneAspect * CELL_H) / CELL_W)))
    const cells = new Map<string, { x: number; y: number; width: number; height: number }>()
    ordered.forEach((cluster, index) => {
      cells.set(cluster.id, {
        x: (index % cols) * (CELL_W + CELL_GAP),
        y: Math.floor(index / cols) * (CELL_H + CELL_GAP),
        width: CELL_W,
        height: CELL_H,
      })
    })
    return cells
  }, [clustering, model, paneAspect])

  // Only a clicked task dims the rest of the canvas; hovering lights the
  // hovered card's own lines, after a short pause, so sweeping the pointer
  // across a dense cluster does not strobe it.
  const activeTask = focus
  const hoverTimer = useRef<ReturnType<typeof setTimeout>>(undefined)
  const chain = useMemo(() => {
    if (!activeTask || !model.tasks.has(activeTask)) return undefined
    return new Set([
      activeTask,
      ...walk(activeTask, 'up', model.tasks, model.dependents),
      ...walk(activeTask, 'down', model.tasks, model.dependents),
    ])
  }, [activeTask, model])

  const nodes = useMemo<Node[]>(() => {
    if (!layout) return []
    const list: Node[] = []
    for (const cluster of clustering.clusters) {
      const frame = far ? grid.get(cluster.id) : layout.frames.get(cluster.id)
      if (!frame) continue
      const tasks = cluster.taskIds.map((id) => model.tasks.get(id)).filter(Boolean)
      const counts: Partial<Record<GraphStatus, number>> = {}
      for (const task of tasks) if (task) counts[task.status] = (counts[task.status] ?? 0) + 1
      const stats = index.stats.get(cluster.id)
      const touched = !chain || cluster.taskIds.some((id) => chain.has(id))
      list.push({
        id: `frame:${cluster.id}`,
        type: 'tile',
        position: { x: frame.x, y: frame.y },
        width: frame.width,
        height: frame.height,
        zIndex: -1,
        selectable: false,
        data: {
          title: cluster.id === UNATTACHED ? `No ${index.kind}` : (index.docs.get(cluster.id)?.title ?? cluster.id),
          open: tasks.filter((task) => task && !DONE.has(task.status)).length,
          total: stats?.total ?? cluster.taskIds.length,
          merged: stats?.merged ?? 0,
          blocked: counts.blocked ?? 0,
          gates: tasks.filter((task) => task && GATES.has(task.status)).length,
          selected: cluster.id === selected,
          dim: !touched,
          far,
        },
      } satisfies TileFlowNode)
      // Zoomed out, the tile is all there is.
      if (far) continue
      if (cluster.hubId) {
        const position = layout.positions.get(cluster.hubId)
        const doc = index.docs.get(cluster.id)
        if (position && doc) {
          list.push({
            id: cluster.hubId,
            type: 'hub',
            position,
            data: {
              doc: { ...doc, kind: index.kind },
              stats: stats ?? { total: 0, merged: 0, open: 0, byStatus: {} },
              emphasis: chain && !touched ? 'dim' : 'normal',
              vertical: true,
            },
          } satisfies HubFlowNode)
        }
      }
      for (const id of cluster.taskIds) {
        const task = model.tasks.get(id)
        const position = layout.positions.get(id)
        if (!task || !position) continue
        const highlighted = highlight.size === 0 || highlight.has(labelOf(task))
        list.push({
          id,
          type: 'task',
          position,
          data: {
            task,
            unblocks: model.unblocks.get(id) ?? 0,
            emphasis:
              id === activeTask
                ? 'focus'
                : (chain && !chain.has(id)) || !highlighted
                  ? 'dim'
                  : chain
                    ? 'chain'
                    : 'normal',
            match: matches.has(id),
            vertical: true,
          },
        } satisfies TaskFlowNode)
      }
    }
    return list
  }, [layout, grid, clustering, model, index, selected, far, chain, highlight, activeTask, matches])

  const edges = useMemo<Edge[]>(() => {
    // Zoomed out, the tiles are all there is.
    if (far) return []
    const upstream = activeTask ? walk(activeTask, 'up', model.tasks, model.dependents) : undefined
    const downstream = activeTask ? walk(activeTask, 'down', model.tasks, model.dependents) : undefined
    const base = clustering.clusters.flatMap((cluster) => cluster.edges)
    // A task's links to other requirements appear while it is in focus.
    const touches = (edge: { source: string; target: string }, id?: string) =>
      Boolean(id) && (edge.source === id || edge.target === id)
    const cross = clustering.crossEdges.filter((edge) => touches(edge, activeTask) || touches(edge, hoverTask))
    return [...base, ...cross].map((edge) => {
      const source = model.tasks.get(edge.source)
      const target = model.tasks.get(edge.target)
      const delivers = edge.target.startsWith('doc:')
      let up = false
      let down = false
      if (activeTask && upstream && downstream) {
        if (delivers) down = edge.source === activeTask || downstream.has(edge.source)
        else {
          up = (edge.target === activeTask || upstream.has(edge.target)) && upstream.has(edge.source)
          down = (edge.source === activeTask || downstream.has(edge.source)) && downstream.has(edge.target)
        }
      }
      const lit = up || down
      const hovered = !lit && touches(edge, hoverTask)
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
      const dim = Boolean(activeTask) && !lit && !hovered
      return {
        id: edge.id,
        source: edge.source,
        target: edge.target,
        // Work in flight flows: edges out of running tasks, and the traced
        // path of a clicked task.
        animated: !dim && (source?.status === 'running' || lit),
        markerEnd: delivers ? undefined : { type: MarkerType.ArrowClosed, color, width: 14, height: 14 },
        style: {
          stroke: color,
          strokeWidth: lit || hovered ? 2.25 : 1.25,
          strokeDasharray: delivers || (source && DONE.has(source.status)) ? '4 5' : undefined,
          opacity: dim ? 0.12 : 1,
        },
        zIndex: lit || hovered ? 1 : 0,
      }
    })
  }, [far, clustering, model, activeTask, hoverTask])

  // Tiles and cards anchor edges differently, so handles are re-measured when
  // the zoom crosses the threshold.
  // biome-ignore lint/correctness/useExhaustiveDependencies: re-measure on a mode switch only
  useEffect(() => {
    requestAnimationFrame(() => updateNodeInternals(nodes.map((node) => node.id)))
  }, [far])

  // Crossing the threshold by zooming swaps tile grid and clusters; the
  // requirement under the pointer (or the middle of the pane) stays put.
  // biome-ignore lint/correctness/useExhaustiveDependencies: swap on a threshold crossing only
  useEffect(() => {
    if (flying.current || !layout) return
    const { x, y, zoom } = flow.getViewport()
    const pane = document.querySelector('.react-flow')?.getBoundingClientRect()
    const anchor = pointer.current ?? { x: (pane?.width ?? 0) / 2, y: (pane?.height ?? 0) / 2 }
    const point = { x: (anchor.x - x) / zoom, y: (anchor.y - y) / zoom }
    const from = rawFar ? layout.frames : grid
    const to = rawFar ? grid : layout.frames
    const id = nearest(from, point)
    const source = id ? from.get(id) : undefined
    const target = id ? to.get(id) : undefined
    if (!source || !target) return
    const u = Math.min(1, Math.max(0, (point.x - source.x) / source.width))
    const v = Math.min(1, Math.max(0, (point.y - source.y) / source.height))
    const mapped = { x: target.x + u * target.width, y: target.y + v * target.height }
    void flow.setViewport({ x: anchor.x - mapped.x * zoom, y: anchor.y - mapped.y * zoom, zoom })
  }, [rawFar])

  // Programmatic camera moves keep cluster mode for their duration, then
  // hand back to the zoom level.
  function fly(move: () => Promise<unknown>) {
    flying.current = true
    setForceNear(true)
    let done = false
    const release = () => {
      if (done) return
      done = true
      flying.current = false
      setForceNear(false)
    }
    // Released when the camera arrives (rendering the cluster can delay the
    // start), with a safety net in case the animation is interrupted.
    requestAnimationFrame(() => void move().finally(release))
    setTimeout(release, 2500)
  }
  function paneSize() {
    const pane = document.querySelector('.react-flow')?.getBoundingClientRect()
    return { width: pane?.width ?? 1200, height: pane?.height ?? 800 }
  }
  // A tile expands into its cluster: the camera starts with the cluster
  // where the tile was on screen, then frames it at a readable zoom.
  function flyTo(id: string) {
    const frame = layout?.frames.get(id)
    if (!frame) return
    const cell = grid.get(id)
    const { width, height } = paneSize()
    const zoomTo = Math.min(1, Math.max(TILE_ZOOM + 0.08, Math.min(width / frame.width, height / frame.height) * 0.92))
    const target = {
      x: width / 2 - (frame.x + frame.width / 2) * zoomTo,
      y: height / 2 - (frame.y + frame.height / 2) * zoomTo,
      zoom: zoomTo,
    }
    if (far && cell) {
      const { x, y, zoom } = flow.getViewport()
      const start = (cell.width * zoom) / frame.width
      void flow.setViewport({
        x: x + cell.x * zoom - frame.x * start,
        y: y + cell.y * zoom - frame.y * start,
        zoom: start,
      })
    }
    fly(() => flow.setViewport(target, { duration: 650 }))
  }
  function centre(id: string) {
    const position = layout?.positions.get(id)
    if (!position) return
    const { width, height } = paneSize()
    const zoom = Math.max(flow.getZoom(), 0.85)
    fly(() =>
      flow.setViewport(
        {
          x: width / 2 - (position.x + CARD_WIDTH / 2) * zoom,
          y: height / 2 - (position.y + CARD_HEIGHT / 2) * zoom,
          zoom,
        },
        { duration: 450 },
      ),
    )
  }
  // Back to the whole map: the tile grid, fitted to the pane.
  function overview() {
    const { width, height } = paneSize()
    let right = 0
    let bottom = 0
    for (const cell of grid.values()) {
      right = Math.max(right, cell.x + cell.width)
      bottom = Math.max(bottom, cell.y + cell.height)
    }
    if (!right) return
    const zoom = Math.min(TILE_ZOOM * 0.95, Math.min(width / right, height / bottom) * 0.94)
    flying.current = true
    void flow.setViewport({ x: (width - right * zoom) / 2, y: Math.max(16, (height - bottom * zoom) / 2), zoom })
    setTimeout(() => {
      flying.current = false
    }, 60)
  }

  // Selecting a requirement flies to it; focusing a task centres it.
  // biome-ignore lint/correctness/useExhaustiveDependencies: frame once per new layout
  useEffect(() => {
    if (!laidOutKey) return
    requestAnimationFrame(() => {
      if (focus) centre(focus)
      else if (selected) flyTo(selected)
    })
  }, [laidOutKey])
  // biome-ignore lint/correctness/useExhaustiveDependencies: fly on a new selection only
  useEffect(() => {
    if (selected && laidOutKey && !focus) requestAnimationFrame(() => flyTo(selected))
  }, [selected])
  // biome-ignore lint/correctness/useExhaustiveDependencies: centre on a new focus only
  useEffect(() => {
    if (focus && laidOutKey) requestAnimationFrame(() => centre(focus))
  }, [focus])

  const theme = document.documentElement.dataset.theme
  const palette = useMemo(() => {
    const style = getComputedStyle(document.documentElement)
    const read = (name: string) => style.getPropertyValue(name).trim() || '#888'
    return {
      failure: read('--color-failure'),
      attention: read('--color-attention-dot'),
      primary: read('--color-primary'),
      positive: read('--color-positive'),
      edge: read('--color-edge'),
      raised: read('--color-raised'),
    }
    // biome-ignore lint/correctness/useExhaustiveDependencies: the palette follows the theme
  }, [theme])

  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      // The map opens on the tile grid, framed once its nodes are measured.
      defaultViewport={{ x: 0, y: 0, zoom: 0.1 }}
      fitView
      fitViewOptions={{ padding: 0.04, maxZoom: TILE_ZOOM * 0.95 }}
      onMouseMove={(event) => {
        const rect = event.currentTarget.getBoundingClientRect()
        pointer.current = { x: event.clientX - rect.left, y: event.clientY - rect.top }
      }}
      onMouseLeave={() => {
        pointer.current = undefined
      }}
      colorMode={theme === 'dark' ? 'dark' : 'light'}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      minZoom={0.04}
      maxZoom={1.6}
      onNodeMouseEnter={(_, node) => {
        clearTimeout(hoverTimer.current)
        if (node.type === 'task') hoverTimer.current = setTimeout(() => setHoverTask(node.id), 140)
      }}
      onNodeMouseLeave={() => {
        clearTimeout(hoverTimer.current)
        setHoverTask(undefined)
      }}
      onNodeClick={(event, node) => {
        if (node.type === 'tile') {
          // Clicking a tile always flies to it, even when it is already selected.
          const id = node.id.slice('frame:'.length)
          onSelectDoc(id)
          // A selection change flies there itself; re-clicking flies again.
          if (id === selected) flyTo(id)
          return
        }
        // A double-click's second click leaves the selection to the
        // double-click, which opens the task (AC-2.5).
        if (node.type === 'task' && event.detail < 2) onFocus(node.id === focus ? undefined : node.id)
      }}
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
      <Controls showInteractive={false} showFitView={false} position="top-right">
        <ControlButton onClick={overview} title="Whole map" aria-label="Whole map">
          <Maximize2 />
        </ControlButton>
      </Controls>
      <MiniMap
        pannable
        zoomable
        position="bottom-right"
        nodeBorderRadius={12}
        nodeColor={(node) => {
          if (node.type !== 'task') return palette.raised
          switch ((node as TaskFlowNode).data.task.status) {
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
        }}
        maskColor="rgba(0, 0, 0, 0.35)"
      />
    </ReactFlow>
  )
}

function nearest(
  rects: Map<string, { x: number; y: number; width: number; height: number }>,
  point: { x: number; y: number },
) {
  let best: string | undefined
  let bestDistance = Number.POSITIVE_INFINITY
  for (const [id, rect] of rects) {
    const dx = Math.max(rect.x - point.x, 0, point.x - (rect.x + rect.width))
    const dy = Math.max(rect.y - point.y, 0, point.y - (rect.y + rect.height))
    const distance = dx * dx + dy * dy
    if (distance < bestDistance) {
      best = id
      bestDistance = distance
    }
  }
  return best
}
