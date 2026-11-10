import { stageLabels, workOrderStageLabels } from '../../lib/contracts'
import type { ActivityItem, Intervention, Job, WorkOrder, WorkspaceMembership } from '../../lib/types'
import { cn } from '../../lib/utils'
import { useWorkspaceMembers } from '../app-shell'
import { Disclosure } from '../ui/disclosure'

// Who ran each timeline entry: the claimant's avatar takes the place of the
// rail dot, and the person's name shows only in the avatar's label, which
// opens on hover, keyboard focus, and tap. No card prints the name
// (req-operator-next-action REQ-7; task 261110-96f5ed). A past claimant comes
// from `summaries.job_results`, never from `events`
// (component-web-task-surfaces invariant 25).

export interface Person {
  userId: string
  name: string
}

export interface RailPerson {
  person: Person
  label: string
  live?: boolean
}

const runClaimantPrefix = 'run:'

// The user ID of a run:<user id> claimant, and undefined for any other
// claimant, the same rule the server folds (core.IsTaskRunClaimantID).
export function runClaimantUserId(claimant: string | undefined): string | undefined {
  if (!claimant?.startsWith(runClaimantPrefix)) return undefined
  const userId = claimant.slice(runClaimantPrefix.length)
  return userId && userId.trim() === userId ? userId : undefined
}

// A job's person: the order's claimant while that order is claimed, else the
// job's latest run claimant from the server summary.
export function jobClaimantUserId(item: ActivityItem, job: Job | undefined, order: WorkOrder | undefined) {
  if (order?.state === 'claimed') {
    const live = runClaimantUserId(order.claimed_by)
    if (live) return live
  }
  const jobId = job?.id ?? order?.job_id
  return (jobId && item.summaries?.job_results?.[jobId]?.claimant_user_id) || undefined
}

// An intervention's person: a human user:<id> actor, or the user who owns a
// retired agent credential (req-security-boundaries AC-1.3).
export function interventionUserId(intervention: Intervention): string | undefined {
  const owner = intervention.actor_owner_user_id?.trim()
  if (owner) return owner
  if (intervention.actor_role !== 'human') return undefined
  const id = intervention.actor_id?.startsWith('user:') ? intervention.actor_id.slice('user:'.length).trim() : ''
  return id || undefined
}

// Names resolve only through the workspace roster: display name, then email,
// then the user ID as a last resort (req-accounts-and-membership AC-4.3). A
// roster that failed to load names the person by user ID and still shows the
// avatar.
export function personFor(userId: string, members: WorkspaceMembership[]): Person {
  const member = members.find((entry) => entry.user_id === userId)
  return { userId, name: member?.display_name?.trim() || member?.email?.trim() || userId }
}

export function usePersonResolver() {
  const members = useWorkspaceMembers().data ?? []
  return (userId: string | undefined) => (userId ? personFor(userId, members) : undefined)
}

const runningWords: Record<string, string> = {
  implement: 'implementing now',
  spec: 'planning now',
  review: 'reviewing now',
  verify: 'verifying now',
}

// The avatar label's second part for a job or order of one stage.
export function stageWork(stage: string, running: boolean) {
  if (running) return runningWords[stage] ?? 'working now'
  return `ran this ${(workOrderStageLabels[stage] ?? stageLabels[stage] ?? stage).toLowerCase()}`
}

export function personLabel(person: Person, what: string) {
  return `${person.name} · ${what}`
}

// Up to two initials from the name's words, or from an email's local part.
export function initials(name: string) {
  const source = name.includes('@') ? name.slice(0, name.indexOf('@')) : name
  const words = source.split(/[^\p{L}\p{N}]+/u).filter(Boolean)
  if (words.length === 0) return '?'
  const letters = words.length === 1 ? [...words[0]].slice(0, 2) : [[...words[0]][0], [...words.at(-1)!][0]]
  return letters.join('').toUpperCase()
}

// The existing soft-token pairs; a stable hash of the user ID picks one so a
// person keeps one tone everywhere.
const tones = [
  'bg-primary-soft text-primary',
  'bg-positive-soft text-positive',
  'bg-document-soft text-document',
  'bg-info-soft text-info',
  'bg-attention-soft text-attention',
] as const

export function avatarTone(userId: string) {
  let hash = 0
  for (const char of userId) hash = (Math.imul(hash, 31) + (char.codePointAt(0) ?? 0)) | 0
  return tones[Math.abs(hash) % tones.length]
}

const sizes = {
  rail: 'size-[22px] text-[9px]',
  inline: 'size-4 text-[7px]',
} as const

// One person's avatar as a disclosure trigger. Its accessible name is the
// person's name, and its label reads "<name> · <what>". A live avatar carries
// a solid primary ring and a halo that pulses only when motion is allowed.
export function PersonAvatar({
  person,
  label,
  size = 'rail',
  live = false,
  className,
}: {
  person: Person
  label: string
  size?: keyof typeof sizes
  live?: boolean
  className?: string
}) {
  return (
    <Disclosure
      className={cn('shrink-0', className)}
      label={person.name}
      content={label}
      renderTrigger={(trigger) => (
        <button
          {...trigger}
          data-person-avatar={person.userId}
          data-live={live ? '' : undefined}
          className={cn(
            trigger.className,
            'relative justify-center rounded-full pointer-coarse:min-h-0 pointer-coarse:min-w-0',
            'focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary',
          )}
        >
          {live && (
            <span
              aria-hidden
              className="absolute -inset-1 rounded-full bg-primary/25 motion-safe:animate-ping"
              data-live-halo
            />
          )}
          {/* The initials draw from an attribute, so the avatar adds no text
              to the row it sits in. */}
          <span
            aria-hidden
            data-initials={initials(person.name)}
            className={cn(
              'relative inline-flex select-none items-center justify-center rounded-full font-semibold leading-none before:content-[attr(data-initials)]',
              sizes[size],
              avatarTone(person.userId),
              live
                ? 'shadow-[0_0_0_2px_var(--color-background),0_0_0_3.5px_var(--color-primary)]'
                : 'shadow-[0_0_0_2px_var(--color-background)]',
            )}
          />
        </button>
      )}
    />
  )
}

// The rail slot: each distinct person's 22 px avatar centred on the rail line,
// stacked down the rail. The avatar's background ring stops the line at its
// edge.
export function RailPeople({ people }: { people: RailPerson[] }) {
  const seen = new Set<string>()
  const distinct = people.filter(({ person }) => !seen.has(person.userId) && seen.add(person.userId))
  return (
    <span className="absolute left-[-3.5px] top-2 z-[1] flex flex-col gap-1.5" data-rail-people>
      {distinct.map(({ person, label, live }) => (
        <PersonAvatar key={person.userId} person={person} label={label} live={live} />
      ))}
    </span>
  )
}
