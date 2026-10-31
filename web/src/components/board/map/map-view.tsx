// Every requirement on one canvas (req-board-map REQ-1): each requirement's
// group holds its tasks flowing down into the requirement, at every zoom
// (AC-1.8). Selecting a requirement flies the camera to it.
import {
  Background,
  BackgroundVariant,
  ControlButton,
  Controls,
  type Edge,
  type EdgeProps,
  getBezierPath,
  MarkerType,
  MiniMap,
  type Node,
  type NodeHandle,
  type NodeProps,
  Position,
  ReactFlow,
  useReactFlow,
  useStore,
} from '@xyflow/react'
import { Maximize2, RotateCw } from 'lucide-react'
import { memo, useEffect, useMemo, useRef, useState } from 'react'
import type { RequirementInsightTab } from '../../../lib/types'
import { cn } from '../../../lib/utils'
import { Button } from '../../ui/button'
import { byUrgency } from './doc-map'
import { type DocIndex, hubId, mapClusters, UNATTACHED } from './docs'
import { DONE, type GraphStatus, type GraphTask, labelOf, type TaskGraphModel, walk } from './graph-model'
import { type HubFlowNode, HubNode } from './hub-node'
import { CARD_HEIGHT, CARD_WIDTH, CLUSTER_HEADER, HUB_HEIGHT, HUB_WIDTH, layoutClusters, type Rect } from './layout'
import { DOT_ZOOM, type TaskFlowNode, TaskNode, TITLE_ZOOM } from './task-node'

// The least zoom a selected requirement is shown at, where task titles read
// (AC-1.5).
const READABLE_ZOOM = 0.38
// The usual least zoom; a whole map that needs less lowers it (AC-1.9).
const MIN_ZOOM = 0.02

const GATES = new Set<GraphStatus>(['plan_gate', 'merge_gate', 'needs_you'])
// The height below a requirement's node that its insight toolbar takes.
const TOOLBAR_SPACE = 56
// The least zoom a requirement is framed at beside its insight panel: its
// node and toolbar read at full size, above the toolbar's near threshold.
const INSIGHT_ZOOM = Math.max(0.85, TITLE_ZOOM)
// The pane's foot that the minimap covers, with its margin.
const MINIMAP_SPACE = 176

// The tasks an insight panel row names, and the requirement whose group the
// rest of is dimmed (req-requirement-insights AC-1.3).
export interface InsightEmphasis {
  requirementId: string
  taskIds: string[]
}

type FrameData = {
  title: string
  open: number
  total: number
  merged: number
  blocked: number
  gates: number
  selected: boolean
  dim: boolean
}
type FrameFlowNode = Node<FrameData, 'frame'>

// The frame around a requirement's group: its title, a mark when it holds a
// blocked task or else a task needing judgment, and its open count (AC-1.3).
const FrameNode = memo(function FrameNode({ data, width = 0, height = 0 }: NodeProps<FrameFlowNode>) {
  const dot = data.blocked ? 'bg-failure' : data.gates ? 'bg-attention-dot' : undefined
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
})

const nodeTypes = { frame: FrameNode, hub: HubNode, task: TaskNode }

// Map edges take no pointer input (React Flow marks them inactive), so each
// draws as one bezier path with its marker and style, without the invisible
// interaction stroke React Flow's default edge adds (req-board-map AC-3.2).
const MapEdge = memo(function MapEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style,
  markerEnd,
}: EdgeProps) {
  const [path] = getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition })
  return <path id={id} className="react-flow__edge-path" d={path} fill="none" style={style} markerEnd={markerEnd} />
})
const edgeTypes = { map: MapEdge }

// A map that could not be read or laid out says so in words, in place of the
// canvas, and offers Retry (component-web-task-surfaces, Board map Read and
// Layout failure).
export function MapFailure({
  title,
  detail,
  retrying = false,
  onRetry,
}: {
  title: string
  detail: string
  retrying?: boolean
  onRetry: () => void
}) {
  return (
    <div role="alert" className="m-6 max-w-md">
      <p className="text-sm font-medium text-foreground">{title}</p>
      <p className="mt-1 text-sm text-muted">{detail}</p>
      <Button size="sm" variant="outline" className="mt-3" disabled={retrying} onClick={onRetry}>
        <RotateCw />
        Retry
      </Button>
    </div>
  )
}

export const LAYOUT_FAILURE = {
  title: 'Could not lay out the map',
  detail: 'The layout engine stopped before placing the tasks. Retry lays the map out again.',
}

export function MapView({
  model,
  index,
  visible,
  highlight,
  matches,
  selected,
  focus,
  insight,
  emphasis,
  onSelectDoc,
  onInsight,
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
  // The insight tab the panel beside the map shows for the selected
  // requirement; absent while no insight panel is shown.
  insight?: RequirementInsightTab
  // The tasks a hovered insight panel row names.
  emphasis?: InsightEmphasis
  onSelectDoc: (id: string) => void
  onInsight: (id: string, tab: RequirementInsightTab | undefined) => void
  onClear: () => void
  onFocus: (id: string | undefined) => void
  onOpen: (id: string) => void
}) {
  const flow = useReactFlow()
  // Dots and cards anchor edges at different points.
  const dots = useStore((state) => state.transform[2] < DOT_ZOOM)
  const [hoverTask, setHoverTask] = useState<string>()

  const clustering = useMemo(() => mapClusters(visible, model, index), [visible, model, index])
  // Most urgent first; the stable sort keeps exact ties in cluster order
  // (AC-1.3).
  const ordered = useMemo(() => {
    const tasksOf = (cluster: (typeof clustering.clusters)[number]) => ({
      tasks: cluster.taskIds.map((id) => model.tasks.get(id)).filter((task): task is GraphTask => Boolean(task)),
    })
    return [...clustering.clusters].sort((a, b) => byUrgency(tasksOf(a), tasksOf(b)))
  }, [clustering, model])
  // The key names the order as well as the contents, so a change in urgency
  // that reorders the groups lays the map out again.
  const layoutKey = useMemo(
    () =>
      ordered
        .map(
          (cluster) =>
            `${cluster.id}:${[...cluster.taskIds].sort().join(',')}:${cluster.edges
              .map((edge) => `${edge.source}>${edge.target}`)
              .sort()
              .join(',')}`,
        )
        .join('|'),
    [ordered],
  )
  const [layout, setLayout] = useState<Awaited<ReturnType<typeof layoutClusters>>>()
  const [laidOutKey, setLaidOutKey] = useState('')
  // The key whose layout rejected, and Retry's count of fresh attempts.
  const [failedKey, setFailedKey] = useState<string>()
  const [attempt, setAttempt] = useState(0)
  const latestKey = useRef(layoutKey)
  // The layout is requested while the map renders, so it leaves before React
  // Flow mounts; layoutClusters reuses a request in flight for the same key
  // and attempt (AC-3.2).
  // biome-ignore lint/correctness/useExhaustiveDependencies: the key names everything laid out; Retry lays it out again
  const requested = useMemo(
    () => (ordered.length === 0 ? undefined : layoutClusters(`${attempt}|${layoutKey}`, ordered)),
    [layoutKey, attempt],
  )
  useEffect(() => {
    latestKey.current = layoutKey
    if (!requested) return
    // A result or a failure for a layout since replaced is ignored.
    requested.then(
      (next) => {
        if (latestKey.current !== layoutKey) return
        setLayout(next)
        setLaidOutKey(layoutKey)
        setFailedKey(undefined)
      },
      () => {
        if (latestKey.current === layoutKey) setFailedKey(layoutKey)
      },
    )
  }, [layoutKey, requested])

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
  // A hovered insight row marks its named tasks and dims the rest of its
  // requirement's group with the tracing treatment (AC-2.3), independently of
  // the map's own pointer hover, which dims nothing (AC-2.6).
  const emphasized = useMemo(
    () => (emphasis && !activeTask ? new Set(emphasis.taskIds) : undefined),
    [emphasis, activeTask],
  )

  const nodes = useMemo<Node[]>(() => {
    if (!layout) return []
    const list: Node[] = []
    for (const cluster of clustering.clusters) {
      const frame = layout.frames.get(cluster.id)
      if (!frame) continue
      const tasks = cluster.taskIds.map((id) => model.tasks.get(id)).filter(Boolean)
      const counts: Partial<Record<GraphStatus, number>> = {}
      for (const task of tasks) if (task) counts[task.status] = (counts[task.status] ?? 0) + 1
      const stats = index.stats.get(cluster.id)
      const touched = !chain || cluster.taskIds.some((id) => chain.has(id))
      const rowGroup = emphasized && emphasis?.requirementId === cluster.id
      list.push({
        id: `frame:${cluster.id}`,
        type: 'frame',
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
        },
      } satisfies FrameFlowNode)
      if (cluster.hubId) {
        const position = layout.positions.get(cluster.hubId)
        const doc = index.docs.get(cluster.id)
        if (position && doc) {
          list.push({
            id: cluster.hubId,
            type: 'hub',
            position,
            width: HUB_WIDTH,
            height: HUB_HEIGHT,
            handles: hubHandles,
            data: {
              doc: { ...doc, kind: index.kind },
              stats: stats ?? { total: 0, merged: 0, open: 0, byStatus: {} },
              // The node is outlined while its insight panel is open.
              emphasis: chain && !touched ? 'dim' : insight && cluster.id === selected ? 'focus' : 'normal',
              vertical: true,
              insight: {
                tab: cluster.id === selected ? insight : undefined,
                onOpen: (tab) => onInsight(cluster.id, tab),
              },
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
          width: CARD_WIDTH,
          height: CARD_HEIGHT,
          handles: dots ? dotHandles : cardHandles,
          data: {
            task,
            unblocks: model.unblocks.get(id) ?? 0,
            emphasis:
              id === activeTask
                ? 'focus'
                : emphasized?.has(id)
                  ? 'chain'
                  : (chain && !chain.has(id)) || (rowGroup && !emphasized?.has(id)) || !highlighted
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
  }, [
    layout,
    clustering,
    model,
    index,
    selected,
    chain,
    highlight,
    activeTask,
    matches,
    dots,
    insight,
    onInsight,
    emphasis,
    emphasized,
  ])

  // The row group's tasks a hovered insight row does not name.
  const rowDimmed = useMemo(() => {
    if (!emphasized || !emphasis) return undefined
    const cluster = clustering.clusters.find((entry) => entry.id === emphasis.requirementId)
    return new Set((cluster?.taskIds ?? []).filter((id) => !emphasized.has(id)))
  }, [emphasized, emphasis, clustering])

  const edges = useMemo<Edge[]>(() => {
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
      const dim =
        (Boolean(activeTask) && !lit && !hovered) ||
        Boolean(rowDimmed && (rowDimmed.has(edge.source) || rowDimmed.has(edge.target)))
      return {
        id: edge.id,
        type: 'map',
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
  }, [clustering, model, activeTask, hoverTask, rowDimmed])

  // The zoom that fits every group in the pane. A group too tall for the
  // usual least zoom, such as one long chain, lowers the least zoom to it, so
  // the whole map always fits (AC-1.9).
  const paneWidth = useStore((state) => state.width)
  const paneHeight = useStore((state) => state.height)
  const bounds = useMemo(() => enclosing(layout?.frames.values() ?? []), [layout])
  const fit = bounds && paneWidth && paneHeight ? fitZoom(bounds, paneWidth, paneHeight) : undefined
  const minZoom = Math.min(MIN_ZOOM, fit ?? MIN_ZOOM)

  function paneSize() {
    const pane = document.querySelector('.react-flow')?.getBoundingClientRect()
    return { width: pane?.width ?? 1200, height: pane?.height ?? 800 }
  }
  // A selected requirement's group, at a zoom where its task titles read
  // (AC-1.5). A group taller than the pane starts at its top.
  function flyTo(id: string) {
    const frame = layout?.frames.get(id)
    if (!frame) return
    const { width, height } = paneSize()
    const zoom = Math.min(1, Math.max(READABLE_ZOOM, Math.min(width / frame.width, height / frame.height) * 0.92))
    const y = frame.height * zoom <= height ? height / 2 - (frame.y + frame.height / 2) * zoom : 16 - frame.y * zoom
    void flow.setViewport({ x: width / 2 - (frame.x + frame.width / 2) * zoom, y, zoom }, { duration: 650 })
  }
  // A requirement whose insight panel opens: its node and toolbar inside the
  // map area the panel leaves visible, at a zoom where the toolbar renders
  // (AC-1.1). The node sits low in the pane, above the minimap, so its tasks
  // show above it; a tall group's top-aligned flight would instead leave the
  // node below the pane.
  function frameHub(id: string) {
    const position = layout?.positions.get(hubId(id))
    if (!position) return
    const { width, height } = paneSize()
    const zoom = Math.min(1, Math.max(flow.getZoom(), INSIGHT_ZOOM))
    const below = (HUB_HEIGHT + TOOLBAR_SPACE) * zoom
    const y = height >= below + MINIMAP_SPACE + 48 ? height - MINIMAP_SPACE - below : (height - below) / 2
    void flow.setViewport(
      { x: width / 2 - (position.x + HUB_WIDTH / 2) * zoom, y: y - position.y * zoom, zoom },
      { duration: 450 },
    )
  }
  function centre(id: string) {
    const position = layout?.positions.get(id)
    if (!position) return
    const { width, height } = paneSize()
    const zoom = Math.max(flow.getZoom(), 0.85)
    void flow.setViewport(
      {
        x: width / 2 - (position.x + CARD_WIDTH / 2) * zoom,
        y: height / 2 - (position.y + CARD_HEIGHT / 2) * zoom,
        zoom,
      },
      { duration: 450 },
    )
  }
  // The whole map: every group inside the pane (AC-1.9).
  function overview(duration?: number) {
    if (!bounds) return
    const { width, height } = paneSize()
    const zoom = fitZoom(bounds, width, height)
    void flow.setViewport(
      {
        x: (width - bounds.width * zoom) / 2 - bounds.x * zoom,
        y: (height - bounds.height * zoom) / 2 - bounds.y * zoom,
        zoom,
      },
      { duration },
    )
  }

  // The first layout opens on the whole map unless the address names a task
  // or a requirement; selecting a requirement flies to it; focusing a task
  // centres it.
  const framed = useRef(false)
  // biome-ignore lint/correctness/useExhaustiveDependencies: frame once per new layout
  useEffect(() => {
    if (!laidOutKey) return
    const first = !framed.current
    framed.current = true
    requestAnimationFrame(() => {
      if (focus) centre(focus)
      else if (selected && insight) frameHub(selected)
      else if (selected) flyTo(selected)
      else if (first) overview()
    })
  }, [laidOutKey])
  // biome-ignore lint/correctness/useExhaustiveDependencies: fly on a new selection only
  useEffect(() => {
    if (selected && laidOutKey && !focus && !insight) requestAnimationFrame(() => flyTo(selected))
  }, [selected])
  // An insight panel that opens, or a pane the panel resizes, reframes the
  // requirement's node beside the panel.
  const insightShown = Boolean(insight)
  useEffect(() => {
    if (selected && insightShown && laidOutKey && !focus) requestAnimationFrame(() => frameHub(selected))
  }, [selected, insightShown, paneWidth, paneHeight])
  // biome-ignore lint/correctness/useExhaustiveDependencies: centre on a new focus only
  useEffect(() => {
    if (focus && laidOutKey) requestAnimationFrame(() => centre(focus))
  }, [focus])

  // The minimap mounts one frame after the first laid-out map renders, so its
  // nodes stay out of the commit that first shows the map (AC-3.2).
  const [miniMap, setMiniMap] = useState(false)
  useEffect(() => {
    if (!laidOutKey || miniMap) return
    const frame = requestAnimationFrame(() => setMiniMap(true))
    return () => cancelAnimationFrame(frame)
  }, [laidOutKey, miniMap])

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

  if (failedKey === layoutKey)
    return (
      <MapFailure
        {...LAYOUT_FAILURE}
        onRetry={() => {
          setFailedKey(undefined)
          setAttempt((count) => count + 1)
        }}
      />
    )
  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      edgeTypes={edgeTypes}
      // The first layout frames itself (overview, centre, or flyTo).
      defaultViewport={{ x: 0, y: 0, zoom: 0.1 }}
      colorMode={theme === 'dark' ? 'dark' : 'light'}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      minZoom={minZoom}
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
        if (node.type === 'frame') {
          // Clicking a group always flies to it, even when it is already selected.
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
        <ControlButton onClick={() => overview(400)} title="Whole map" aria-label="Whole map">
          <Maximize2 />
        </ControlButton>
      </Controls>
      {miniMap && (
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
      )}
    </ReactFlow>
  )
}

// Task and hub nodes state their sizes and handles, so edges draw in the
// first render rather than after React Flow measures 500 nodes (AC-3.2).
// Cards connect top to bottom; a dot connects at its centre.
const HANDLE = 4
function handle(type: NodeHandle['type'], width: number, y: number): NodeHandle {
  const position = type === 'target' ? Position.Top : Position.Bottom
  return { type, position, x: width / 2 - HANDLE / 2, y, width: HANDLE, height: HANDLE }
}
const cardHandles = [handle('target', CARD_WIDTH, -HANDLE / 2), handle('source', CARD_WIDTH, CARD_HEIGHT - HANDLE / 2)]
const dotHandles = [
  handle('target', CARD_WIDTH, CARD_HEIGHT / 2 - HANDLE / 2),
  handle('source', CARD_WIDTH, CARD_HEIGHT / 2 - HANDLE * 1.5),
]
const hubHandles = [handle('target', HUB_WIDTH, -HANDLE / 2)]

function fitZoom(bounds: Rect, width: number, height: number) {
  return Math.min(1, Math.min(width / bounds.width, height / bounds.height) * 0.94)
}

function enclosing(rects: Iterable<Rect>): Rect | undefined {
  let bounds: { left: number; top: number; right: number; bottom: number } | undefined
  for (const rect of rects) {
    bounds = {
      left: Math.min(bounds?.left ?? rect.x, rect.x),
      top: Math.min(bounds?.top ?? rect.y, rect.y),
      right: Math.max(bounds?.right ?? rect.x + rect.width, rect.x + rect.width),
      bottom: Math.max(bounds?.bottom ?? rect.y + rect.height, rect.y + rect.height),
    }
  }
  if (!bounds) return undefined
  return { x: bounds.left, y: bounds.top, width: bounds.right - bounds.left, height: bounds.bottom - bounds.top }
}
