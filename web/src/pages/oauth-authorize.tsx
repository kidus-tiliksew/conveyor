import { useNavigate } from '@tanstack/react-router'
import { LoaderCircle, ShieldAlert, ShieldCheck } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { Button } from '../components/ui/button'
import {
  decideOAuthAuthorization,
  fetchOAuthAuthorization,
  fetchOAuthConsentCaller,
  OAuthRequestError,
  oauthAuthorizePath,
  safeOAuthRedirect,
  safeOAuthReturnPath,
} from '../lib/api'
import type { CallerIdentity, OAuthConsentView, OAuthDecision } from '../lib/types'

// The authorization query as the browser loaded this document. The router
// rewrites a loaded address into its own search encoding, so the page keeps
// the client's exact string here, read before the router starts. A return
// from sign-in opens this page as a new document for the same reason
// (req-mcp-oauth AC-3.1; component-web-dashboard invariant 37).
const loadedSearch = window.location.pathname === oauthAuthorizePath ? window.location.search : undefined

function authorizationSearch() {
  return loadedSearch ?? window.location.search
}

type PageState =
  | { kind: 'loading' }
  | { kind: 'ready'; view: OAuthConsentView; caller: CallerIdentity }
  | { kind: 'failed'; message: string }
  | { kind: 'leaving'; host: string }

// One message for each failure the page shows. The text comes from the
// contract layer, and the page branches only on the status it keeps
// (component-web-dashboard invariant 13).
function failureMessage(error: unknown) {
  if (error instanceof OAuthRequestError) {
    if (error.status === 404 && error.code === 'oauth_unavailable')
      return 'This Conveyor server does not offer MCP sign-in. Ask your operator to turn it on.'
    // A decision refused with 401: the session ended after the page opened.
    if (error.status === 401) return 'Your sign-in session ended. Reload this page to sign in again.'
    return error.message
  }
  return 'Conveyor did not answer. Check your connection and reload this page.'
}

// Opens the client's address as a top-level navigation that sends no referrer,
// so the consent page's query does not reach the client.
function openWithoutReferrer(address: string) {
  const link = document.createElement('a')
  link.href = address
  link.rel = 'noreferrer'
  link.referrerPolicy = 'no-referrer'
  document.body.append(link)
  link.click()
  link.remove()
}

/**
 * The OAuth consent page at `/oauth/authorize` (req-mcp-oauth AC-3.1, AC-3.2,
 * AC-3.4; DEC-146(3)). It reads the caller and the verified request, sends a
 * signed-out browser to sign-in with a validated return path, shows an
 * unverified request's error on this page, and sends one explicit decision.
 * Nothing it reads or receives enters the Query cache or browser storage.
 */
export function OAuthAuthorizePage() {
  const navigate = useNavigate()
  const search = authorizationSearch()
  const [state, setState] = useState<PageState>({ kind: 'loading' })
  const [pending, setPending] = useState<OAuthDecision | undefined>()
  const [decisionError, setDecisionError] = useState('')
  // The request that the current decision belongs to. A result that arrives
  // after the page leaves, or after the request changes, is ignored.
  const activeSearch = useRef<string | undefined>(search)
  // Set while a decision request is in flight, so a second click sends nothing.
  const deciding = useRef(false)

  useEffect(() => {
    activeSearch.current = search
    const controller = new AbortController()
    setState({ kind: 'loading' })
    setPending(undefined)
    setDecisionError('')
    deciding.current = false
    void Promise.all([fetchOAuthConsentCaller(controller.signal), fetchOAuthAuthorization(search, controller.signal)])
      .then(([caller, view]) => {
        if (!controller.signal.aborted) setState({ kind: 'ready', view, caller })
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted) return
        if (error instanceof OAuthRequestError && error.status === 401) {
          // A signed-out browser signs in and returns to this exact request.
          // `next` is encoded once, as one search value.
          const next = safeOAuthReturnPath(`${oauthAuthorizePath}${search}`)
          void navigate({ to: '/sign-in', search: { next }, replace: true })
          return
        }
        setState({ kind: 'failed', message: failureMessage(error) })
      })
    return () => {
      controller.abort()
      activeSearch.current = undefined
    }
  }, [navigate, search])

  async function decide(decision: OAuthDecision, view: OAuthConsentView) {
    if (deciding.current) return
    deciding.current = true
    const requestSearch = search
    setPending(decision)
    setDecisionError('')
    try {
      const result = await decideOAuthAuthorization(requestSearch, decision)
      if (activeSearch.current !== requestSearch) return
      const target = safeOAuthRedirect(
        result.redirect_to,
        view.redirect_host,
        new URLSearchParams(requestSearch).get('redirect_uri'),
      )
      if (!target) {
        setDecisionError('Conveyor did not open the client. The address in the answer is not safe.')
        setPending(undefined)
        deciding.current = false
        return
      }
      setState({ kind: 'leaving', host: view.redirect_host })
      openWithoutReferrer(target)
    } catch (error) {
      if (activeSearch.current !== requestSearch) return
      setDecisionError(failureMessage(error))
      setPending(undefined)
      deciding.current = false
    }
  }

  return (
    <main className="grid min-h-screen place-items-center bg-background px-4 py-10 sm:px-6">
      <section className="w-full max-w-md rounded-xl border border-border bg-card p-6 shadow-sm sm:p-7">
        {state.kind === 'loading' && (
          <div className="text-center" role="status">
            <LoaderCircle className="mx-auto mb-4 size-6 animate-spin text-primary" aria-hidden="true" />
            <h1 className="text-lg font-semibold tracking-tight">Checking the sign-in request…</h1>
          </div>
        )}
        {state.kind === 'failed' && (
          <div className="text-center">
            <ShieldAlert className="mx-auto mb-4 size-7 text-failure" aria-hidden="true" />
            <h1 className="text-lg font-semibold tracking-tight">This sign-in request cannot continue</h1>
            <p className="mt-3 text-sm leading-6 text-foreground" role="alert">
              {state.message}
            </p>
            <p className="mt-3 text-sm leading-6 text-muted">
              Conveyor did not send you back to the client. Start the sign-in again from your coding agent.
            </p>
          </div>
        )}
        {state.kind === 'leaving' && (
          <div className="text-center" role="status">
            <LoaderCircle className="mx-auto mb-4 size-6 animate-spin text-primary" aria-hidden="true" />
            <h1 className="text-lg font-semibold tracking-tight">Returning you to the client…</h1>
            <p className="mt-2 text-sm leading-6 text-muted">
              Your browser opens <span className="font-mono text-foreground">{state.host}</span>.
            </p>
          </div>
        )}
        {state.kind === 'ready' && (
          <ConsentDetails
            view={state.view}
            caller={state.caller}
            pending={pending}
            error={decisionError}
            onDecide={(decision) => void decide(decision, state.view)}
          />
        )}
      </section>
    </main>
  )
}

function ConsentDetails({
  view,
  caller,
  pending,
  error,
  onDecide,
}: {
  view: OAuthConsentView
  caller: CallerIdentity
  pending: OAuthDecision | undefined
  error: string
  onDecide: (decision: OAuthDecision) => void
}) {
  const clientName = view.client.client_name.trim() || 'An unnamed MCP client'
  const displayName = view.user.display_name || caller.display_name
  const email = view.user.email || caller.email
  return (
    <div>
      <div className="text-center">
        <img src="/favicon.svg" alt="" className="mx-auto mb-4 size-10" />
        <h1 className="text-lg font-semibold tracking-tight break-words">Allow {clientName} to use Conveyor?</h1>
        <p className="mt-2 text-sm leading-6 text-muted">Check these details before you approve.</p>
      </div>
      <dl className="mt-6 divide-y divide-border rounded-lg border border-border text-sm">
        <div className="space-y-1 p-3">
          <dt className="text-xs font-medium text-muted">Client</dt>
          <dd className="font-medium break-words text-foreground" data-testid="oauth-client-name">
            {clientName}
          </dd>
          {view.client.kind === 'metadata_document' && view.client.origin ? (
            <dd className="text-muted">
              Published at{' '}
              <span className="font-mono break-all text-foreground" data-testid="oauth-client-origin">
                {view.client.origin}
              </span>
            </dd>
          ) : (
            <dd className="text-muted" data-testid="oauth-client-dynamic">
              Dynamic registration. The client registered itself, so Conveyor cannot confirm who made it.
            </dd>
          )}
        </div>
        <div className="space-y-1 p-3">
          <dt className="text-xs font-medium text-muted">Returns you to</dt>
          <dd className="font-mono break-all text-foreground" data-testid="oauth-redirect-host">
            {view.redirect_host}
          </dd>
        </div>
        <div className="space-y-1 p-3">
          <dt className="text-xs font-medium text-muted">Signed in as</dt>
          <dd className="break-words text-foreground" data-testid="oauth-signed-in-user">
            {displayName ? (
              <>
                <span className="font-medium">{displayName}</span>
                {email && <span className="text-muted"> ({email})</span>}
              </>
            ) : (
              email || view.user.id
            )}
          </dd>
        </div>
      </dl>
      <div
        className="mt-4 flex gap-2 rounded-lg border border-border bg-surface p-3 text-sm leading-6 text-foreground"
        data-testid="oauth-scope-notice"
      >
        <ShieldCheck className="mt-1 size-4 shrink-0 text-primary" aria-hidden="true" />
        <p>
          If you approve, this client can use only Conveyor's MCP endpoint, <code className="font-mono">/mcp</code>. It
          acts as you, with your workspace roles. It cannot open the dashboard or use other Conveyor APIs.
        </p>
      </div>
      {error && (
        <p className="mt-4 text-sm leading-5 text-failure" role="alert">
          {error}
        </p>
      )}
      <div className="mt-6 grid grid-cols-2 gap-2">
        <Button type="button" variant="secondary" disabled={pending !== undefined} onClick={() => onDecide('deny')}>
          {pending === 'deny' ? 'Denying…' : 'Deny'}
        </Button>
        <Button type="button" disabled={pending !== undefined} onClick={() => onDecide('approve')}>
          {pending === 'approve' ? 'Approving…' : 'Approve'}
        </Button>
      </div>
    </div>
  )
}
