// ELK's layered layout, run in a web worker so a few hundred tasks never stall
// the page (DEC-76).
import ELK from 'elkjs/lib/elk-api'
import workerUrl from 'elkjs/lib/elk-worker.min.js?url'

export const CARD_WIDTH = 264
export const CARD_HEIGHT = 80
export const HUB_WIDTH = 300
export const HUB_HEIGHT = 132

let elk: InstanceType<typeof ELK> | undefined

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
  elk ??= new ELK({ workerUrl })
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

// The map: each document's tasks flow top to bottom into its hub, and the
// clusters are packed into one canvas.
export async function layoutClusters(clusters: ClusterInput[]) {
  elk ??= new ELK({ workerUrl })
  const result = await elk.layout({
    id: 'root',
    layoutOptions: {
      'elk.algorithm': 'rectpacking',
      'elk.aspectRatio': '1.7',
      'elk.spacing.nodeNode': '96',
      'elk.padding': '[top=24,left=24,bottom=24,right=24]',
    },
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
        // A floor on every tile, so a two-task requirement still reads as a
        // tile when zoomed out.
        'elk.nodeSize.constraints': 'MINIMUM_SIZE',
        'elk.nodeSize.minimum': '(1200, 900)',
        'elk.contentAlignment': 'H_CENTER V_TOP',
      },
      children: [
        ...cluster.taskIds.map((id) => ({ id, width: CARD_WIDTH, height: CARD_HEIGHT })),
        ...(cluster.hubId ? [{ id: cluster.hubId, width: HUB_WIDTH, height: HUB_HEIGHT }] : []),
      ],
      edges: cluster.edges.map((edge) => ({ id: edge.id, sources: [edge.source], targets: [edge.target] })),
    })),
  })
  const positions = new Map<string, { x: number; y: number }>()
  const frames = new Map<string, { x: number; y: number; width: number; height: number }>()
  for (const frame of result.children ?? []) {
    const fx = frame.x ?? 0
    const fy = frame.y ?? 0
    frames.set(frame.id.slice('frame:'.length), { x: fx, y: fy, width: frame.width ?? 0, height: frame.height ?? 0 })
    for (const child of frame.children ?? [])
      positions.set(child.id, { x: fx + (child.x ?? 0), y: fy + (child.y ?? 0) })
  }
  return { positions, frames }
}
