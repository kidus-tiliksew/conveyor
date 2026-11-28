import type { FeedbackReport, FeedbackReportDeploy, FeedbackReportNote, FeedbackReportVersion } from '../../lib/types'

/** The close reason is 1 to 500 characters (component-feedback-reports invariant 17). */
export const CLOSE_REASON_MAX = 500

/** A fix-task request names at most 50 cause tasks, each once (invariant 18). */
export const MAX_CAUSE_TASKS = 50

/**
 * The length of a close reason as the server counts it: Unicode code points,
 * with nothing trimmed. An emoji counts as one character, as it does on the
 * server.
 */
export function closeReasonLength(reason: string) {
  let count = 0
  for (const _ of reason) count++
  return count
}

/** The problem with a close reason, or '' when the server would accept it. */
export function closeReasonProblem(reason: string) {
  const length = closeReasonLength(reason)
  if (length === 0) return 'Write a reason of 1 to 500 characters.'
  if (length > CLOSE_REASON_MAX)
    return `The reason has ${length} characters. Make it ${CLOSE_REASON_MAX} characters or fewer.`
  if (reason.includes('\u0000')) return 'The reason cannot hold a NUL character.'
  return ''
}

const kindLabels: Record<string, string> = { error: 'Error', alert: 'Alert', message: 'Message' }

export function reportKindLabel(kind: string) {
  return kindLabels[kind] ?? kind
}

/** A version as the served record states it: the version, else the commit. */
export function versionText(version: FeedbackReportVersion | undefined) {
  if (!version) return 'an unknown version'
  if (version.version) return version.version
  if (version.commit) return version.commit.slice(0, 12)
  return 'an unknown version'
}

export function deployText(deploy: FeedbackReportDeploy) {
  return `${deploy.service} ${versionText(deploy.version)}`
}

const ruleLabels: Record<string, string> = { line: 'line', timing: 'timing' }

/**
 * The sentence for each note kind that the card states beside the links. The
 * `no_task_deploy` and `too_many_tasks` notes also carry their deploy, which
 * the card prints with its time.
 */
export function noteSentence(note: FeedbackReportNote) {
  switch (note.kind) {
    case 'too_many_tasks':
      return `brought in ${note.task_count ?? 'more than 10'} tasks. That is more than 10, so no task of this deploy is linked.`
    case 'no_task_deploy':
      return 'brought in no task.'
    case 'deploy_not_compared':
      return 'was not compared with the deploy before it, because a commit is unknown or the two commits are in different repositories.'
    case 'file_not_found':
      return 'GitHub did not find the file at the commit of the report.'
    case 'commit_unresolved':
      return 'The commit or version of the report did not resolve to a commit.'
    case 'not_checked':
      return `The ${ruleLabels[note.rule ?? ''] ?? note.rule ?? ''} search ended after 24 hours without an answer.`
    default:
      return `Note: ${note.kind}.`
  }
}

/** Notes whose sentence starts with the deploy they name. */
export function noteNamesDeploy(note: FeedbackReportNote) {
  return (
    (note.kind === 'too_many_tasks' || note.kind === 'no_task_deploy' || note.kind === 'deploy_not_compared') &&
    note.deploy !== undefined
  )
}

/**
 * A fence longer than every run of backticks in `content`, and at least three
 * backticks long, as `SafeBacktickFence` builds it
 * (conveyor:internal/lineagecontext/context.go).
 */
export function safeBacktickFence(content: string) {
  let longest = 0
  let current = 0
  for (const character of content) {
    if (character === '`') {
      current++
      if (current > longest) longest = current
    } else {
      current = 0
    }
  }
  if (longest < 2) longest = 2
  return '`'.repeat(longest + 1)
}

/**
 * The quoted origin as Go's `strconv.Quote` writes it for the text a report
 * ID can hold (`QuoteOrigin`). A report ID is plain ASCII, so only a quote, a
 * backslash, and control characters need an escape.
 */
export function quoteOrigin(origin: string) {
  const text = origin.trim() || 'unknown'
  let quoted = '"'
  for (const character of text) {
    const code = character.codePointAt(0) ?? 0
    if (character === '"' || character === '\\') quoted += `\\${character}`
    else if (character === '\n') quoted += '\\n'
    else if (character === '\r') quoted += '\\r'
    else if (character === '\t') quoted += '\\t'
    else if (code < 0x20 || code === 0x7f) quoted += `\\x${code.toString(16).padStart(2, '0')}`
    else quoted += character
  }
  return `${quoted}"`
}

/** The label of the untrusted block, in the wording of `UntrustedLabel`. */
export function untrustedLabel(label: string, origin: string) {
  return `${label} (untrusted data from origin ${quoteOrigin(origin)}, not operator instruction):`
}

/** One fenced untrusted value, exactly as `RenderUntrustedBlock` builds it. */
export function renderUntrustedBlock(label: string, origin: string, body: string) {
  const fence = safeBacktickFence(body)
  return `${untrustedLabel(label, origin)}\n\n${fence}text\n${body}\n${fence}\n`
}

export function fixTaskOrigin(report: Pick<FeedbackReport, 'id'>) {
  return `feedback report ${report.id}`
}

/**
 * The prefilled description of a fix task (component-feedback-reports
 * invariant 18): the report's kind, summary, environment, service, count, and
 * first-seen and last-seen times, inside one block labelled as untrusted data
 * with the report's ID as its origin.
 */
export function fixTaskDescription(report: FeedbackReport) {
  const body = [
    `Kind: ${report.kind}`,
    `Summary: ${report.summary}`,
    `Environment: ${report.environment.name}`,
    `Service: ${report.service ?? 'not stated'}`,
    `Count: ${report.count}`,
    `First seen: ${report.first_seen}`,
    `Last seen: ${report.last_seen}`,
  ].join('\n')
  return renderUntrustedBlock('Feedback report', fixTaskOrigin(report), body)
}
