// ELK's layered layout, run in a web worker so a few hundred tasks never stall
// the page (DEC-76).
import ELK from 'elkjs/lib/elk-api'
import workerUrl from 'elkjs/lib/elk-worker.min.js?url'

export const CARD_WIDTH = 264
export const CARD_HEIGHT = 80
export const HUB_WIDTH = 300
export const HUB_HEIGHT = 132

// The worker starts when the map's code loads, and a two-node layout compiles
// its layered code, so the first real layout does not wait for either.
const elk = new ELK({ workerUrl })
void elk.layout({
  id: 'warm',
  layoutOptions: { 'elk.algorithm': 'layered', 'elk.layered.nodePlacement.strategy': 'NETWORK_SIMPLEX' },
  children: [
    { id: 'a', width: CARD_WIDTH, height: CARD_HEIGHT },
    { id: 'b', width: CARD_WIDTH, height: CARD_HEIGHT },
  ],
  edges: [{ id: 'a->b', sources: ['a'], targets: ['b'] }],
})

export interface LayoutEdge {
  id: string
  source: string
  target: string
}

export interface LayoutNode {
  id: string
  width: number
  height: number
}

export async function layoutGraph(nodes: LayoutNode[], edges: LayoutEdge[], direction: 'RIGHT' | 'DOWN' = 'RIGHT') {
  const result = await elk.layout({
    id: 'root',
    layoutOptions: {
      'elk.algorithm': 'layered',
      'elk.direction': direction,
      'elk.layered.spacing.nodeNodeBetweenLayers': direction === 'DOWN' ? '56' : '64',
      'elk.spacing.edgeNode': '16',
      'elk.spacing.nodeNode': '18',
      'elk.layered.nodePlacement.strategy': 'NETWORK_SIMPLEX',
      'elk.layered.crossingMinimization.strategy': 'LAYER_SWEEP',
      'elk.layered.considerModelOrder.strategy': 'NODES_AND_EDGES',
      'elk.separateConnectedComponents': 'true',
      'elk.spacing.componentComponent': '72',
      'elk.aspectRatio': '1.6',
    },
    children: nodes,
    edges: edges.map((edge) => ({ id: edge.id, sources: [edge.source], targets: [edge.target] })),
  })
  const positions = new Map<string, { x: number; y: number }>()
  for (const child of result.children ?? []) positions.set(child.id, { x: child.x ?? 0, y: child.y ?? 0 })
  return positions
}

export interface ClusterInput {
  id: string
  hubId?: string
  taskIds: string[]
  edges: LayoutEdge[]
}

export const CLUSTER_HEADER = 72
// The narrowest frame still holds its header's title and open count.
export const FRAME_MIN_WIDTH = 560
const FRAME_GAP = 96
const FRAME_PADDING = 24
const CANVAS_ASPECT = 1.7

export interface Rect {
  x: number
  y: number
  width: number
  height: number
}

// The map: each document's tasks flow top to bottom into its hub, and the
// frames are placed in the order given (req-board-map AC-1.3).
export async function layoutClusters(clusters: ClusterInput[]) {
  const result = await elk.layout({
    id: 'root',
    // ELK only sizes the frames; placeFrames places them.
    layoutOptions: { 'elk.algorithm': 'fixed' },
    children: clusters.map((cluster) => ({
      id: `frame:${cluster.id}`,
      layoutOptions: {
        'elk.algorithm': 'layered',
        'elk.direction': 'DOWN',
        'elk.padding': `[top=${CLUSTER_HEADER},left=32,bottom=32,right=32]`,
        'elk.layered.spacing.nodeNodeBetweenLayers': '56',
        'elk.spacing.nodeNode': '18',
        'elk.layered.nodePlacement.strategy': 'NETWORK_SIMPLEX',
        'elk.separateConnectedComponents': 'false',
        'elk.nodeSize.constraints': 'MINIMUM_SIZE',
        'elk.nodeSize.minimum': `(${FRAME_MIN_WIDTH}, 0)`,
        'elk.contentAlignment': 'H_CENTER V_TOP',
      },
      children: [
        ...cluster.taskIds.map((id) => ({ id, width: CARD_WIDTH, height: CARD_HEIGHT })),
        ...(cluster.hubId ? [{ id: cluster.hubId, width: HUB_WIDTH, height: HUB_HEIGHT }] : []),
      ],
      edges: cluster.edges.map((edge) => ({ id: edge.id, sources: [edge.source], targets: [edge.target] })),
    })),
  })
  const sized = (result.children ?? []).map((frame) => ({
    id: frame.id.slice('frame:'.length),
    width: frame.width ?? 0,
    height: frame.height ?? 0,
    children: frame.children ?? [],
  }))
  const placed = placeFrames(sized)
  const positions = new Map<string, { x: number; y: number }>()
  const frames = new Map<string, Rect>()
  sized.forEach((frame, index) => {
    const { x, y } = placed[index]
    frames.set(frame.id, { x, y, width: frame.width, height: frame.height })
    for (const child of frame.children) positions.set(child.id, { x: x + (child.x ?? 0), y: y + (child.y ?? 0) })
  })
  return { positions, frames }
}

// Rows in the given order, left to right and top to bottom, so no frame
// splits across rows or overlaps another. The row width is the widest frame or
// the width of some run of consecutive frames: whichever brings the canvas
// closest to 1.7 times wider than tall.
export function placeFrames(sizes: Array<{ width: number; height: number }>) {
  const widths = new Set([sizes.reduce((max, size) => Math.max(max, size.width), 0)])
  for (let first = 0; first < sizes.length; first++) {
    let run = -FRAME_GAP
    for (let last = first; last < sizes.length; last++) {
      run += sizes[last].width + FRAME_GAP
      widths.add(run)
    }
  }
  let best: { placed: Array<{ x: number; y: number }>; score: number } | undefined
  for (const rowWidth of widths) {
    const { placed, width, height } = placeRows(sizes, rowWidth)
    const score = Math.abs(Math.log(width / Math.max(height, 1) / CANVAS_ASPECT))
    if (!best || score < best.score) best = { placed, score }
  }
  return best?.placed ?? []
}

function placeRows(sizes: Array<{ width: number; height: number }>, rowWidth: number) {
  const placed: Array<{ x: number; y: number }> = []
  let x = 0
  let y = 0
  let rowHeight = 0
  let width = 0
  for (const size of sizes) {
    if (x > 0 && x + size.width > rowWidth) {
      x = 0
      y += rowHeight + FRAME_GAP
      rowHeight = 0
    }
    placed.push({ x: FRAME_PADDING + x, y: FRAME_PADDING + y })
    width = Math.max(width, x + size.width)
    x += size.width + FRAME_GAP
    rowHeight = Math.max(rowHeight, size.height)
  }
  return { placed, width, height: y + rowHeight }
}
