import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { LoaderCircle, UserRoundCheck } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { fetchOwnProfile, updateOwnDisplayName, updateOwnPassword } from '../lib/api'

export function OnboardingPage() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [loading, setLoading] = useState(true)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let active = true
    void fetchOwnProfile()
      .then((profile) => {
        if (active) setDisplayName(profile.display_name)
      })
      .catch(() => {
        if (active) setError('Your sign-in session is unavailable. Ask your operator for a fresh sign-in link.')
      })
      .finally(() => {
        if (active) setLoading(false)
      })
    return () => {
      active = false
    }
  }, [])

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setError('')
    if (password !== confirmPassword) {
      setError('Passwords do not match.')
      return
    }
    setPending(true)
    try {
      await updateOwnDisplayName(displayName)
      await updateOwnPassword('', password)
      queryClient.clear()
      await navigate({ to: '/', replace: true })
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : 'Could not complete profile setup.')
    } finally {
      setPending(false)
    }
  }

  return (
    <main className="grid min-h-screen place-items-center bg-background px-6 py-12">
      <section className="w-full max-w-md rounded-xl border border-border bg-card p-8 shadow-sm">
        <div className="mx-auto mb-5 grid size-12 place-items-center rounded-xl bg-primary-soft text-primary">
          {loading ? (
            <LoaderCircle className="size-6 animate-spin" aria-hidden="true" />
          ) : (
            <UserRoundCheck className="size-6" aria-hidden="true" />
          )}
        </div>
        <div className="text-center">
          <h1 className="text-xl font-semibold tracking-tight">Complete your profile</h1>
          <p className="mt-3 text-sm leading-6 text-muted">
            Choose the name teammates will see and set your password before opening a workspace.
          </p>
        </div>
        {!loading && (
          <form className="mt-6 space-y-3" aria-label="Complete your profile" onSubmit={submit}>
            <Input
              aria-label="Display name"
              autoComplete="name"
              maxLength={128}
              value={displayName}
              onChange={(event) => setDisplayName(event.target.value)}
            />
            <Input
              type="password"
              aria-label="New password"
              autoComplete="new-password"
              placeholder="Password (12 characters minimum)"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
            <Input
              type="password"
              aria-label="Confirm new password"
              autoComplete="new-password"
              placeholder="Confirm password"
              value={confirmPassword}
              onChange={(event) => setConfirmPassword(event.target.value)}
            />
            <Button
              type="submit"
              className="w-full"
              disabled={pending || !displayName.trim() || password.length < 12 || !confirmPassword}
            >
              {pending ? 'Opening Conveyor…' : 'Complete setup'}
            </Button>
          </form>
        )}
        {error && (
          <p className="mt-4 text-sm leading-5 text-failure" role="alert">
            {error}
          </p>
        )}
      </section>
    </main>
  )
}
