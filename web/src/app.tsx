import { createContext, useContext, useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, createRootRoute, createRoute, createRouter, useParams } from '@tanstack/react-router'
import { fetchActivity, fetchTaskActivity, reviewTask } from './api'
import { Button } from './components/ui/button'
import { interventionActions, pipelineGroups, reasonCodes, type InterventionAction } from './lib/contracts'
import type { ActivityItem, ActivitySummary, Job } from './lib/types'
import { cn, duration, relativeTime } from './lib/utils'

const AuthTokenContext = createContext('')

function groupFor(item: ActivitySummary) {
  if (item.task.state === 'awaiting_human' || item.task.state === 'parked') return 'human'
  const stage = item.latest_stage
  if (stage === 'spec' || stage === 'review' || stage === 'verify') return stage
  if (item.task.state === 'running' || stage === 'implement') return 'implement'
  if (item.task.state === 'approved' || item.task.state === 'merged' || item.task.state === 'closed') return 'review'
  return 'triage'
}

function useActivity() {
  return useQuery({ queryKey: ['activity'], queryFn: fetchActivity })
}

function Layout() {
  const { data = [], isLoading, error } = useActivity()
  const [token, setToken] = useState(() => sessionStorage.getItem('conveyor-token') ?? '')
  const grouped = useMemo(() => Object.fromEntries(pipelineGroups.map(([key]) => [key, data.filter((item) => groupFor(item) === key)])), [data])

  const saveToken = (value: string) => {
    setToken(value)
    sessionStorage.setItem('conveyor-token', value)
  }

  return (
    <div className="min-h-screen bg-[#10120f] text-stone-100">
      <header className="sticky top-0 z-20 flex h-16 items-center justify-between border-b border-stone-800 bg-[#10120f]/95 px-5 backdrop-blur">
        <Link to="/" className="flex items-center gap-3">
          <span className="grid size-8 place-items-center rounded-md bg-lime-300 font-mono text-sm font-black text-stone-950">CV</span>
          <span><strong className="block leading-none">Conveyor</strong><small className="text-stone-500">Activity</small></span>
        </Link>
        <label className="flex items-center gap-2 text-xs text-stone-500">
          Review token
          <input aria-label="Review token" type="password" value={token} onChange={(event) => saveToken(event.target.value)} placeholder="Required to act" className="w-48 rounded-md border border-stone-700 bg-stone-900 px-2.5 py-1.5 text-stone-200 outline-none focus:border-stone-500" />
        </label>
      </header>
      <div className="grid min-h-[calc(100vh-4rem)] grid-cols-1 lg:grid-cols-[390px_1fr]">
        <aside className="border-r border-stone-800 bg-[#151713] p-3 lg:h-[calc(100vh-4rem)] lg:overflow-y-auto">
          {isLoading && <p className="p-4 text-sm text-stone-500">Loading factory activity…</p>}
          {error && <p className="m-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-200">{String(error)}</p>}
          {pipelineGroups.map(([key, label]) => {
            const items = grouped[key] ?? []
            return <section key={key} className="mb-2">
              <div className="flex items-center justify-between px-2 py-2 text-[11px] font-semibold uppercase tracking-[.15em] text-stone-500"><span>{label}</span><span>{items.length}</span></div>
              <div className="space-y-1">
                {items.map((item) => <TaskRow key={item.task.id} item={item} />)}
                {items.length === 0 && <div className="mx-2 h-px bg-stone-800/70" />}
              </div>
            </section>
          })}
        </aside>
        <main className="min-w-0"><AuthTokenContext.Provider value={token}><Outlet /></AuthTokenContext.Provider></main>
      </div>
    </div>
  )
}

function TaskRow({ item }: { item: ActivitySummary }) {
  const lastAt = item.last_event_at ?? item.task.created_at
  return <Link to="/tasks/$taskId" params={{ taskId: item.task.id }} activeProps={{ className: 'border-stone-600 bg-stone-800/80' }} className="block rounded-md border border-transparent px-3 py-3 hover:bg-stone-800/50">
    <div className="mb-1.5 flex items-center gap-2">
      <span className="font-mono text-xs text-stone-500">{item.task.id}</span>
      <span className="rounded border border-stone-700 px-1.5 py-0.5 text-[10px] font-bold text-stone-400">{item.task.level || 'L2'}</span>
      {item.needs_attention && <span className="ml-auto rounded bg-amber-300 px-1.5 py-0.5 text-[10px] font-bold text-stone-950">Needs attention</span>}
    </div>
    <p className="truncate text-sm font-medium text-stone-100">{item.task.title}</p>
    <div className="mt-2 flex items-center gap-2 text-[11px] text-stone-500"><span>{item.task.repo}</span><span>·</span><span className="truncate">{item.task.source}</span><span className="ml-auto shrink-0">{relativeTime(lastAt)}</span></div>
  </Link>
}

function EmptyDetail() {
  return <div className="grid min-h-[calc(100vh-4rem)] place-items-center p-10"><div className="max-w-md text-center"><p className="mb-2 text-2xl font-semibold">Factory activity at a glance</p><p className="text-sm leading-6 text-stone-500">Choose a task to inspect its costed timeline, audit events, and review actions.</p></div></div>
}

function TaskDetail() {
	const { taskId } = useParams({ from: '/tasks/$taskId' })
	const { data: item, isLoading } = useQuery({ queryKey: ['task-activity', taskId], queryFn: () => fetchTaskActivity(taskId) })
  const token = useContext(AuthTokenContext)
  const queryClient = useQueryClient()
	useEffect(() => {
		const stream = new EventSource(`/v1/tasks/${encodeURIComponent(taskId)}/events/stream`)
		let refresh: number | undefined
		stream.addEventListener('activity', () => {
			window.clearTimeout(refresh)
			refresh = window.setTimeout(() => {
				void queryClient.invalidateQueries({ queryKey: ['task-activity', taskId] })
				void queryClient.invalidateQueries({ queryKey: ['activity'] })
			}, 250)
		})
		return () => {
			window.clearTimeout(refresh)
			stream.close()
		}
	}, [queryClient, taskId])

	if (isLoading) return <div className="p-10 text-stone-500">Loading task…</div>
	if (!item) return <div className="p-10 text-stone-500">Task not found.</div>
  const totalCost = item.jobs.reduce((sum, job) => sum + job.cost_usd, 0)
  const totalBudget = item.jobs.reduce((sum, job) => sum + job.budget_usd, 0)
  return <div className="mx-auto max-w-5xl p-5 md:p-8">
    <div className="mb-8 flex flex-col gap-5 border-b border-stone-800 pb-7 md:flex-row md:items-start md:justify-between">
      <div><div className="mb-3 flex flex-wrap items-center gap-2 text-xs text-stone-500"><span className="font-mono">{item.task.id}</span><span className="rounded border border-stone-700 px-2 py-0.5">{item.task.repo}</span><span>{item.task.source}</span></div><h1 className="max-w-3xl text-2xl font-semibold tracking-tight md:text-3xl">{item.task.title}</h1>{item.task.body && <p className="mt-3 max-w-3xl text-sm leading-6 text-stone-400">{item.task.body}</p>}</div>
      <div className="min-w-48 rounded-lg border border-stone-800 bg-stone-900/50 p-4"><p className="text-[11px] uppercase tracking-wider text-stone-500">Budget consumed</p><p className="mt-1 text-xl font-semibold">${totalCost.toFixed(2)} <span className="text-sm font-normal text-stone-600">/ ${totalBudget.toFixed(2)}</span></p><div className="mt-3 h-1.5 overflow-hidden rounded bg-stone-800"><div className="h-full bg-lime-300" style={{ width: `${Math.min(100, totalBudget ? totalCost / totalBudget * 100 : 0)}%` }} /></div></div>
    </div>
    {item.needs_attention && <ReviewPanel item={item} token={token} />}
    <section className="mt-8"><div className="mb-5 flex items-center justify-between"><h2 className="text-sm font-semibold uppercase tracking-[.14em] text-stone-400">Costed timeline</h2><span className="text-xs text-stone-600">{item.events.length} audit events</span></div><div className="relative space-y-5 before:absolute before:bottom-5 before:left-[7px] before:top-5 before:w-px before:bg-stone-800">{item.jobs.map((job) => <TimelineJob key={job.id} job={job} item={item} />)}{item.jobs.length === 0 && <p className="pl-7 text-sm text-stone-500">Waiting for the first job to start.</p>}</div></section>
    {item.interventions.length > 0 && <section className="mt-10"><h2 className="mb-4 text-sm font-semibold uppercase tracking-[.14em] text-stone-400">Bounce history</h2><div className="space-y-2">{item.interventions.map((entry) => <div key={entry.id} className="rounded-md border border-stone-800 p-3 text-sm"><strong>{entry.action}</strong><span className="mx-2 text-stone-700">·</span><span className="text-stone-400">{entry.reason_code}</span>{entry.comment && <p className="mt-2 text-stone-300">{entry.comment}</p>}</div>)}</div></section>}
  </div>
}

function TimelineJob({ job, item }: { job: Job; item: ActivityItem }) {
  const summaryEvent = [...item.events].reverse().find((event) => event.job_id === job.id && event.kind === 'job.summary')
  const summary = typeof summaryEvent?.payload?.summary === 'string' ? summaryEvent.payload.summary : `Job ${job.state.replaceAll('_', ' ')}.`
  return <article className="relative pl-7"><span className={cn('absolute left-0 top-5 size-[15px] rounded-full border-4 border-[#10120f] bg-stone-600', job.state === 'done' && 'bg-lime-300', job.state.includes('failed') && 'bg-red-500')} /><div className="rounded-lg border border-stone-800 bg-stone-900/40 p-4"><div className="flex flex-wrap items-start justify-between gap-3"><div><p className="text-xs font-semibold uppercase tracking-wider text-stone-500">{job.stage}</p><p className="mt-2 max-w-3xl text-sm leading-6 text-stone-300">{summary}</p></div><span className="rounded border border-stone-700 px-2 py-1 text-xs text-stone-400">{job.state}</span></div><div className="mt-4 flex flex-wrap gap-x-4 gap-y-1 border-t border-stone-800 pt-3 font-mono text-[11px] text-stone-500"><span>{duration(job.started_at, job.ended_at)}</span><span>${job.cost_usd.toFixed(2)}</span><span>{job.tokens_in.toLocaleString()} in / {job.tokens_out.toLocaleString()} out</span><span>{job.harness}{job.model_tier ? ` / ${job.model_tier}` : ''}{job.auth_mode ? ` / ${job.auth_mode}` : ''}</span><span>{job.confinement}</span></div></div></article>
}

function ReviewPanel({ item, token }: { item: ActivityItem; token: string }) {
  const queryClient = useQueryClient()
  const [action, setAction] = useState<InterventionAction>('approve')
  const [reasonCode, setReasonCode] = useState('approved')
  const [comment, setComment] = useState('')
	const mutation = useMutation({
		mutationFn: () => reviewTask(item.task.id, token, action, reasonCode, comment),
		onSuccess: () => {
			void queryClient.invalidateQueries({ queryKey: ['task-activity', item.task.id] })
			void queryClient.invalidateQueries({ queryKey: ['activity'] })
		},
  })
  return <section className="rounded-lg border border-amber-500/40 bg-amber-300/[.04] p-4 md:p-5"><div className="mb-4 flex items-center justify-between"><div><p className="text-xs font-bold uppercase tracking-[.15em] text-amber-300">Human gate</p><p className="mt-1 text-sm text-stone-400">Record a structured decision without leaving the task context.</p></div></div><div className="mb-4 flex flex-wrap gap-2">{interventionActions.map(([value,label]) => <Button key={value} variant={action === value ? 'default' : 'outline'} onClick={() => { setAction(value); if (value === 'approve') setReasonCode('approved') }}>{label}</Button>)}</div><div className="grid gap-3 md:grid-cols-[220px_1fr]"><select value={reasonCode} onChange={(event) => setReasonCode(event.target.value)} className="rounded-md border border-stone-700 bg-stone-900 px-3 py-2 text-sm outline-none focus:border-stone-500">{reasonCodes.map((code) => <option key={code}>{code}</option>)}</select><textarea value={comment} onChange={(event) => setComment(event.target.value)} placeholder={action === 'redirect' ? 'What should the agent change?' : 'Optional review note'} className="min-h-20 rounded-md border border-stone-700 bg-stone-900 px-3 py-2 text-sm outline-none focus:border-stone-500" /></div><div className="mt-3 flex flex-wrap items-center justify-between gap-3"><code className="text-xs text-stone-500">{item.checkout_command}</code><Button disabled={!token || !reasonCode || mutation.isPending} onClick={() => mutation.mutate()}>{mutation.isPending ? 'Recording…' : `Confirm ${action.replaceAll('_', ' ')}`}</Button></div>{!token && <p className="mt-2 text-xs text-amber-300/80">Enter the API review token in the header to act.</p>}{mutation.error && <p className="mt-2 text-sm text-red-300">{String(mutation.error)}</p>}</section>
}

const rootRoute = createRootRoute({ component: Layout })
const indexRoute = createRoute({ getParentRoute: () => rootRoute, path: '/', component: EmptyDetail })
const taskRoute = createRoute({ getParentRoute: () => rootRoute, path: '/tasks/$taskId', component: TaskDetail })
const routeTree = rootRoute.addChildren([indexRoute, taskRoute])
export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' { interface Register { router: typeof router } }
