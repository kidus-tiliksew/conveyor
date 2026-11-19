// PROTOTYPE. The crate's line in 3D: a conveyor belt that carries one module
// per member task through plan, build, review, and the merge gate, into the
// crate at the end. A module's place is its task's place, so the scene is a
// picture of the crate's real state and nothing in it is decoration only.
//
// When the crate's branches release into one another, the belt splits after
// the merge gate: work merged into a branch waits on that branch's side belt,
// and the side belt joins the main belt again where a release takes it on.
// The crate then holds only what has reached the last branch.
//
// This file is loaded on demand, so three.js stays out of the first load.
import { ContactShadows, RoundedBox } from '@react-three/drei'
import { Canvas, useFrame, useThree } from '@react-three/fiber'
import { type RefObject, useEffect, useLayoutEffect, useMemo, useRef } from 'react'
import * as THREE from 'three'
import { type Phase, phaseMeta, type Station, shortTitle, type TaskTrack } from './model'

export interface LineSceneProps {
  tracks: TaskTrack[]
  // Branches whose merged work waits for a release, and the branch that
  // releases reach last. Both are empty for a crate with no recorded release.
  lanes: string[]
  trunk?: string
  shipped: boolean
  closed: boolean
  compact: boolean
  dark: boolean
  reducedMotion: boolean
  hoveredId: string | null
  onHover: (id: string | null) => void
  onSelect: (id: string) => void
  // Pixels the page overlays on the scene's top and bottom edges.
  padTop: number
  padBottom: number
}

const MODULE = 0.6
const BELT_Y = 1.0
const BELT_HALF_WIDTH = 0.82

// A side belt: it leaves the main belt at `split`, runs level from `from` to
// `to` at depth `z`, and joins the main belt again at `join`.
interface Lane {
  branch: string
  z: number
  split: number
  from: number
  to: number
  join: number
}

interface Layout {
  beltFrom: number
  beltTo: number
  zones: Record<'plan' | 'build' | 'review' | 'gate', [number, number]>
  lanes: Lane[]
  pallet: number
  crate: number
  crateSize: number
}

function layoutFor(compact: boolean, branches: string[]): Layout {
  const k = compact ? 0.56 : 1
  const crateSize = compact ? 2.1 : 2.5
  const pallet = -5.9 * k - (compact ? 1.05 : 1.25)
  if (branches.length === 0)
    return {
      beltFrom: -5.9 * k,
      beltTo: 4.3 * k,
      zones: {
        plan: [-5.5 * k, -3.5 * k],
        build: [-2.9 * k, -0.5 * k],
        review: [0.1 * k, 2.3 * k],
        gate: [2.9 * k, 4.0 * k],
      },
      lanes: [],
      pallet,
      crate: 4.3 * k + (compact ? 1.5 : 1.75),
      crateSize,
    }
  // The stations close up to make room for the side belts after the gate.
  return {
    beltFrom: -5.9 * k,
    beltTo: 6.8 * k,
    zones: {
      plan: [-5.5 * k, -3.9 * k],
      build: [-3.4 * k, -1.6 * k],
      review: [-1.1 * k, 0.7 * k],
      gate: [1.2 * k, 2.3 * k],
    },
    lanes: branches.map((branch, index) => ({
      branch,
      z: 1.8 + index * 1.55,
      split: 2.55 * k,
      from: 3.55 * k,
      to: 5.45 * k,
      join: 6.45 * k,
    })),
    pallet,
    crate: 6.8 * k + (compact ? 1.5 : 1.75),
    crateSize,
  }
}

type Place = {
  position: THREE.Vector3
  kind: 'belt' | 'lane' | 'crate' | 'pallet' | 'aside'
  size: number
  tilt?: number
}

// Where every module sits. Belt modules fill their zone from the end nearest
// the next station, two abreast, then stack.
function place(tracks: TaskTrack[], layout: Layout): Map<string, Place> {
  const places = new Map<string, Place>()
  const by = (station: Station) => tracks.filter((track) => track.station === station)

  for (const zone of ['plan', 'build', 'review', 'gate'] as const) {
    const [from, to] = layout.zones[zone]
    const columns = Math.max(1, Math.floor((to - from) / (MODULE + 0.16)))
    by(zone).forEach((track, index) => {
      const perLayer = columns * 2
      const layer = Math.floor(index / perLayer)
      const slot = index % perLayer
      const column = Math.floor(slot / 2)
      const row = slot % 2
      places.set(track.id, {
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

  // Merged work that waits for a release fills its side belt from the join.
  for (const lane of layout.lanes) {
    const columns = Math.max(1, Math.floor((lane.to - lane.from) / (MODULE + 0.16)))
    tracks
      .filter((track) => track.station === 'lane' && track.lane === lane.branch)
      .forEach((track, index) => {
        const perLayer = columns * 2
        const slot = index % perLayer
        places.set(track.id, {
          kind: 'lane',
          size: MODULE,
          position: new THREE.Vector3(
            lane.to - MODULE / 2 - 0.05 - Math.floor(slot / 2) * (MODULE + 0.16),
            BELT_Y + 0.05 + MODULE / 2 + Math.floor(index / perLayer) * (MODULE + 0.02),
            lane.z + (slot % 2 === 0 ? 0.36 : -0.36),
          ),
        })
      })
  }

  by('intake').forEach((track, index) => {
    const layer = Math.floor(index / 4)
    const slot = index % 4
    places.set(track.id, {
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
  merged.forEach((track, index) => {
    const layer = Math.floor(index / (grid * grid))
    const slot = index % (grid * grid)
    places.set(track.id, {
      kind: 'crate',
      size,
      position: new THREE.Vector3(
        layout.crate - inner / 2 + cell / 2 + (slot % grid) * cell,
        0.2 + size / 2 + layer * (size + 0.03),
        inner / 2 - cell / 2 - Math.floor(slot / grid) * cell,
      ),
    })
  })

  by('aside').forEach((track, index) => {
    places.set(track.id, {
      kind: 'aside',
      size: MODULE * 0.9,
      tilt: 0.5 + index * 0.9,
      position: new THREE.Vector3(layout.beltFrom + 0.7 + index * 0.95, MODULE * 0.45, 1.55),
    })
  })
  return places
}

interface Palette {
  phase: Record<Phase, string>
  belt: string
  beltMark: string
  metal: string
  metalDark: string
  wood: string
  woodDark: string
  shadow: number
}

function readPalette(dark: boolean): Palette {
  const style = getComputedStyle(document.documentElement)
  const token = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback
  const phase = Object.fromEntries(
    (Object.keys(phaseMeta) as Phase[]).map((key) => {
      const name = phaseMeta[key].color.replace(/^var\((.*)\)$/, '$1')
      return [key, token(name, '#888')]
    }),
  ) as Record<Phase, string>
  // Brighter fills than the text tokens, so a module reads as a lit object.
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

function Module({
  track,
  spot,
  index,
  color,
  hovered,
  dimmed,
  reducedMotion,
  onHover,
  onSelect,
}: {
  track: TaskTrack
  spot: Place
  index: number
  color: string
  hovered: boolean
  dimmed: boolean
  reducedMotion: boolean
  onHover: (id: string | null) => void
  onSelect: (id: string) => void
}) {
  const group = useRef<THREE.Group>(null)
  const beacon = useRef<THREE.Mesh>(null)
  const lift = useRef(0)
  // One move at a time: from where the module is to its new place.
  const move = useRef<{ from: THREE.Vector3; to: THREE.Vector3; start: number; hop: number; first: boolean } | null>(
    null,
  )
  const kind = useRef(spot.kind)
  const working = track.phase === 'building' || track.phase === 'planning' || track.phase === 'review'
  const needsYou = track.phase === 'needs_you'

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
      // Its task moved on. Into the crate, across to a side belt, or on and
      // off the floor, it is lifted.
      const lifted = spot.kind !== 'belt' || kind.current !== 'belt'
      move.current = {
        from: node.position.clone(),
        to: spot.position.clone(),
        start: time,
        hop: !lifted ? 0 : spot.kind === 'lane' ? 0.8 : 1.5,
        first: false,
      }
    }
    kind.current = spot.kind
    const { from, to, start, hop, first } = move.current
    const progress = reducedMotion ? 1 : THREE.MathUtils.clamp((time - start) / 0.95, 0, 1)
    const eased = first && spot.kind === 'crate' ? bounce(progress) : easeOut(progress)
    let x = THREE.MathUtils.lerp(from.x, to.x, eased)
    let y = THREE.MathUtils.lerp(from.y, to.y, eased) + Math.sin(Math.PI * progress) * hop
    const z = THREE.MathUtils.lerp(from.z, to.z, eased)
    const scale = first ? Math.max(0.001, easeOut(Math.min(1, progress * (spot.kind === 'crate' ? 8 : 2.2)))) : 1
    if (!reducedMotion && progress === 1) {
      if (spot.kind === 'belt') {
        // A carried module rides the belt a little, then settles back.
        x += Math.sin(time * 0.9 + index * 1.7) * 0.035
        if (working) y += Math.abs(Math.sin(time * 2.2 + index)) * 0.035
      }
      if (needsYou) y += Math.sin(time * 3) * 0.03
    }
    lift.current = THREE.MathUtils.damp(lift.current, hovered ? 1 : 0, 12, delta)
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
          onHover(track.id)
          document.body.style.cursor = 'pointer'
        }}
        onPointerOut={() => {
          onHover(null)
          document.body.style.cursor = ''
        }}
        onClick={(event) => {
          event.stopPropagation()
          onSelect(track.id)
        }}
      >
        <meshStandardMaterial
          color={color}
          roughness={0.5}
          metalness={0.05}
          emissive={color}
          emissiveIntensity={hovered ? 0.35 : working ? 0.12 : 0.04}
          transparent
          opacity={dimmed ? 0.32 : 1}
        />
      </RoundedBox>
      {/* A strap across the top marks the module as a packed unit of work. */}
      <mesh position={[0, spot.size / 2 + 0.003, 0]} rotation={[-Math.PI / 2, 0, 0]}>
        <planeGeometry args={[spot.size * 0.22, spot.size * 0.86]} />
        <meshBasicMaterial color="#ffffff" transparent opacity={dimmed ? 0.06 : 0.26} />
      </mesh>
      {needsYou && (
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
// [x, z]. Its chevrons run from `a` to `b`.
function BeltRun({
  a,
  b,
  width = BELT_HALF_WIDTH * 2,
  rails = true,
  drop = 0,
  palette,
  reducedMotion,
}: {
  a: [number, number]
  b: [number, number]
  width?: number
  rails?: boolean
  // How far below the main belt this run sits, so runs that cross never
  // share a surface.
  drop?: number
  palette: Palette
  reducedMotion: boolean
}) {
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
    <group position={[(a[0] + b[0]) / 2, -drop, (a[1] + b[1]) / 2]} rotation={[0, -angle, 0]}>
      <mesh position={[0, BELT_Y, 0]}>
        <boxGeometry args={[length, 0.1, width]} />
        <meshStandardMaterial map={texture} roughness={0.9} />
      </mesh>
      {rails &&
        [1, -1].map((side) => (
          <mesh key={side} position={[0, BELT_Y - 0.02, side * (half + 0.06)]}>
            <boxGeometry args={[length + 0.3, 0.24, 0.12]} />
            <meshStandardMaterial color={palette.metal} roughness={0.35} metalness={0.5} />
          </mesh>
        ))}
      {legs.map((x) =>
        [1, -1].map((side) => (
          <mesh key={`${x}:${side}`} position={[x, (BELT_Y - 0.14) / 2, side * (half + (rails ? 0.06 : -0.08))]}>
            <boxGeometry args={[0.1, BELT_Y - 0.14, 0.1]} />
            <meshStandardMaterial color={palette.metalDark} roughness={0.5} metalness={0.4} />
          </mesh>
        )),
      )}
    </group>
  )
}

function Belt({ layout, palette, reducedMotion }: { layout: Layout; palette: Palette; reducedMotion: boolean }) {
  const edge = BELT_HALF_WIDTH - 0.25
  return (
    <group>
      <BeltRun a={[layout.beltFrom, 0]} b={[layout.beltTo, 0]} palette={palette} reducedMotion={reducedMotion} />
      {[layout.beltFrom, layout.beltTo].map((x) => (
        <mesh key={x} position={[x, BELT_Y - 0.02, 0]} rotation={[Math.PI / 2, 0, 0]}>
          <cylinderGeometry args={[0.13, 0.13, BELT_HALF_WIDTH * 2 + 0.1, 20]} />
          <meshStandardMaterial color={palette.metalDark} roughness={0.4} metalness={0.5} />
        </mesh>
      ))}
      {layout.lanes.map((lane) => (
        <group key={lane.branch}>
          {/* Out to the side belt, along it, and back at the release. */}
          <BeltRun
            a={[lane.split, edge]}
            b={[lane.from + 0.2, lane.z]}
            width={1.15}
            rails={false}
            drop={0.03}
            palette={palette}
            reducedMotion={reducedMotion}
          />
          <BeltRun a={[lane.from, lane.z]} b={[lane.to, lane.z]} palette={palette} reducedMotion={reducedMotion} />
          <BeltRun
            a={[lane.to - 0.2, lane.z]}
            b={[lane.join, edge]}
            width={1.15}
            rails={false}
            drop={0.03}
            palette={palette}
            reducedMotion={reducedMotion}
          />
        </group>
      ))}
      {/* The chute that drops merged modules into the crate. */}
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
          {/* The front and near side stay lower, so the packed modules show. */}
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
        <group ref={lid} position={[0, height + 3.4, 0]}>
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
// ordinary elements beside the canvas, moved here every frame.
export interface Anchor {
  id: string
  position: [number, number, number]
}

function Projector({
  anchors,
  elements,
}: {
  anchors: Anchor[]
  elements: RefObject<Record<string, HTMLElement | null>>
}) {
  const origin = useRef<THREE.Group>(null)
  const point = useMemo(() => new THREE.Vector3(), [])
  useFrame(({ camera, size }) => {
    if (!origin.current) return
    for (const anchor of anchors) {
      const element = elements.current[anchor.id]
      if (!element) continue
      point.set(...anchor.position)
      origin.current.localToWorld(point).project(camera)
      const x = (point.x * 0.5 + 0.5) * size.width
      const y = (-point.y * 0.5 + 0.5) * size.height
      element.style.transform = `translate(-50%, -50%) translate(${x.toFixed(1)}px, ${y.toFixed(1)}px)`
      element.style.visibility = 'visible'
    }
  })
  return <group ref={origin} />
}

// Fits the whole line in the canvas, below what the page overlays on it, and
// leans the scene a little toward the pointer.
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
  children: React.ReactNode
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
  tracks,
  lanes,
  trunk,
  shipped,
  closed,
  compact,
  dark,
  reducedMotion,
  hoveredId,
  onHover,
  onSelect,
  padTop,
  padBottom,
}: LineSceneProps) {
  const laneKey = lanes.join('|')
  // biome-ignore lint/correctness/useExhaustiveDependencies: laneKey stands for the lanes
  const layout = useMemo(() => layoutFor(compact, lanes), [compact, laneKey])
  const places = useMemo(() => place(tracks, layout), [tracks, layout])
  // biome-ignore lint/correctness/useExhaustiveDependencies: the tokens change with the theme
  const palette = useMemo(() => readPalette(dark), [dark])
  const bounds = useMemo(
    () =>
      new THREE.Box3(
        new THREE.Vector3(layout.pallet - 0.95, 0, -1.3),
        new THREE.Vector3(
          layout.crate + layout.crateSize / 2 + 0.2,
          compact ? 4.25 : 2.95,
          Math.max(1.9, ...layout.lanes.map((lane) => lane.z + 1.05)),
        ),
      ),
    [layout, compact],
  )
  const count = (station: Station) => tracks.filter((track) => track.station === station).length
  const zoneMid = (zone: keyof Layout['zones']) => (layout.zones[zone][0] + layout.zones[zone][1]) / 2
  const tone = (phase: Phase) => palette.phase[phase]
  const stationTone: Record<keyof Layout['zones'], string> = {
    plan: tone('planning'),
    build: tone('building'),
    review: tone('review'),
    gate: tone('needs_you'),
  }
  const hasAttention = (zone: Station) => tracks.some((track) => track.station === zone && track.phase === 'needs_you')

  // The station signs and the pointed-at task's card, as page elements.
  const signs = [
    {
      id: 'intake',
      label: 'Queued',
      count: count('intake'),
      color: tone('queued'),
      position: [layout.pallet + (compact ? 0.25 : 0), 2.05, 0],
    },
    // On a narrow screen the signs alternate between two heights, so
    // neighbours never cover each other.
    ...(['plan', 'build', 'review', 'gate'] as const).map((zone, index) => ({
      id: zone,
      label: zone === 'gate' ? (compact ? 'Gate' : 'Merge gate') : zone[0].toUpperCase() + zone.slice(1),
      count: count(zone),
      color: hasAttention(zone) ? tone('needs_you') : stationTone[zone],
      position: [zoneMid(zone), BELT_Y + (compact && index % 2 === 0 ? 3.0 : 1.78), 0],
    })),
    // A side belt and the crate are named for the branch that holds the work.
    ...layout.lanes.map((lane) => ({
      id: `lane:${lane.branch}`,
      label: `In ${lane.branch}`,
      count: tracks.filter((track) => track.station === 'lane' && track.lane === lane.branch).length,
      color: tone('merged'),
      // On the floor in front of the side belt, clear of the main belt behind it.
      position: [(lane.from + lane.to) / 2, 0, lane.z + (compact ? 1.5 : 1.75)],
    })),
    {
      id: 'crate',
      label: layout.lanes.length > 0 && trunk ? `In ${trunk}` : 'Merged',
      count: count('crate'),
      color: tone('merged'),
      position: [layout.crate, compact ? BELT_Y + 3.0 : 2.7, 0],
    },
  ] as Array<{ id: string; label: string; count: number; color: string; position: [number, number, number] }>
  const hovered = hoveredId ? tracks.find((track) => track.id === hoveredId) : undefined
  const hoveredSpot = hovered ? places.get(hovered.id) : undefined
  const anchors: Anchor[] = signs.map((sign) => ({ id: sign.id, position: sign.position }))
  if (hoveredSpot)
    anchors.push({
      id: 'hovered',
      position: [hoveredSpot.position.x, hoveredSpot.position.y + hoveredSpot.size / 2 + 0.2, hoveredSpot.position.z],
    })
  const elements = useRef<Record<string, HTMLElement | null>>({})

  return (
    <div className="relative size-full">
      <Canvas
        orthographic
        flat
        dpr={[1, 2]}
        gl={{ antialias: true, alpha: true }}
        camera={{ position: [10, 12, 24], zoom: 50, near: 0.1, far: 200 }}
        frameloop={reducedMotion ? 'demand' : 'always'}
        onPointerMissed={() => onHover(null)}
        aria-hidden
      >
        <ambientLight intensity={dark ? 1.6 : 1.9} />
        <directionalLight position={[6, 12, 8]} intensity={dark ? 1.9 : 1.5} />
        <directionalLight position={[-8, 5, -4]} intensity={0.5} />
        <Rig bounds={bounds} padTop={padTop} padBottom={padBottom} reducedMotion={reducedMotion}>
          <Belt layout={layout} palette={palette} reducedMotion={reducedMotion} />
          <Pallet x={layout.pallet} palette={palette} />
          {(['plan', 'build', 'review', 'gate'] as const).map((zone) => (
            <Arch
              key={zone}
              x={layout.zones[zone][0] - 0.22}
              color={hasAttention(zone) ? tone('needs_you') : stationTone[zone]}
              active={count(zone) > 0}
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
          {tracks.map((track, index) => {
            const spot = places.get(track.id)
            if (!spot) return null
            return (
              <Module
                key={track.id}
                track={track}
                spot={spot}
                index={index}
                color={tone(track.phase)}
                hovered={hoveredId === track.id}
                dimmed={hoveredId !== null && hoveredId !== track.id}
                reducedMotion={reducedMotion}
                onHover={onHover}
                onSelect={onSelect}
              />
            )
          })}
          <Projector anchors={anchors} elements={elements} />
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
      <div className="pointer-events-none absolute inset-0 overflow-hidden">
        {signs.map((sign) => (
          <div
            key={sign.id}
            ref={(element) => {
              elements.current[sign.id] = element
            }}
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
        {hovered && (
          <div
            ref={(element) => {
              elements.current.hovered = element
            }}
            style={{ visibility: 'hidden' }}
            className="absolute top-0 left-0 z-10"
          >
            <div className="w-max max-w-56 -translate-y-1/2 rounded-md border border-border bg-card px-2.5 py-1.5 text-left shadow-md">
              <p className="text-xs font-medium leading-snug text-foreground">{shortTitle(hovered.title, 60)}</p>
              <p className="mt-0.5 flex items-center gap-1.5 text-[11px] text-muted">
                <span
                  className="size-1.5 shrink-0 rounded-full"
                  style={{ background: phaseMeta[hovered.phase].color }}
                />
                {hovered.task.next_action.label}
                <span className="font-mono text-faint">→ {hovered.baseBranch}</span>
              </p>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
