// Stage preferences: the harness, model, and effort a person's agent sessions
// use for each stage (component-stage-preferences; req-stage-preferences).
// One set per person applies in every workspace, so its cache key carries no
// workspace. The requests live in api.ts with the rest of the contract layer
// (component-web-dashboard invariant 9).

export const stageKeys = ['plan', 'implement', 'verify', 'review'] as const
export type StageKey = (typeof stageKeys)[number]
export type SingleStageKey = Exclude<StageKey, 'review'>

export const maxReviewSeats = 8
export const maxNameBytes = 128
export const maxEffortBytes = 32

export interface StagePreferenceEntry {
  harness: string
  model: string
  effort: string
}

export interface ReviewStagePreference {
  primary: StagePreferenceEntry | null
  seats: StagePreferenceEntry[]
}

export interface StagePreferenceStages {
  plan: StagePreferenceEntry | null
  implement: StagePreferenceEntry | null
  verify: StagePreferenceEntry | null
  review: ReviewStagePreference | null
}

export interface StagePreferences {
  version: number
  updated_at: string | null
  stages: StagePreferenceStages
}

/** The person-scoped cache key: the set is the same in every workspace. */
export function stagePreferencesQueryKey() {
  return ['stage-preferences'] as const
}

export const stageLabels: Record<StageKey, string> = {
  plan: 'Plan',
  implement: 'Implement',
  verify: 'Verify',
  review: 'Review',
}

export function emptyStages(): StagePreferenceStages {
  return { plan: null, implement: null, verify: null, review: null }
}

export function emptyEntry(): StagePreferenceEntry {
  return { harness: '', model: '', effort: '' }
}

/** A deep copy, so an edit to a draft never reaches the cached set. */
export function copyStages(stages: StagePreferenceStages): StagePreferenceStages {
  const entry = (value: StagePreferenceEntry | null) => (value ? { ...value, effort: value.effort ?? '' } : null)
  return {
    plan: entry(stages.plan),
    implement: entry(stages.implement),
    verify: entry(stages.verify),
    review: stages.review
      ? { primary: entry(stages.review.primary), seats: (stages.review.seats ?? []).map((seat) => ({ ...seat })) }
      : null,
  }
}

/**
 * The stages a save sends: text trimmed, and a review with no primary and no
 * seats sent as unset (req-stage-preferences AC-1.2).
 */
export function stagesForSave(stages: StagePreferenceStages): StagePreferenceStages {
  const entry = (value: StagePreferenceEntry | null) =>
    value ? { harness: value.harness.trim(), model: value.model.trim(), effort: value.effort.trim() } : null
  const review = stages.review
  return {
    plan: entry(stages.plan),
    implement: entry(stages.implement),
    verify: entry(stages.verify),
    review:
      review && (review.primary || review.seats.length > 0)
        ? { primary: entry(review.primary), seats: review.seats.map((seat) => entry(seat) as StagePreferenceEntry) }
        : null,
  }
}

export function sameStages(left: StagePreferenceStages, right: StagePreferenceStages) {
  return JSON.stringify(stagesForSave(left)) === JSON.stringify(stagesForSave(right))
}

function byteLength(value: string) {
  return new TextEncoder().encode(value).length
}

function hasControlCharacter(value: string) {
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0
    if (code < 0x20 || (code >= 0x7f && code <= 0x9f)) return true
  }
  return false
}

function entryProblem(label: string, entry: StagePreferenceEntry | null): string | null {
  if (!entry) return null
  for (const [name, value, limit, required] of [
    ['harness', entry.harness, maxNameBytes, true],
    ['model', entry.model, maxNameBytes, true],
    ['effort', entry.effort, maxEffortBytes, false],
  ] as const) {
    if (required && !value.trim()) return `${label}: enter a ${name}.`
    if (byteLength(value.trim()) > limit) return `${label}: the ${name} must be ${limit} bytes or fewer.`
    if (hasControlCharacter(value)) return `${label}: the ${name} must not contain a control character.`
  }
  return null
}

/**
 * The first problem the server would refuse in a draft, named in the card's
 * own words, or null (component-stage-preferences invariant 2).
 */
export function draftProblem(stages: StagePreferenceStages): string | null {
  for (const stage of ['plan', 'implement', 'verify'] as const) {
    const problem = entryProblem(stageLabels[stage], stages[stage])
    if (problem) return problem
  }
  const review = stages.review
  if (!review) return null
  const primary = entryProblem('Review primary', review.primary)
  if (primary) return primary
  if (review.seats.length > maxReviewSeats) return `Review: use ${maxReviewSeats} seats or fewer.`
  for (const [index, seat] of review.seats.entries()) {
    const problem = entryProblem(`Review seat ${index + 1}`, seat)
    if (problem) return problem
  }
  return null
}

/**
 * Names a refused field from the server, such as stages.review.seats[2].model,
 * in the card's words: "Review seat 3 model".
 */
export function describeStagePreferenceField(field: string, position?: number): string {
  const parts = field.replace(/^stages\./, '').split('.')
  const words: string[] = []
  for (const part of parts) {
    const seat = /^seats\[(\d+)\]$/.exec(part)
    if (seat) {
      words.push(`seat ${position ?? Number(seat[1]) + 1}`)
    } else if (part in stageLabels) {
      words.push(stageLabels[part as StageKey])
    } else {
      words.push(part)
    }
  }
  const text = words.join(' ')
  return text.charAt(0).toUpperCase() + text.slice(1)
}
