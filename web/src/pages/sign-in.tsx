import { useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { CheckCircle2, Link2, LoaderCircle } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { redeemSignInLink } from '../lib/api'

type State = 'checking' | 'success' | 'invalid'

export function SignInPage() {
  const { token } = useSearch({ strict: false }) as { token?: string }
  const [state, setState] = useState<State>(token ? 'checking' : 'invalid')
  const started = useRef(false)
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  useEffect(() => {
    if (!token || started.current) return
    started.current = true
    void redeemSignInLink(token)
      .then(async () => {
        sessionStorage.removeItem('conveyor-token')
        queryClient.clear()
        setState('success')
        await navigate({ to: '/settings', search: { welcome: true }, replace: true })
      })
      .catch(() => setState('invalid'))
  }, [navigate, queryClient, token])

  return (
    <main className="grid min-h-screen place-items-center bg-background px-6 py-12">
      <section className="w-full max-w-md rounded-xl border border-border bg-card p-8 text-center shadow-sm">
        <div className="mx-auto mb-5 grid size-12 place-items-center rounded-xl bg-primary-soft text-primary">
          {state === 'checking' ? (
            <LoaderCircle className="size-6 animate-spin" aria-hidden="true" />
          ) : state === 'success' ? (
            <CheckCircle2 className="size-6" aria-hidden="true" />
          ) : (
            <Link2 className="size-6" aria-hidden="true" />
          )}
        </div>
        <h1 className="text-xl font-semibold tracking-tight">
          {state === 'checking'
            ? 'Signing you in…'
            : state === 'success'
              ? 'You’re signed in'
              : 'This link no longer works'}
        </h1>
        <p className="mt-3 text-sm leading-6 text-muted" role={state === 'invalid' ? 'alert' : undefined}>
          {state === 'checking'
            ? 'This should only take a moment.'
            : state === 'success'
              ? 'Opening your workspace…'
              : 'It may have expired or already been used. Ask your operator to resend your invitation.'}
        </p>
      </section>
    </main>
  )
}
