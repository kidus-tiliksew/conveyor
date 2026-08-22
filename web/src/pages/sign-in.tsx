import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { CheckCircle2, KeyRound, Link2, LoaderCircle } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { redeemSignInLink, signInWithPassword } from '../lib/api'

type State = 'checking' | 'success' | 'invalid'

export function SignInPage() {
  const [hash, setHash] = useState(() => window.location.hash)
  const token = new URLSearchParams(hash.replace(/^#/, '')).get('token') ?? undefined
  const [state, setState] = useState<State>(token ? 'checking' : 'invalid')
  const processedToken = useRef<string | undefined>(undefined)
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [passwordError, setPasswordError] = useState('')
  const [passwordPending, setPasswordPending] = useState(false)

  useEffect(() => {
    const readHash = () => setHash(window.location.hash)
    window.addEventListener('hashchange', readHash)
    return () => window.removeEventListener('hashchange', readHash)
  }, [])

  useEffect(() => {
    if (!token) {
      processedToken.current = undefined
      setState('invalid')
      return
    }
    if (processedToken.current === token) return
    processedToken.current = token
    setState('checking')
    window.history.replaceState(window.history.state, '', `${window.location.pathname}${window.location.search}`)
    void redeemSignInLink(token)
      .then(async () => {
        queryClient.clear()
        setState('success')
        await navigate({ to: '/onboarding', replace: true })
      })
      .catch(() => setState('invalid'))
  }, [navigate, queryClient, token])

  async function submitPassword(event: React.FormEvent) {
    event.preventDefault()
    setPasswordPending(true)
    setPasswordError('')
    try {
      await signInWithPassword(email, password)
      queryClient.clear()
      await navigate({ to: '/', replace: true })
    } catch {
      setPasswordError(
        'Email or password was not accepted. Ask your operator for a fresh sign-in link if you forgot it.',
      )
    } finally {
      setPasswordPending(false)
    }
  }

  return (
    <main className="grid min-h-screen place-items-center bg-background px-6 py-12">
      <section className="w-full max-w-md rounded-xl border border-border bg-card p-8 shadow-sm">
        <div className="text-center">
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
                : token
                  ? 'This link no longer works'
                  : 'Sign in to Conveyor'}
          </h1>
          <p className="mt-3 text-sm leading-6 text-muted" role={state === 'invalid' && token ? 'alert' : undefined}>
            {state === 'checking'
              ? 'This should only take a moment.'
              : state === 'success'
                ? 'Opening profile setup…'
                : token
                  ? 'It may have expired or already been used. Ask your operator to issue a fresh sign-in link.'
                  : 'Use your account password, or ask your operator to issue a one-time sign-in link.'}
          </p>
        </div>
        {state !== 'checking' && state !== 'success' && (
          <>
            <div className="my-6 flex items-center gap-3 text-xs text-faint">
              <span className="h-px flex-1 bg-border" />
              Password sign-in
              <span className="h-px flex-1 bg-border" />
            </div>
            <form className="space-y-3" aria-label="Password sign-in" onSubmit={submitPassword}>
              <Input
                type="email"
                aria-label="Email address"
                autoComplete="email"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
              />
              <Input
                type="password"
                aria-label="Password"
                autoComplete="current-password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
              <Button type="submit" className="w-full" disabled={passwordPending || !email.trim() || !password}>
                <KeyRound aria-hidden="true" />
                {passwordPending ? 'Signing in…' : 'Sign in'}
              </Button>
              {passwordError && (
                <p className="text-sm leading-5 text-failure" role="alert">
                  {passwordError}
                </p>
              )}
            </form>
            <p className="mt-5 text-xs leading-5 text-faint">
              Locked out? On the Conveyor host, an operator can run{' '}
              <code className="font-mono">conveyor user issue-link &lt;email&gt;</code> to issue a fresh one-time
              sign-in link.
            </p>
          </>
        )}
      </section>
    </main>
  )
}
