// The crate's line in 3D: one conveyor belt that carries one block per member
// task through plan, build, review, and the merge gate, into the crate at the
// end, with queued tasks on a pallet before the belt and closed tasks set
// aside in front of it (req-crate-insights AC-1.1 to AC-1.3). A block's place
// and colour are its task's served station and phase, so the scene is a
// picture of the crate's recorded state and decides only drawing, camera,
// and motion (DEC-143(3); component-crates invariant 21).
//
// The scene is built in code from primitive shapes and ships no model,
// texture, or font file (DEC-143(1)). The station signs and the card of the
// pointed-at task are page elements placed from scene positions (DEC-143(4)).
// So is each dependency link: it shows only at the present, only while the
// emphasized task waits on a member, from the block of each member it waits
// on to the emphasized block (req-crate-insights AC-6.5; component-crates
// invariant 39; DEC-151(3), (4)).
// The page loads this file on demand, so three.js stays out of every other
// route's bundle (DEC-143(2)).
import { ContactShadows, RoundedBox } from '@react-three/drei'
import { Canvas, useFrame, useThree } from '@react-three/fiber'
import { type ReactNode, type RefObject, useEffect, useLayoutEffect, useMemo, useRef } from 'react'
import * as THREE from 'three'
import { DependencyDetails } from './dependency-details'
import { linksKey, type WaitLink } from './dependency-model'
import { type LineBlock, type LineStation, phaseMeta } from './line-model'
import type { CrateInsightTask, CratePhase, CrateStation } from '../../lib/types'

export interface LineSceneProps {
  blocks: LineBlock[]
  // The station counts the signs print: the same counts as the page's text
  // list of them.
  counts: Record<LineStation, number>
  shipped: boolean
  closed: boolean
  compact: boolean
  dark: boolean
  reducedMotion: boolean
  emphasizedId: string | null
  // The emphasized task as the insight view serves it, for what its card
  // states about its dependencies (req-crate-insights AC-6.1, AC-6.2).
  emphasizedTask?: CrateInsightTask
  // The members left undimmed while a task is emphasized: that task and the
  // members its dependencies connect it to (AC-6.6). Null with no emphasis.
  related: Set<string> | null
  // Each served `waiting` entry that names a member, at the present. Empty
  // during a replay (AC-6.5).
  links: WaitLink[]
  onEmphasize: (id: string | null) => void
  onSelect: (id: string) => void
  // Called when the browser takes the canvas's WebGL context away, so the
  // page can state the counts as text (req-crate-insights AC-1.5).
  onContextLost: () => void
  // Pixels the page overlays on the scene's top and bottom edges.
  padTop: number
  padBottom: number
}

const MODULE = 0.6
const BELT_Y = 1.0
const BELT_HALF_WIDTH = 0.82

type Zone = 'plan' | 'build' | 'review' | 'gate'
const zones: Zone[] = ['plan', 'build', 'review', 'gate']

interface Layout {
  beltFrom: number
  beltTo: number
  zones: Record<Zone, [number, number]>
  pallet: number
  crate: number
  crateSize: number
}

function layoutFor(compact: boolean): Layout {
  const k = compact ? 0.56 : 1
  return {
    beltFrom: -5.9 * k,
    beltTo: 4.3 * k,
    zones: {
      plan: [-5.5 * k, -3.5 * k],
      build: [-2.9 * k, -0.5 * k],
      review: [0.1 * k, 2.3 * k],
      gate: [2.9 * k, 4.0 * k],
    },
    pallet: -5.9 * k - (compact ? 1.05 : 1.25),
    crate: 4.3 * k + (compact ? 1.5 : 1.75),
    crateSize: compact ? 2.1 : 2.5,
  }
}

type Place = {
  position: THREE.Vector3
  kind: 'belt' | 'crate' | 'pallet' | 'aside'
  size: number
  tilt?: number
}

// Where every block sits. Belt blocks fill their zone from the end nearest
// the next station, two abreast, then stack. At the present, a member that
// waits on a member at the same station stands behind it on the belt when
// the station has room for every block in file (req-crate-insights AC-6.5).
function place(blocks: LineBlock[], layout: Layout, links: WaitLink[]): Map<string, Place> {
  const places = new Map<string, Place>()
  const by = (station: CrateStation) => blocks.filter((block) => block.station === station)

  for (const zone of zones) {
    const [from, to] = layout.zones[zone]
    const columns = Math.max(1, Math.floor((to - from) / (MODULE + 0.16)))
    const here = by(zone)
    const inFile = fileSpots(here, links, to - from)
    if (inFile) {
      for (const spot of inFile.spots)
        places.set(spot.id, {
          kind: 'belt',
          size: MODULE,
          position: new THREE.Vector3(
            to - MODULE / 2 - 0.05 - spot.column * inFile.gap,
            BELT_Y + 0.05 + MODULE / 2,
            inFile.single ? 0 : spot.row === 0 ? 0.36 : -0.36,
          ),
        })
      continue
    }
    here.forEach((block, index) => {
      const perLayer = columns * 2
      const layer = Math.floor(index / perLayer)
      const slot = index % perLayer
      const column = Math.floor(slot / 2)
      const row = slot % 2
      places.set(block.id, {
        kind: 'belt',
        size: MODULE,
        position: new THREE.Vector3(
          to - MODULE / 2 - 0.1 - column * (MODULE + 0.16),
          BELT_Y + 0.05 + MODULE / 2 + layer * (MODULE + 0.02),
          row === 0 ? 0.36 : -0.36,
        ),
      })
    })
  }

  by('intake').forEach((block, index) => {
    const layer = Math.floor(index / 4)
    const slot = index % 4
    places.set(block.id, {
      kind: 'pallet',
      size: MODULE,
      position: new THREE.Vector3(
        layout.pallet + (slot % 2 === 0 ? -0.34 : 0.34),
        0.2 + MODULE / 2 + layer * (MODULE + 0.02),
        slot < 2 ? 0.34 : -0.34,
      ),
    })
  })

  // The crate packs a square grid that grows with what it holds.
  const merged = by('crate')
  const grid = merged.length <= 18 ? 3 : merged.length <= 48 ? 4 : 5
  const inner = layout.crateSize - 0.34
  const cell = inner / grid
  const size = Math.min(MODULE, cell - 0.08)
  merged.forEach((block, index) => {
    const layer = Math.floor(index / (grid * grid))
    const slot = index % (grid * grid)
    places.set(block.id, {
      kind: 'crate',
      size,
      position: new THREE.Vector3(
        layout.crate - inner / 2 + cell / 2 + (slot % grid) * cell,
        0.2 + size / 2 + layer * (size + 0.03),
        inner / 2 - cell / 2 - Math.floor(slot / grid) * cell,
      ),
    })
  })

  // Closed tasks are set aside on the floor in front of the line.
  by('aside').forEach((block, index) => {
    places.set(block.id, {
      kind: 'aside',
      size: MODULE * 0.9,
      tilt: 0.5 + index * 0.9,
      position: new THREE.Vector3(layout.beltFrom + 0.7 + index * 0.95, MODULE * 0.45, 1.55),
    })
  })
  return places
}

// The places of one station's blocks in file: each member that no other
// member here holds back heads a file, and the members that wait on it stand
// behind it, toward the station before. Files fill the belt's two rows. Null
// when no two blocks here are linked, when a link loop leaves a block out, or
// when the files do not fit the station without stacking.
function fileSpots(here: LineBlock[], links: WaitLink[], length: number) {
  const ids = new Set(here.map((block) => block.id))
  const inZone = links.filter((link) => ids.has(link.from) && ids.has(link.to))
  if (inZone.length === 0) return null
  const waits = new Set(inZone.map((link) => link.to))
  const seen = new Set<string>()
  const walk = (id: string, file: string[]) => {
    if (seen.has(id)) return
    seen.add(id)
    file.push(id)
    for (const link of inZone) if (link.from === id) walk(link.to, file)
  }
  const files: string[][] = []
  for (const block of here) {
    if (waits.has(block.id)) continue
    const file: string[] = []
    walk(block.id, file)
    if (file.length > 0) files.push(file)
  }
  const used = [0, 0]
  const spots: Array<{ id: string; row: number; column: number }> = []
  for (const file of files) {
    const row = used[0] <= used[1] ? 0 : 1
    for (const id of file) spots.push({ id, row, column: used[row]++ })
  }
  const longest = Math.max(...used)
  const gap = longest > 1 ? Math.min(MODULE + 0.3, (length - MODULE) / (longest - 1)) : MODULE + 0.3
  if (spots.length !== here.length || gap < MODULE + 0.08) return null
  return { spots, gap, single: used[1] === 0 }
}

interface Palette {
  phase: Record<CratePhase, string>
  belt: string
  beltMark: string
  metal: string
  metalDark: string
  wood: string
  woodDark: string
  shadow: number
}

// The scene's colours: the dashboard's tokens for the current theme, with
// brighter fills for the block phases so a block reads as a lit object.
function readPalette(dark: boolean): Palette {
  const style = getComputedStyle(document.documentElement)
  const token = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback
  const phase = Object.fromEntries(
    (Object.keys(phaseMeta) as CratePhase[]).map((key) => {
      const name = phaseMeta[key].color.replace(/^var\((.*)\)$/, '$1')
      return [key, token(name, '#888')]
    }),
  ) as Record<CratePhase, string>
  phase.merged = dark ? '#4fbf86' : '#2fa56b'
  phase.building = dark ? '#7f8cf2' : '#5563e0'
  phase.review = dark ? '#a493ee' : '#8562d8'
  phase.planning = dark ? '#5fb4e0' : '#2f8fc4'
  phase.queued = dark ? '#6b7180' : '#b4b8c4'
  phase.closed = dark ? '#3d414b' : '#d5d7de'
  return {
    phase,
    belt: dark ? '#262a33' : '#3a3f4b',
    beltMark: dark ? '#353a46' : '#4b5160',
    metal: dark ? '#565c6a' : '#c9cdd6',
    metalDark: dark ? '#3a3f4a' : '#a3a8b4',
    wood: dark ? '#b08552' : '#d6a56b',
    woodDark: dark ? '#8a6539' : '#b98850',
    shadow: dark ? 0.55 : 0.3,
  }
}

const easeOut = (value: number) => 1 - (1 - value) ** 3

function bounce(value: number) {
  const n = 7.5625
  const d = 2.75
  if (value < 1 / d) return n * value * value
  if (value < 2 / d) return n * (value - 1.5 / d) ** 2 + 0.75
  if (value < 2.5 / d) return n * (value - 2.25 / d) ** 2 + 0.9375
  return n * (value - 2.625 / d) ** 2 + 0.984375
}

function Block({
  block,
  spot,
  index,
  color,
  emphasized,
  dimmed,
  reducedMotion,
  groups,
  onEmphasize,
  onSelect,
}: {
  block: LineBlock
  spot: Place
  index: number
  color: string
  emphasized: boolean
  dimmed: boolean
  reducedMotion: boolean
  groups: RefObject<Map<string, THREE.Group>>
  onEmphasize: (id: string | null) => void
  onSelect: (id: string) => void
}) {
  const group = useRef<THREE.Group>(null)
  // The card of an emphasized block follows where the block is drawn.
  useEffect(() => {
    const node = group.current
    const registry = groups.current
    if (!node) return
    registry.set(block.id, node)
    return () => {
      if (registry.get(block.id) === node) registry.delete(block.id)
    }
  }, [block.id, groups])
  const beacon = useRef<THREE.Mesh>(null)
  const lift = useRef(0)
  // One move at a time: from where the block is to its new place.
  const move = useRef<{ from: THREE.Vector3; to: THREE.Vector3; start: number; hop: number; first: boolean } | null>(
    null,
  )
  const kind = useRef(spot.kind)
  const working = block.phase === 'building' || block.phase === 'planning' || block.phase === 'review'
  const needsPerson = block.phase === 'needs_you'

  useFrame((state, delta) => {
    const node = group.current
    if (!node) return
    const time = state.clock.elapsedTime
    if (!move.current) {
      // It enters once: dropped into the crate, or carried in from the left.
      const from = spot.position.clone()
      if (spot.kind === 'crate') from.y += 4.2
      else if (spot.kind === 'belt') from.x -= 2.6
      move.current = { from, to: spot.position.clone(), start: time + 0.25 + index * 0.07, hop: 0, first: true }
    } else if (!move.current.to.equals(spot.position)) {
      // Its task moved on. Into the crate, or on and off the floor, it is
      // lifted.
      const lifted = spot.kind !== 'belt' || kind.current !== 'belt'
      move.current = {
        from: node.position.clone(),
        to: spot.position.clone(),
        start: time,
        hop: lifted ? 1.5 : 0,
        first: false,
      }
    }
    kind.current = spot.kind
    const { from, to, start, hop, first } = move.current
    // Under reduced motion every block stands at its place at once.
    const progress = reducedMotion ? 1 : THREE.MathUtils.clamp((time - start) / 0.95, 0, 1)
    const eased = first && spot.kind === 'crate' ? bounce(progress) : easeOut(progress)
    let x = THREE.MathUtils.lerp(from.x, to.x, eased)
    let y = THREE.MathUtils.lerp(from.y, to.y, eased) + Math.sin(Math.PI * progress) * hop
    const z = THREE.MathUtils.lerp(from.z, to.z, eased)
    const scale = first ? Math.max(0.001, easeOut(Math.min(1, progress * (spot.kind === 'crate' ? 8 : 2.2)))) : 1
    if (!reducedMotion && progress === 1) {
      if (spot.kind === 'belt') {
        // A carried block rides the belt a little, then settles back.
        x += Math.sin(time * 0.9 + index * 1.7) * 0.035
        if (working) y += Math.abs(Math.sin(time * 2.2 + index)) * 0.035
      }
      if (needsPerson) y += Math.sin(time * 3) * 0.03
    }
    lift.current = reducedMotion
      ? emphasized
        ? 1
        : 0
      : THREE.MathUtils.damp(lift.current, emphasized ? 1 : 0, 12, delta)
    node.position.set(x, y + lift.current * 0.22, z)
    node.scale.setScalar(scale * (1 + lift.current * 0.1))
    if (beacon.current) {
      const pulse = reducedMotion ? 0.5 : (time * 0.9) % 1
      beacon.current.scale.setScalar(0.6 + pulse * 1.1)
      ;(beacon.current.material as THREE.MeshBasicMaterial).opacity = 0.75 * (1 - pulse)
    }
  })

  return (
    <group ref={group} rotation={spot.tilt ? [0, spot.tilt, 0.12] : undefined}>
      <RoundedBox
        args={[spot.size, spot.size, spot.size]}
        radius={0.07}
        smoothness={3}
        onPointerOver={(event) => {
          event.stopPropagation()
          onEmphasize(block.id)
          document.body.style.cursor = 'pointer'
        }}
        onPointerOut={() => {
          onEmphasize(null)
          document.body.style.cursor = ''
        }}
        onClick={(event) => {
          event.stopPropagation()
          onSelect(block.id)
        }}
      >
        <meshStandardMaterial
          color={color}
          roughness={0.5}
          metalness={0.05}
          emissive={color}
          emissiveIntensity={emphasized ? 0.35 : working ? 0.12 : 0.04}
          transparent
          opacity={dimmed ? 0.32 : 1}
        />
      </RoundedBox>
      {/* A strap across the top marks the block as a packed unit of work. */}
      <mesh position={[0, spot.size / 2 + 0.003, 0]} rotation={[-Math.PI / 2, 0, 0]}>
        <planeGeometry args={[spot.size * 0.22, spot.size * 0.86]} />
        <meshBasicMaterial color="#ffffff" transparent opacity={dimmed ? 0.06 : 0.26} />
      </mesh>
      {/* The beacon over a task that waits on a person: a mark that does not
          depend on colour (req-crate-insights AC-1.2). */}
      {needsPerson && (
        <>
          <mesh position={[0, spot.size / 2 + 0.3, 0]}>
            <sphereGeometry args={[0.09, 16, 16]} />
            <meshBasicMaterial color={color} />
          </mesh>
          <mesh ref={beacon} position={[0, spot.size / 2 + 0.3, 0]} rotation={[-Math.PI / 2, 0, 0]}>
            <ringGeometry args={[0.16, 0.21, 32]} />
            <meshBasicMaterial color={color} transparent opacity={0.6} side={THREE.DoubleSide} />
          </mesh>
        </>
      )}
    </group>
  )
}

// One straight run of belt between two points on the floor plan, given as
// [x, z]. Its chevrons, drawn in code, run from `a` to `b`.
function BeltRun({
  a,
  b,
  palette,
  reducedMotion,
}: {
  a: [number, number]
  b: [number, number]
  palette: Palette
  reducedMotion: boolean
}) {
  const width = BELT_HALF_WIDTH * 2
  const length = Math.hypot(b[0] - a[0], b[1] - a[1])
  const angle = Math.atan2(b[1] - a[1], b[0] - a[0])
  const texture = useMemo(() => {
    const canvas = document.createElement('canvas')
    canvas.width = 64
    canvas.height = 64
    const context = canvas.getContext('2d') as CanvasRenderingContext2D
    context.fillStyle = palette.belt
    context.fillRect(0, 0, 64, 64)
    context.strokeStyle = palette.beltMark
    context.lineWidth = 5
    // Chevrons that point the way the belt runs.
    context.beginPath()
    context.moveTo(14, 8)
    context.lineTo(34, 32)
    context.lineTo(14, 56)
    context.stroke()
    const map = new THREE.CanvasTexture(canvas)
    map.wrapS = THREE.RepeatWrapping
    map.wrapT = THREE.RepeatWrapping
    map.repeat.set(length / 0.9, 1)
    map.colorSpace = THREE.SRGBColorSpace
    return map
  }, [palette.belt, palette.beltMark, length])
  useEffect(() => () => texture.dispose(), [texture])
  useFrame((_, delta) => {
    if (!reducedMotion) texture.offset.x -= delta * 0.35
  })
  const legs = useMemo(() => {
    const count = Math.max(1, Math.round(length / 2.4))
    return Array.from({ length: count + 1 }, (_, index) => -length / 2 + 0.3 + (index * (length - 0.6)) / count)
  }, [length])
  const half = width / 2
  return (
    <group position={[(a[0] + b[0]) / 2, 0, (a[1] + b[1]) / 2]} rotation={[0, -angle, 0]}>
      <mesh position={[0, BELT_Y, 0]}>
        <boxGeometry args={[length, 0.1, width]} />
        <meshStandardMaterial map={texture} roughness={0.9} />
      </mesh>
      {[1, -1].map((side) => (
        <mesh key={side} position={[0, BELT_Y - 0.02, side * (half + 0.06)]}>
          <boxGeometry args={[length + 0.3, 0.24, 0.12]} />
          <meshStandardMaterial color={palette.metal} roughness={0.35} metalness={0.5} />
        </mesh>
      ))}
      {legs.map((x) =>
        [1, -1].map((side) => (
          <mesh key={`${x}:${side}`} position={[x, (BELT_Y - 0.14) / 2, side * (half + 0.06)]}>
            <boxGeometry args={[0.1, BELT_Y - 0.14, 0.1]} />
            <meshStandardMaterial color={palette.metalDark} roughness={0.5} metalness={0.4} />
          </mesh>
        )),
      )}
    </group>
  )
}

function Belt({ layout, palette, reducedMotion }: { layout: Layout; palette: Palette; reducedMotion: boolean }) {
  return (
    <group>
      <BeltRun a={[layout.beltFrom, 0]} b={[layout.beltTo, 0]} palette={palette} reducedMotion={reducedMotion} />
      {[layout.beltFrom, layout.beltTo].map((x) => (
        <mesh key={x} position={[x, BELT_Y - 0.02, 0]} rotation={[Math.PI / 2, 0, 0]}>
          <cylinderGeometry args={[0.13, 0.13, BELT_HALF_WIDTH * 2 + 0.1, 20]} />
          <meshStandardMaterial color={palette.metalDark} roughness={0.4} metalness={0.5} />
        </mesh>
      ))}
      {/* The chute that drops merged blocks into the crate. */}
      <mesh position={[layout.beltTo + 0.36, BELT_Y - 0.2, 0]} rotation={[0, 0, -0.5]}>
        <boxGeometry args={[0.8, 0.06, BELT_HALF_WIDTH * 1.7]} />
        <meshStandardMaterial color={palette.metal} roughness={0.35} metalness={0.5} />
      </mesh>
    </group>
  )
}

function Arch({
  x,
  color,
  active,
  scanner,
  palette,
  reducedMotion,
}: {
  x: number
  color: string
  active: boolean
  scanner?: boolean
  palette: Palette
  reducedMotion: boolean
}) {
  const beam = useRef<THREE.Mesh>(null)
  const z = BELT_HALF_WIDTH + 0.22
  const height = 1.45
  useFrame((state) => {
    if (!beam.current) return
    const sweep = reducedMotion ? 0.5 : (Math.sin(state.clock.elapsedTime * 1.6) + 1) / 2
    beam.current.position.y = BELT_Y + 0.16 + sweep * (height - 0.42)
  })
  return (
    <group position={[x, 0, 0]}>
      {[1, -1].map((side) => (
        <mesh key={side} position={[0, BELT_Y + height / 2 - 0.1, side * z]}>
          <boxGeometry args={[0.09, height, 0.09]} />
          <meshStandardMaterial color={palette.metal} roughness={0.35} metalness={0.5} />
        </mesh>
      ))}
      <mesh position={[0, BELT_Y + height - 0.1, 0]}>
        <boxGeometry args={[0.12, 0.12, z * 2 + 0.09]} />
        <meshStandardMaterial
          color={active ? color : palette.metal}
          emissive={active ? color : '#000000'}
          emissiveIntensity={active ? 0.7 : 0}
          roughness={0.4}
        />
      </mesh>
      {scanner && active && (
        <mesh ref={beam} position={[0, BELT_Y + 0.6, 0]}>
          <boxGeometry args={[0.02, 0.03, z * 2 - 0.1]} />
          <meshBasicMaterial color={color} transparent opacity={0.75} />
        </mesh>
      )}
    </group>
  )
}

// The crate at the end of the line. A shipped crate gets its lid, and a
// closed crate is drawn in muted metal in place of wood.
function CrateBox({
  x,
  size,
  palette,
  sealed,
  muted,
  reducedMotion,
}: {
  x: number
  size: number
  palette: Palette
  sealed: boolean
  muted: boolean
  reducedMotion: boolean
}) {
  const lid = useRef<THREE.Group>(null)
  const height = 1.75
  const half = size / 2
  const wood = muted ? palette.metal : palette.wood
  const woodDark = muted ? palette.metalDark : palette.woodDark
  const slats = [0.36, 0.86, 1.36]
  useFrame((state, delta) => {
    if (!lid.current) return
    const target = height + 0.12
    lid.current.position.y = reducedMotion
      ? target
      : THREE.MathUtils.damp(lid.current.position.y, target, 4, state.clock.elapsedTime > 1.6 ? delta : 0)
  })
  return (
    <group position={[x, 0, 0]}>
      <mesh position={[0, 0.1, 0]}>
        <boxGeometry args={[size, 0.2, size]} />
        <meshStandardMaterial color={woodDark} roughness={0.85} />
      </mesh>
      {[
        [half, half],
        [half, -half],
        [-half, half],
        [-half, -half],
      ].map(([px, pz]) => (
        <mesh key={`${px}:${pz}`} position={[px, height / 2 + 0.05, pz]}>
          <boxGeometry args={[0.16, height + 0.1, 0.16]} />
          <meshStandardMaterial color={woodDark} roughness={0.85} />
        </mesh>
      ))}
      {slats.map((y, index) => (
        <group key={y}>
          {/* The front and near side stay lower, so the packed blocks show. */}
          {index < 2 && (
            <mesh position={[0, y, half]}>
              <boxGeometry args={[size - 0.1, 0.34, 0.07]} />
              <meshStandardMaterial color={wood} roughness={0.8} />
            </mesh>
          )}
          <mesh position={[0, y, -half]}>
            <boxGeometry args={[size - 0.1, 0.34, 0.07]} />
            <meshStandardMaterial color={wood} roughness={0.8} />
          </mesh>
          <mesh position={[-half, y, 0]}>
            <boxGeometry args={[0.07, 0.34, size - 0.1]} />
            <meshStandardMaterial color={wood} roughness={0.8} />
          </mesh>
          {index < 2 && (
            <mesh position={[half, y, 0]}>
              <boxGeometry args={[0.07, 0.34, size - 0.1]} />
              <meshStandardMaterial color={wood} roughness={0.8} />
            </mesh>
          )}
        </group>
      ))}
      {sealed && (
        <group ref={lid} position={[0, reducedMotion ? height + 0.12 : height + 3.4, 0]}>
          <mesh>
            <boxGeometry args={[size + 0.16, 0.14, size + 0.16]} />
            <meshStandardMaterial color={wood} roughness={0.8} />
          </mesh>
          {[-0.6, 0.6].map((offset) => (
            <mesh key={offset} position={[offset, 0.09, 0]}>
              <boxGeometry args={[0.22, 0.06, size + 0.18]} />
              <meshStandardMaterial color={woodDark} roughness={0.85} />
            </mesh>
          ))}
        </group>
      )}
    </group>
  )
}

function Pallet({ x, palette }: { x: number; palette: Palette }) {
  return (
    <group position={[x, 0, 0]}>
      {[-0.5, 0, 0.5].map((z) => (
        <mesh key={z} position={[0, 0.05, z]}>
          <boxGeometry args={[1.5, 0.1, 0.16]} />
          <meshStandardMaterial color={palette.woodDark} roughness={0.9} />
        </mesh>
      ))}
      {[-0.56, -0.19, 0.19, 0.56].map((offset) => (
        <mesh key={offset} position={[offset, 0.15, 0]}>
          <boxGeometry args={[0.3, 0.07, 1.5]} />
          <meshStandardMaterial color={palette.wood} roughness={0.85} />
        </mesh>
      ))}
    </group>
  )
}

// Keeps the page's own labels over their places in the scene. The labels are
// ordinary elements beside the canvas, moved here every frame (DEC-143(4)).
// The card follows its block where the block is drawn in this frame, so it
// stays over a block that is still moving, and it stays whole inside the
// scene.
interface Anchor {
  id: string
  position: [number, number, number]
}

const CARD_MARGIN = 4

// A dependency link the line shows, with the heights over its two blocks'
// centres where it starts and ends.
interface ShownLink extends WaitLink {
  key: string
  liftFrom: number
  liftTo: number
}

function Projector({
  anchors,
  card,
  groups,
  elements,
  links,
  paths,
  minRise,
}: {
  anchors: Anchor[]
  // The emphasized block and the height over its centre where its card
  // points.
  card: { id: string; lift: number } | null
  groups: RefObject<Map<string, THREE.Group>>
  elements: RefObject<Record<string, HTMLElement | null>>
  links: ShownLink[]
  paths: RefObject<Record<string, SVGPathElement | null>>
  // The least height, in pixels, that a link rises over its two ends.
  minRise: number
}) {
  const origin = useRef<THREE.Group>(null)
  const point = useMemo(() => new THREE.Vector3(), [])
  useFrame(({ camera, size }) => {
    if (!origin.current) return
    // The page position of `point`, a place in the scene's own frame.
    const project = () => {
      origin.current?.localToWorld(point).project(camera)
      return { x: (point.x * 0.5 + 0.5) * size.width, y: (-point.y * 0.5 + 0.5) * size.height }
    }
    for (const anchor of anchors) {
      const element = elements.current[anchor.id]
      if (!element) continue
      point.set(...anchor.position)
      const { x, y } = project()
      element.style.transform = `translate(-50%, -50%) translate(${x.toFixed(1)}px, ${y.toFixed(1)}px)`
      element.style.visibility = 'visible'
    }
    // Each link is a curve from the top of the block waited on, up and over,
    // down onto the top of the block that waits. It keeps one height over
    // the belt at both ends, so it follows the belt's slope on screen and
    // stays under the station signs.
    for (const link of links) {
      const from = groups.current.get(link.from)
      const to = groups.current.get(link.to)
      if (!from || !to) continue
      point.copy(from.position)
      point.y += link.liftFrom * from.scale.y
      const a = project()
      point.copy(to.position)
      point.y += link.liftTo * to.scale.y
      const b = project()
      const span = Math.hypot(b.x - a.x, b.y - a.y)
      const rise = Math.min(Math.max(minRise, span * 0.42), minRise * 1.75)
      const d = `M${a.x.toFixed(1)},${(a.y - 2).toFixed(1)} C${(a.x + (b.x - a.x) * 0.12).toFixed(1)},${(a.y - rise).toFixed(1)} ${(b.x - (b.x - a.x) * 0.12).toFixed(1)},${(b.y - rise).toFixed(1)} ${b.x.toFixed(1)},${(b.y - 5).toFixed(1)}`
      for (const layer of ['halo', 'line']) paths.current[`${layer}:${link.key}`]?.setAttribute('d', d)
      paths.current[`dot:${link.key}`]?.setAttribute(
        'transform',
        `translate(${a.x.toFixed(1)},${(a.y - 2).toFixed(1)})`,
      )
    }
    const element = elements.current.card
    const node = card ? groups.current.get(card.id) : undefined
    if (!card || !element || !node) return
    point.copy(node.position)
    point.y += card.lift * node.scale.y
    let { x, y } = project()
    // The card's box runs from half its width either side of x, and from
    // its full height above y down to y.
    const width = element.offsetWidth
    const height = element.offsetHeight
    if (size.width >= width + 2 * CARD_MARGIN)
      x = THREE.MathUtils.clamp(x, width / 2 + CARD_MARGIN, size.width - width / 2 - CARD_MARGIN)
    if (size.height >= height + 2 * CARD_MARGIN)
      y = THREE.MathUtils.clamp(y, height + CARD_MARGIN, size.height - CARD_MARGIN)
    element.style.transform = `translate(-50%, -50%) translate(${x.toFixed(1)}px, ${y.toFixed(1)}px)`
    element.style.visibility = 'visible'
  })
  return <group ref={origin} />
}

// Reports the loss of the mounted canvas's WebGL context. three.js handles
// the event without throwing and draws nothing until the context returns, so
// no error boundary sees it (req-crate-insights AC-1.5; component-crates
// invariant 34).
function ContextLossWatch({ onLost }: { onLost: () => void }) {
  const canvas = useThree((state) => state.gl.domElement)
  useEffect(() => {
    canvas.addEventListener('webglcontextlost', onLost)
    return () => canvas.removeEventListener('webglcontextlost', onLost)
  }, [canvas, onLost])
  return null
}

// Under reduced motion the canvas draws on demand. Each change of what it
// shows draws a frame, and in that frame every block stands at its place.
function DrawOnChange({
  blocks,
  emphasizedId,
  related,
  links,
  shipped,
  closed,
  dark,
}: {
  blocks: LineBlock[]
  emphasizedId: string | null
  related: Set<string> | null
  // The key of the shown links, so a link that appears or goes is placed.
  links: string
  shipped: boolean
  closed: boolean
  dark: boolean
}) {
  const invalidate = useThree((state) => state.invalidate)
  useEffect(() => {
    void [blocks, emphasizedId, related, links, shipped, closed, dark]
    invalidate()
  }, [invalidate, blocks, emphasizedId, related, links, shipped, closed, dark])
  return null
}

// Fits the whole line in the canvas, below what the page overlays on it, and
// leans the scene a little toward the pointer unless motion is reduced.
function Rig({
  bounds,
  padTop,
  padBottom,
  reducedMotion,
  children,
}: {
  bounds: THREE.Box3
  padTop: number
  padBottom: number
  reducedMotion: boolean
  children: ReactNode
}) {
  const { camera, size } = useThree()
  const stage = useRef<THREE.Group>(null)
  useLayoutEffect(() => {
    const direction = new THREE.Vector3(0.2, 0.52, 1).normalize()
    const centre = bounds.getCenter(new THREE.Vector3())
    camera.position.copy(centre).addScaledVector(direction, 40)
    camera.lookAt(centre)
    camera.zoom = 1
    camera.updateMatrixWorld(true)
    const min = new THREE.Vector2(Infinity, Infinity)
    const max = new THREE.Vector2(-Infinity, -Infinity)
    const corner = new THREE.Vector3()
    for (const x of [bounds.min.x, bounds.max.x])
      for (const y of [bounds.min.y, bounds.max.y])
        for (const z of [bounds.min.z, bounds.max.z]) {
          corner.set(x, y, z).applyMatrix4(camera.matrixWorldInverse)
          min.min(new THREE.Vector2(corner.x, corner.y))
          max.max(new THREE.Vector2(corner.x, corner.y))
        }
    const usableHeight = Math.max(80, size.height - padTop - padBottom)
    const zoom = Math.min((size.width - 28) / (max.x - min.x), usableHeight / (max.y - min.y))
    const right = new THREE.Vector3().setFromMatrixColumn(camera.matrixWorld, 0)
    const up = new THREE.Vector3().setFromMatrixColumn(camera.matrixWorld, 1)
    camera.position
      .addScaledVector(right, (min.x + max.x) / 2)
      .addScaledVector(up, (min.y + max.y) / 2 + (padTop - padBottom) / 2 / zoom)
    camera.zoom = zoom
    camera.updateProjectionMatrix()
    camera.updateMatrixWorld(true)
  }, [camera, size.width, size.height, bounds, padTop, padBottom])
  useFrame((state, delta) => {
    if (!stage.current || reducedMotion) return
    stage.current.rotation.y = THREE.MathUtils.damp(stage.current.rotation.y, state.pointer.x * 0.05, 3, delta)
    stage.current.rotation.x = THREE.MathUtils.damp(stage.current.rotation.x, -state.pointer.y * 0.02, 3, delta)
  })
  return <group ref={stage}>{children}</group>
}

export default function LineScene({
  blocks,
  counts,
  shipped,
  closed,
  compact,
  dark,
  reducedMotion,
  emphasizedId,
  emphasizedTask,
  related,
  links,
  onEmphasize,
  onSelect,
  onContextLost,
  padTop,
  padBottom,
}: LineSceneProps) {
  const layout = useMemo(() => layoutFor(compact), [compact])
  const places = useMemo(() => place(blocks, layout, links), [blocks, layout, links])
  const palette = useMemo(() => readPalette(dark), [dark])
  // The camera fits the line and every block on it, so a long row of
  // closed tasks or a tall stack at a station stays in view.
  const bounds = useMemo(() => {
    const box = new THREE.Box3(
      new THREE.Vector3(layout.pallet - 0.95, 0, -1.3),
      new THREE.Vector3(layout.crate + layout.crateSize / 2 + 0.2, compact ? 4.25 : 2.95, 1.9),
    )
    const corner = new THREE.Vector3()
    for (const spot of places.values()) {
      const half = spot.size / 2
      box.expandByPoint(corner.copy(spot.position).subScalar(half))
      box.expandByPoint(corner.copy(spot.position).addScalar(half))
    }
    return box
  }, [layout, compact, places])
  // A pointer that leaves with the scene leaves no pointer cursor behind.
  useEffect(
    () => () => {
      document.body.style.cursor = ''
    },
    [],
  )
  const zoneMid = (zone: Zone) => (layout.zones[zone][0] + layout.zones[zone][1]) / 2
  const tone = (phase: CratePhase) => palette.phase[phase]
  const stationTone: Record<Zone, string> = {
    plan: tone('planning'),
    build: tone('building'),
    review: tone('review'),
    gate: tone('needs_you'),
  }
  const hasAttention = (zone: Zone) => blocks.some((block) => block.station === zone && block.phase === 'needs_you')

  // The six station signs and the pointed-at task's card, as page elements.
  const signs: Array<{
    id: LineStation
    label: string
    count: number
    color: string
    position: [number, number, number]
  }> = [
    {
      id: 'intake',
      label: 'Queued',
      count: counts.intake,
      color: tone('queued'),
      position: [layout.pallet + (compact ? 0.25 : 0), 2.05, 0],
    },
    // On a narrow screen the signs alternate between two heights, so
    // neighbours never cover each other.
    ...zones.map((zone, index) => ({
      id: zone,
      label: zone === 'gate' ? (compact ? 'Gate' : 'Merge gate') : zone[0].toUpperCase() + zone.slice(1),
      count: counts[zone],
      color: hasAttention(zone) ? tone('needs_you') : stationTone[zone],
      position: [zoneMid(zone), BELT_Y + (compact && index % 2 === 0 ? 3.0 : 1.78), 0] as [number, number, number],
    })),
    {
      id: 'crate',
      label: 'Merged',
      count: counts.crate,
      color: tone('merged'),
      position: [layout.crate, compact ? BELT_Y + 3.0 : 2.7, 0],
    },
  ]
  const emphasized = emphasizedId ? blocks.find((block) => block.id === emphasizedId) : undefined
  const emphasizedSpot = emphasized ? places.get(emphasized.id) : undefined
  const anchors: Anchor[] = signs.map((sign) => ({ id: sign.id, position: sign.position }))
  // The links the line draws: one for each served `waiting` member entry of
  // the emphasized task, and none at rest (component-crates invariant 39).
  const shownLinks: ShownLink[] = emphasized
    ? links.flatMap((link) => {
        const a = places.get(link.from)
        const b = places.get(link.to)
        if (link.to !== emphasized.id || !a || !b) return []
        return [{ ...link, key: `${link.from}>${link.to}`, liftFrom: a.size / 2, liftTo: b.size / 2 }]
      })
    : []
  // The card moves up to clear the links that end on its block.
  const card =
    emphasized && emphasizedSpot
      ? { id: emphasized.id, lift: emphasizedSpot.size / 2 + (shownLinks.length > 0 ? 0.95 : 0.2) }
      : null
  const groups = useRef(new Map<string, THREE.Group>())
  const elements = useRef<Record<string, HTMLElement | null>>({})
  const paths = useRef<Record<string, SVGPathElement | null>>({})

  return (
    <div className="relative size-full" data-line-scene data-motion={reducedMotion ? 'still' : 'moving'}>
      <Canvas
        orthographic
        flat
        dpr={[1, 2]}
        gl={{ antialias: true, alpha: true }}
        camera={{ position: [10, 12, 24], zoom: 50, near: 0.1, far: 200 }}
        frameloop={reducedMotion ? 'demand' : 'always'}
        onPointerMissed={() => onEmphasize(null)}
        aria-hidden
      >
        <ambientLight intensity={dark ? 1.6 : 1.9} />
        <directionalLight position={[6, 12, 8]} intensity={dark ? 1.9 : 1.5} />
        <directionalLight position={[-8, 5, -4]} intensity={0.5} />
        <Rig bounds={bounds} padTop={padTop} padBottom={padBottom} reducedMotion={reducedMotion}>
          <Belt layout={layout} palette={palette} reducedMotion={reducedMotion} />
          <Pallet x={layout.pallet} palette={palette} />
          {zones.map((zone) => (
            <Arch
              key={zone}
              x={layout.zones[zone][0] - 0.22}
              color={hasAttention(zone) ? tone('needs_you') : stationTone[zone]}
              active={counts[zone] > 0}
              scanner={zone === 'review'}
              palette={palette}
              reducedMotion={reducedMotion}
            />
          ))}
          <CrateBox
            x={layout.crate}
            size={layout.crateSize}
            palette={palette}
            sealed={shipped}
            muted={closed && !shipped}
            reducedMotion={reducedMotion}
          />
          {blocks.map((block, index) => {
            const spot = places.get(block.id)
            if (!spot) return null
            return (
              <Block
                key={block.id}
                block={block}
                spot={spot}
                index={index}
                color={tone(block.phase)}
                emphasized={emphasizedId === block.id}
                dimmed={related !== null && !related.has(block.id)}
                reducedMotion={reducedMotion}
                onEmphasize={onEmphasize}
                groups={groups}
                onSelect={onSelect}
              />
            )
          })}
          <Projector
            anchors={anchors}
            card={card}
            groups={groups}
            elements={elements}
            links={shownLinks}
            paths={paths}
            minRise={compact ? 13 : 26}
          />
          <ContextLossWatch onLost={onContextLost} />
          <DrawOnChange
            blocks={blocks}
            emphasizedId={emphasizedId}
            related={related}
            links={linksKey(shownLinks)}
            shipped={shipped}
            closed={closed}
            dark={dark}
          />
          <ContactShadows
            position={[0, 0.005, 0]}
            opacity={palette.shadow}
            scale={34}
            blur={2.2}
            far={5}
            resolution={512}
          />
        </Rig>
      </Canvas>
      {/* The page's text list states the same counts to assistive technology,
          so the signs and the card stay out of its reach. */}
      <div aria-hidden className="pointer-events-none absolute inset-0 overflow-hidden">
        <svg data-line-links aria-hidden="true" className="absolute inset-0 size-full overflow-visible">
          <defs>
            <marker
              id="crate-line-link-arrow"
              viewBox="0 0 10 10"
              refX="8"
              refY="5"
              markerWidth="8"
              markerHeight="8"
              orient="auto"
              markerUnits="userSpaceOnUse"
            >
              <path
                d="M2,2 L8,5 L2,8"
                fill="none"
                stroke="var(--color-foreground)"
                strokeWidth="1.6"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </marker>
          </defs>
          {shownLinks.map((link) => {
            const ref = (layer: string) => (element: SVGPathElement | null) => {
              paths.current[`${layer}:${link.key}`] = element
            }
            return (
              <g key={link.key} data-line-link={link.key}>
                <path
                  ref={ref('halo')}
                  fill="none"
                  stroke="var(--color-card)"
                  strokeOpacity={0.55}
                  strokeWidth={4.5}
                  strokeLinecap="round"
                />
                <path
                  ref={ref('line')}
                  data-line-link-path
                  fill="none"
                  stroke="var(--color-foreground)"
                  strokeWidth={1.75}
                  strokeLinecap="round"
                  markerEnd="url(#crate-line-link-arrow)"
                />
                {/* Off the canvas until the first frame places it. */}
                <path
                  ref={ref('dot')}
                  transform="translate(-100,-100)"
                  d="M-2.6,0 a2.6,2.6 0 1,0 5.2,0 a2.6,2.6 0 1,0 -5.2,0"
                  fill="var(--color-foreground)"
                  stroke="var(--color-card)"
                  strokeWidth={1}
                />
              </g>
            )
          })}
        </svg>
        {signs.map((sign) => (
          <div
            key={sign.id}
            ref={(element) => {
              elements.current[sign.id] = element
            }}
            data-station-sign={sign.id}
            style={{ visibility: 'hidden' }}
            className={`absolute top-0 left-0 flex w-max items-center gap-1.5 rounded-full border border-border bg-card/90 px-2 py-0.5 text-[11px] leading-4 shadow-xs backdrop-blur ${
              sign.count === 0 ? 'text-faint' : 'text-foreground'
            }`}
          >
            <span
              className="size-1.5 rounded-full"
              style={{ background: sign.count === 0 ? 'var(--color-edge)' : sign.color }}
            />
            {sign.label}
            <span className="font-medium tabular-nums">{sign.count}</span>
          </div>
        ))}
        {emphasized && (
          <div
            ref={(element) => {
              elements.current.card = element
            }}
            data-line-card={emphasized.id}
            style={{ visibility: 'hidden' }}
            className="absolute top-0 left-0 z-10"
          >
            <div className="w-max max-w-64 -translate-y-1/2 rounded-md border border-border bg-card px-2.5 py-1.5 text-left shadow-md">
              {emphasized.title && (
                <p className="break-words text-xs font-medium leading-snug text-foreground">{emphasized.title}</p>
              )}
              <p className="mt-0.5 flex flex-wrap items-center gap-x-1.5 text-[11px] text-muted">
                <span
                  className="size-1.5 shrink-0 rounded-full"
                  style={{ background: phaseMeta[emphasized.phase].color }}
                />
                {emphasized.label !== undefined && <span data-line-card-label>{emphasized.label}</span>}
                <span className="font-mono text-faint">→ {emphasized.baseBranch}</span>
              </p>
              <DependencyDetails task={emphasizedTask} className="mt-1 border-t border-border pt-1" />
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
