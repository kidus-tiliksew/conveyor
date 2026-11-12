import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { SlidersHorizontal } from 'lucide-react'
import { useState } from 'react'
import { fetchStagePreferences, StagePreferencesError, saveStagePreferences } from '../../lib/api'
import {
  copyStages,
  draftProblem,
  emptyEntry,
  emptyStages,
  type SingleStageKey,
  type StagePreferenceStages,
  type StagePreferences,
  sameStages,
  stageLabels,
  stagePreferencesQueryKey,
  stagesForSave,
} from '../../lib/stage-preferences'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '../ui/card'
import { describeEntry, EntryFields } from './entry-fields'
import { ReviewEditor } from './review-editor'

// The person's stage preferences on Settings (component-stage-preferences;
// component-web-dashboard invariant 25; req-stage-preferences AC-2.1). Edits
// are a draft that a later read never replaces. A save sends the whole set
// with the version the draft started from. A stale save keeps the draft and
// shows the current set beside it, so neither side is lost without the
// person's choice.

type Draft = { stages: StagePreferenceStages; baseVersion: number }
type Status = { text: string; failed: boolean } | null

const singleStages: SingleStageKey[] = ['plan', 'implement', 'verify']

export function StagePreferencesCard() {
  const queryClient = useQueryClient()
  const current = useQuery({ queryKey: stagePreferencesQueryKey(), queryFn: fetchStagePreferences })
  const [draft, setDraft] = useState<Draft | null>(null)
  const [conflict, setConflict] = useState(false)
  const [status, setStatus] = useState<Status>(null)

  const save = useMutation({
    mutationFn: (sent: Draft) => saveStagePreferences(stagesForSave(sent.stages), sent.baseVersion),
    onSuccess: (saved, sent) => {
      queryClient.setQueryData<StagePreferences>(stagePreferencesQueryKey(), saved)
      // Edits typed while the save was in flight stay, based on the saved
      // version.
      setDraft((latest) =>
        !latest || latest.stages === sent.stages ? null : { stages: latest.stages, baseVersion: saved.version },
      )
      setConflict(false)
      setStatus({ text: 'Stage preferences saved.', failed: false })
    },
    onError: async (cause) => {
      const stale = cause instanceof StagePreferencesError && cause.status === 412
      setConflict(stale)
      // The stale-save panel states a conflict itself.
      setStatus(
        stale
          ? null
          : { text: cause instanceof Error ? cause.message : 'Could not save stage preferences.', failed: true },
      )
      if (stale) await queryClient.invalidateQueries({ queryKey: stagePreferencesQueryKey() })
    },
  })

  const saved = current.data
  const shown = draft?.stages ?? saved?.stages ?? emptyStages()
  const editing = Boolean(draft)
  const busy = save.isPending
  const unchanged = !draft || (saved !== undefined && !conflict && sameStages(draft.stages, saved.stages))

  const edit = (change: (stages: StagePreferenceStages) => void) => {
    if (!saved) return
    const base: Draft = draft ?? { stages: copyStages(saved.stages), baseVersion: saved.version }
    const next = copyStages(base.stages)
    change(next)
    setDraft({ ...base, stages: next })
    if (!conflict) setStatus(null)
  }

  const submit = (baseVersion?: number) => {
    if (!draft) return
    const problem = draftProblem(draft.stages)
    if (problem) {
      setStatus({ text: problem, failed: true })
      return
    }
    setStatus(null)
    save.mutate(baseVersion === undefined ? draft : { ...draft, baseVersion })
  }

  const discard = () => {
    setDraft(null)
    setConflict(false)
    setStatus(null)
  }

  return (
    <Card className="mt-4">
      <CardHeader>
        <CardTitle>Stage preferences</CardTitle>
        <SlidersHorizontal className="size-4 text-faint" />
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-1 text-sm leading-6 text-muted">
          <p>Your agent sessions use these choices when they start an agent for a stage.</p>
          <p>
            <span className="font-medium text-foreground">These preferences apply in every workspace.</span> They are
            advice to your own sessions. No factory decision reads them.
          </p>
        </div>

        {current.isPending && <p className="text-sm text-muted">Loading your stage preferences…</p>}

        {current.isError && !draft && (
          <div className="space-y-2" role="alert">
            <p className="text-sm text-failure">{current.error.message}</p>
            <Button type="button" variant="secondary" size="sm" onClick={() => void current.refetch()}>
              Try again
            </Button>
          </div>
        )}

        {conflict && draft && (
          <section
            aria-label="Saved stage preferences"
            className="space-y-3 rounded-md border border-attention/40 bg-attention-soft/30 p-3"
          >
            <div className="space-y-1" role="alert">
              <p className="text-sm font-medium text-foreground">Your stage preferences changed in another place.</p>
              <p className="text-sm leading-6 text-muted">
                The saved preferences are below, and your unsaved changes stay in the form. Save your changes to replace
                the saved preferences, or discard your changes to keep them.
              </p>
            </div>
            {current.isFetching && <p className="text-sm text-muted">Reading the saved preferences…</p>}
            {current.isError && <p className="text-sm text-failure">{current.error.message}</p>}
            {saved && !current.isFetching && (
              <>
                <SavedSummary stages={saved.stages} />
                <div className="flex flex-wrap gap-2">
                  <Button type="button" size="sm" disabled={busy} onClick={() => submit(saved.version)}>
                    Save my changes
                  </Button>
                  <Button type="button" variant="secondary" size="sm" disabled={busy} onClick={discard}>
                    Discard my changes
                  </Button>
                </div>
              </>
            )}
          </section>
        )}

        {saved && (
          <form
            aria-label="Edit stage preferences"
            className="space-y-5"
            onSubmit={(event) => {
              event.preventDefault()
              submit()
            }}
          >
            {conflict && draft && <h3 className="text-sm font-medium text-foreground">Your unsaved changes</h3>}
            {singleStages.map((stage) => {
              const entry = shown[stage]
              return (
                <section key={stage} aria-label={`${stageLabels[stage]} stage`} className="space-y-2">
                  <div className="flex items-center justify-between gap-2">
                    <h3 className="text-sm font-medium text-foreground">{stageLabels[stage]}</h3>
                    {entry ? (
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        disabled={busy}
                        onClick={() =>
                          edit((stages) => {
                            stages[stage] = null
                          })
                        }
                      >
                        Clear {stageLabels[stage].toLowerCase()}
                      </Button>
                    ) : (
                      <Button
                        type="button"
                        variant="secondary"
                        size="sm"
                        disabled={busy}
                        onClick={() =>
                          edit((stages) => {
                            stages[stage] = emptyEntry()
                          })
                        }
                      >
                        Set {stageLabels[stage].toLowerCase()}
                      </Button>
                    )}
                  </div>
                  {entry ? (
                    <EntryFields
                      label={stageLabels[stage]}
                      entry={entry}
                      disabled={busy}
                      onChange={(next) =>
                        edit((stages) => {
                          stages[stage] = next
                        })
                      }
                    />
                  ) : (
                    <p className="text-sm text-faint">Not set</p>
                  )}
                </section>
              )
            })}
            <section aria-label="Review stage" className="space-y-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-sm font-medium text-foreground">Review</h3>
                {shown.review && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    disabled={busy}
                    onClick={() =>
                      edit((stages) => {
                        stages.review = null
                      })
                    }
                  >
                    Clear review
                  </Button>
                )}
              </div>
              {!shown.review && <p className="text-sm text-faint">Not set</p>}
              <ReviewEditor
                review={shown.review}
                disabled={busy}
                onChange={(review) =>
                  edit((stages) => {
                    stages.review = review
                  })
                }
              />
            </section>

            <div className="flex flex-wrap items-center gap-2 border-t border-border pt-4">
              {!conflict && (
                <Button type="submit" disabled={busy || unchanged}>
                  {busy ? 'Saving…' : 'Save stage preferences'}
                </Button>
              )}
              {editing && !conflict && (
                <Button type="button" variant="secondary" disabled={busy} onClick={discard}>
                  Discard changes
                </Button>
              )}
            </div>
          </form>
        )}

        {status && (
          <p className={`text-sm ${status.failed ? 'text-failure' : 'text-positive'}`} role="status">
            {status.text}
          </p>
        )}
      </CardContent>
    </Card>
  )
}

// The current saved set, read-only, shown beside a stale draft.
function SavedSummary({ stages }: { stages: StagePreferenceStages }) {
  const review = stages.review
  return (
    <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-sm">
      {singleStages.map((stage) => (
        <div key={stage} className="contents">
          <dt className="text-muted">{stageLabels[stage]}</dt>
          <dd className="min-w-0 break-words text-foreground">{describeEntry(stages[stage])}</dd>
        </div>
      ))}
      <dt className="text-muted">Review</dt>
      <dd className="min-w-0 break-words text-foreground">
        {review ? (
          <ul className="space-y-0.5">
            <li>Primary: {describeEntry(review.primary)}</li>
            {review.seats.map((seat, index) => (
              <li key={index}>
                Seat {index + 1}: {describeEntry(seat)}
              </li>
            ))}
          </ul>
        ) : (
          'Not set'
        )}
      </dd>
    </dl>
  )
}
