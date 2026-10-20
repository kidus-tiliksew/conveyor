// Every requirement on one canvas (req-board-map REQ-1): each requirement's
// group holds its tasks flowing down into the requirement, at every zoom
// (AC-1.8). Selecting a requirement flies the camera to it.
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
import { CARD_HEIGHT, CARD_WIDTH, CLUSTER_HEADER, layoutClusters, type Rect } from './layout'
import { DOT_ZOOM, type TaskFlowNode, TaskNode } from './task-node'

// The least zoom a selected requirement is shown at, where task titles read
// (AC-1.5).
const READABLE_ZOOM = 0.38
// Low enough that a large workspace's whole map still fits the pane.
const MIN_ZOOM = 0.02

const GATES = new Set<GraphStatus>(['plan_gate', 'merge_gate', 'needs_you'])

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
  // Dots and cards anchor edges differently, so handles are re-measured when
  // the zoom crosses the dot threshold.
  const dots = useStore((state) => state.transform[2] < DOT_ZOOM)
  const updateNodeInternals = useUpdateNodeInternals()
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
  const latestKey = useRef(layoutKey)
  useEffect(() => {
    latestKey.current = layoutKey
    if (ordered.length === 0) return
    void layoutClusters(ordered).then((next) => {
      if (latestKey.current !== layoutKey) return
      setLayout(next)
      setLaidOutKey(layoutKey)
    })
    // biome-ignore lint/correctness/useExhaustiveDependencies: the key names everything laid out
  }, [layoutKey])

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
      const frame = layout.frames.get(cluster.id)
      if (!frame) continue
      const tasks = cluster.taskIds.map((id) => model.tasks.get(id)).filter(Boolean)
      const counts: Partial<Record<GraphStatus, number>> = {}
      for (const task of tasks) if (task) counts[task.status] = (counts[task.status] ?? 0) + 1
      const stats = index.stats.get(cluster.id)
      const touched = !chain || cluster.taskIds.some((id) => chain.has(id))
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
  }, [layout, clustering, model, index, selected, chain, highlight, activeTask, matches])

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
  }, [clustering, model, activeTask, hoverTask])

  // biome-ignore lint/correctness/useExhaustiveDependencies: re-measure on a mode switch only
  useEffect(() => {
    requestAnimationFrame(() => updateNodeInternals(nodes.map((node) => node.id)))
  }, [dots])

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
    const bounds = enclosing(layout?.frames.values() ?? [])
    if (!bounds) return
    const { width, height } = paneSize()
    const zoom = Math.max(MIN_ZOOM, Math.min(1, Math.min(width / bounds.width, height / bounds.height) * 0.94))
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
      else if (selected) flyTo(selected)
      else if (first) overview()
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
      // The first layout frames itself (overview, centre, or flyTo).
      defaultViewport={{ x: 0, y: 0, zoom: 0.1 }}
      colorMode={theme === 'dark' ? 'dark' : 'light'}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      minZoom={MIN_ZOOM}
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
