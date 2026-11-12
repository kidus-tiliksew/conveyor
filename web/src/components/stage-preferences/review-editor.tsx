import { ArrowDown, ArrowUp, Plus, X } from 'lucide-react'
import {
  emptyEntry,
  maxReviewSeats,
  type ReviewStagePreference,
  type StagePreferenceEntry,
} from '../../lib/stage-preferences'
import { Button } from '../ui/button'
import { EntryFields } from './entry-fields'

// The review stage: an optional primary entry and up to eight ordered seat
// entries, where seat N uses entry N or else the primary
// (req-stage-preferences AC-1.2, AC-2.1).
export function ReviewEditor({
  review,
  disabled,
  onChange,
}: {
  review: ReviewStagePreference | null
  disabled?: boolean
  onChange: (review: ReviewStagePreference | null) => void
}) {
  const current: ReviewStagePreference = review ?? { primary: null, seats: [] }
  const change = (next: ReviewStagePreference) => onChange(next)
  const setSeat = (index: number, entry: StagePreferenceEntry) =>
    change({ ...current, seats: current.seats.map((seat, at) => (at === index ? entry : seat)) })
  const moveSeat = (index: number, offset: -1 | 1) => {
    const seats = [...current.seats]
    const [moved] = seats.splice(index, 1)
    seats.splice(index + offset, 0, moved)
    change({ ...current, seats })
  }
  const removeSeat = (index: number) => change({ ...current, seats: current.seats.filter((_, at) => at !== index) })
  const full = current.seats.length >= maxReviewSeats

  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <div className="flex items-center justify-between gap-2">
          <h4 className="text-sm font-medium text-foreground">Primary</h4>
          {current.primary ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={disabled}
              onClick={() => change({ ...current, primary: null })}
            >
              Clear primary
            </Button>
          ) : (
            <Button
              type="button"
              variant="secondary"
              size="sm"
              disabled={disabled}
              onClick={() => change({ ...current, primary: emptyEntry() })}
            >
              Set primary
            </Button>
          )}
        </div>
        <p className="text-xs leading-5 text-muted">
          The primary agent starts the seat agents. A seat without its own entry uses the primary.
        </p>
        {current.primary ? (
          <EntryFields
            label="Review primary"
            entry={current.primary}
            disabled={disabled}
            onChange={(primary) => change({ ...current, primary })}
          />
        ) : (
          <p className="text-sm text-faint">Not set</p>
        )}
      </div>

      <div className="space-y-2">
        <div className="flex items-center justify-between gap-2">
          <h4 className="text-sm font-medium text-foreground">Seats</h4>
          <span className="text-xs text-muted">
            {current.seats.length} of {maxReviewSeats}
          </span>
        </div>
        {current.seats.length === 0 && <p className="text-sm text-faint">No seat has its own entry.</p>}
        <ol className="space-y-3" aria-label="Review seats">
          {current.seats.map((seat, index) => (
            // A seat's position is its identity: it is the seat number.
            <li key={index} className="space-y-2 rounded-md border border-border p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-sm font-medium text-foreground">Seat {index + 1}</span>
                <div className="flex items-center gap-1">
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={`Move seat ${index + 1} up`}
                    disabled={disabled || index === 0}
                    onClick={() => moveSeat(index, -1)}
                  >
                    <ArrowUp />
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={`Move seat ${index + 1} down`}
                    disabled={disabled || index === current.seats.length - 1}
                    onClick={() => moveSeat(index, 1)}
                  >
                    <ArrowDown />
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={`Remove seat ${index + 1}`}
                    disabled={disabled}
                    onClick={() => removeSeat(index)}
                  >
                    <X />
                  </Button>
                </div>
              </div>
              <EntryFields
                label={`Review seat ${index + 1}`}
                entry={seat}
                disabled={disabled}
                onChange={(entry) => setSeat(index, entry)}
              />
            </li>
          ))}
        </ol>
        <Button
          type="button"
          variant="secondary"
          size="sm"
          disabled={disabled || full}
          onClick={() => change({ ...current, seats: [...current.seats, emptyEntry()] })}
        >
          <Plus />
          Add seat
        </Button>
        {full && <p className="text-xs text-muted">Review can have {maxReviewSeats} seat entries at most.</p>}
      </div>
    </div>
  )
}
