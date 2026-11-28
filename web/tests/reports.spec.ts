// Feedback reports on the dashboard (component-feedback-reports v4
// invariants 17, 18, and 21 to 28; req-production-feedback REQ-6, REQ-7,
// AC-2.2, AC-5.3 to AC-5.5, AC-6.1 to AC-6.3, AC-7.1, AC-7.2, AC-7.4, AC-7.5;
// req-security-boundaries AC-4.1). A fake server answers in the shapes
// internal/httpapi writes (reports-fixtures.ts). The page prints what the
// served report holds and derives no state of its own.
import { expect, type Locator, type Page, test } from '@playwright/test'
import type { FeedbackReport } from '../src/lib/types'
import {
  type Act,
  crate,
  crateLine,
  crateReads,
  deploy,
  gate,
  lineEvidence,
  listReads,
  mockReportServer,
  now,
  priorSignalEvidence,
  qa,
  report,
  reportReads,
  type Server,
  staging,
  taskLink,
  timingEvidence,
} from './reports-fixtures'

const card = (page: Page, id: string) => page.locator(`[data-report-card="${id}"]`)

// One report for each row of the card grid (class 1).
const lineLinked = report('rpt-261127-a00001', {
  summary: 'Line link report',
  file: 'internal/checkout/pay.go',
  line: 214,
  last_seen: '2026-11-27T11:59:50Z',
  links: [taskLink('lnk-line', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] })],
})
const timingLinked = report('rpt-261127-a00002', {
  summary: 'Timing link report',
  last_seen: '2026-11-27T11:59:40Z',
  environment: staging,
  links: [
    taskLink(
      'lnk-timing',
      '261120-b02',
      'Store invoice numbers',
      {
        state: 'confirmed',
        confirmed_by: 'usr_operator',
        confirmed_at: '2026-11-27T11:59:00Z',
        evidence: [timingEvidence],
      },
      { id: 'crate-billing', name: 'Billing v2' },
    ),
  ],
})
const bothEvidence = report('rpt-261127-a00003', {
  summary: 'Both kinds of evidence',
  last_seen: '2026-11-27T11:59:30Z',
  environment: qa,
  links: [
    taskLink('lnk-both', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence, priorSignalEvidence] }),
    {
      id: 'lnk-crate',
      state: 'confirmed',
      crate: { id: 'crate-search', name: 'Search' },
      evidence: [],
      confirmed_by: 'usr_operator',
      confirmed_at: '2026-11-27T11:00:00Z',
    },
  ],
})
const noLink = report('rpt-261127-a00004', {
  summary: 'No link report',
  kind: 'alert',
  service: null,
  count: 1,
  last_seen: '2026-11-27T11:59:20Z',
  notes: [
    { kind: 'no_task_deploy', deploy },
    { kind: 'no_task_deploy', deploy: { ...deploy, version: { repository: 'app', commit: null, version: 'v1.4.4' } } },
  ],
})
const tooMany = report('rpt-261127-a00005', {
  summary: 'Too many tasks report',
  kind: 'message',
  last_seen: '2026-11-27T11:59:10Z',
  notes: [
    { kind: 'too_many_tasks', deploy, task_count: 14 },
    { kind: 'deploy_not_compared', deploy },
    { kind: 'file_not_found', file: 'cmd/missing.go', commit: '3f9c2ab' },
    { kind: 'commit_unresolved', reason: 'unknown_version' },
    { kind: 'not_checked', rule: 'line', reason: 'forge_unavailable' },
  ],
})
const pending = report('rpt-261127-a00006', {
  summary: 'Pending links report',
  last_seen: '2026-11-27T11:59:00Z',
  links_pending: true,
  fix_task: { id: '261127-fix', title: 'Fix the checkout timeout' },
  reopened_at: '2026-11-27T11:00:00Z',
})
const closed = report('rpt-261127-a00007', {
  summary: 'Closed report',
  state: 'closed',
  closed: { at: '2026-11-27T11:30:00Z', by: 'usr_operator', reason: 'Fixed by the retry change.' },
  links: [taskLink('lnk-closed', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] })],
  in_inbox: false,
})

const grid = [lineLinked, timingLinked, bothEvidence, noLink, tooMany, pending, closed]

test.describe('the report card', () => {
  test('prints every state of a report as served', async ({ page }) => {
    await page.clock.setFixedTime(now)
    await mockReportServer(page, { reports: grid })
    await page.goto('/w/demo/monitor')

    // Line evidence, a possible link, the file and line, a listening watcher.
    const line = card(page, lineLinked.id)
    await expect(line).toBeVisible()
    await expect(line.locator('[data-report-summary]')).toHaveText('Line link report')
    await expect(line).toContainText('Error')
    await expect(line.locator('[data-report-count]')).toHaveText('Seen 12 times')
    await expect(line.locator('[data-report-environment]')).toHaveText('production')
    await expect(line.locator('[data-report-service]')).toHaveText('api')
    await expect(line.locator('[data-report-first-seen] time')).toHaveAttribute('datetime', '2026-11-27T11:58:00Z')
    await expect(line.locator('[data-report-last-seen] time')).toHaveAttribute('datetime', '2026-11-27T11:59:50Z')
    await expect(line.locator('[data-report-key]')).toHaveText('checkout.timeout.rpt-261127-a00001')
    await expect(line.locator('[data-report-file]')).toHaveText('internal/checkout/pay.go, line 214')
    await expect(line.locator('[data-watcher-state]')).toHaveCount(0)
    const lineLink = line.locator('[data-report-link="lnk-line"]')
    await expect(lineLink).toHaveAttribute('data-link-state', 'possible')
    await expect(lineLink).toContainText('Possible link')
    await expect(lineLink.locator('[data-report-evidence="line"]')).toHaveText(
      'Line evidence: internal/checkout/pay.go, line 214, last changed in commit 3f9c2ab1d4e5.',
    )
    await expect(line).toContainText(
      'A possible link is evidence only. It does not say that the task caused the problem.',
    )
    await expect(line.locator('[data-report-links-pending]')).toHaveCount(0)
    await expect(line.locator('[data-report-fix-task]')).toHaveCount(0)

    // Timing evidence on a confirmed link with its crate, and no watcher with
    // a last signal time.
    const timing = card(page, timingLinked.id)
    const timingLink = timing.locator('[data-report-link="lnk-timing"]')
    await expect(timingLink).toHaveAttribute('data-link-state', 'confirmed')
    await expect(timingLink).toContainText('Confirmed link')
    await expect(timingLink).toContainText('Task Store invoice numbers in the crate Billing v2')
    await expect(timingLink.locator('[data-report-evidence="timing"]')).toContainText(
      'Timing evidence: a deploy of api v1.4.2',
    )
    await expect(timingLink.locator('[data-report-evidence="timing"]')).toContainText(
      '18 minutes before the watcher first saw the problem.',
    )
    await expect(timing.locator('[data-watcher-state="not-listening"]')).toContainText(
      'No watcher is listening. Last signal',
    )
    await expect(timing.locator('[data-watcher-state] time')).toHaveAttribute('datetime', '2026-11-26T18:00:00Z')

    // Both kinds of evidence on one link, a crate link, and no watcher with no
    // signal yet.
    const both = card(page, bothEvidence.id)
    const bothLink = both.locator('[data-report-link="lnk-both"]')
    await expect(bothLink.locator('[data-report-evidence="line"]')).toHaveCount(1)
    await expect(bothLink.locator('[data-report-evidence="timing"]')).toContainText(
      'The watcher first saw the problem after the signal of',
    )
    await expect(bothLink.locator('[data-report-evidence="timing"] time').last()).toHaveAttribute(
      'datetime',
      '2026-11-27T11:30:00Z',
    )
    await expect(both.locator('[data-report-link="lnk-crate"]')).toContainText('Crate Search')
    await expect(both.locator('[data-watcher-state="not-listening"]')).toContainText(
      'No watcher is listening. No signal has arrived.',
    )

    // No served link: no task explains it, with each no-task deploy named.
    const none = card(page, noLink.id)
    await expect(none).toContainText('Alert')
    await expect(none.locator('[data-report-count]')).toHaveText('Seen 1 time')
    await expect(none.locator('[data-report-service]')).toHaveText('Not stated')
    await expect(none.locator('[data-report-no-link]')).toContainText('No task explains this report.')
    const deploys = none.locator('[data-report-note="no_task_deploy"]')
    await expect(deploys).toHaveCount(2)
    await expect(deploys.nth(0)).toContainText('The deploy of api v1.4.3')
    await expect(deploys.nth(0)).toContainText('brought in no task.')
    await expect(deploys.nth(0).locator('time')).toHaveAttribute('datetime', '2026-11-27T10:00:00Z')
    await expect(deploys.nth(1)).toContainText('The deploy of api v1.4.4')
    await expect(none.locator('[data-report-link]')).toHaveCount(0)
    await expect(none).not.toContainText('A possible link is evidence only.')

    // Each other note kind, the too-many-tasks note with its deploy and number.
    const many = card(page, tooMany.id)
    await expect(many).toContainText('Message')
    await expect(many.locator('[data-report-note="too_many_tasks"]')).toContainText('The deploy of api v1.4.3')
    await expect(many.locator('[data-report-note="too_many_tasks"]')).toContainText(
      'brought in 14 tasks. That is more than 10, so Conveyor links no task of this deploy.',
    )
    await expect(many.locator('[data-report-note="deploy_not_compared"]')).toContainText(
      'Conveyor did not compare the two.',
    )
    await expect(many.locator('[data-report-note="file_not_found"]')).toHaveText(
      'GitHub did not find the file at the commit of the report. File: cmd/missing.go.',
    )
    await expect(many.locator('[data-report-note="commit_unresolved"]')).toHaveText(
      'The commit or version of the report did not resolve to a commit. Reason: unknown_version.',
    )
    await expect(many.locator('[data-report-note="not_checked"]')).toHaveText(
      'The line search ended after 24 hours without an answer. Reason: forge_unavailable.',
    )

    // Links still being looked for, a fix task, and a reopening.
    const looking = card(page, pending.id)
    await expect(looking.locator('[data-report-links-pending]')).toHaveText(
      'Conveyor still looks for tasks to link to this report.',
    )
    await expect(looking.locator('[data-report-no-link]')).toContainText('No task explains this report.')
    await expect(looking.locator('[data-report-fix-task]')).toHaveText('Fix task: Fix the checkout timeout')
    await expect(looking.locator('[data-report-fix-task] a')).toHaveAttribute('href', '/w/demo/tasks/261127-fix')
    await expect(looking.locator('[data-report-reopened]')).toContainText('Reopened')

    // No text names a task as the cause.
    for (const item of [line, timing, both, none, many, looking]) {
      await expect(item).not.toContainText(/caused by|is the cause|root cause/i)
    }

    // The open list holds no closed report; the switch lists it.
    await expect(card(page, closed.id)).toHaveCount(0)
    await page.getByRole('switch', { name: 'Show closed reports' }).click()
    const shut = card(page, closed.id)
    await expect(shut).toHaveAttribute('data-report-state', 'closed')
    await expect(shut).toContainText('Closed')
    await expect(shut.locator('[data-report-closed]')).toContainText('Reason: Fixed by the retry change.')
    await expect(shut.locator('[data-report-link="lnk-closed"]')).toBeVisible()
  })

  test('offers each act to a person who may create tasks, and only reopening on a closed report', async ({ page }) => {
    await mockReportServer(page, { reports: [lineLinked, timingLinked, closed] })
    await page.goto('/w/demo/monitor')
    const line = card(page, lineLinked.id)
    await expect(line.getByRole('button', { name: 'Confirm the link to Send invoices by email' })).toBeVisible()
    await expect(line.getByRole('button', { name: 'Remove the link to Send invoices by email…' })).toBeVisible()
    await expect(line.getByRole('button', { name: 'Link a task or crate' })).toBeVisible()
    await expect(line.getByRole('button', { name: 'Close report' })).toBeVisible()
    await expect(line.getByRole('link', { name: 'Start a fix task' })).toHaveAttribute(
      'href',
      `/w/demo/?create=true&report=${lineLinked.id}`,
    )
    await expect(line.getByRole('button', { name: 'Reopen report' })).toHaveCount(0)
    // A confirmed link offers no confirm.
    const timing = card(page, timingLinked.id)
    await expect(timing.getByRole('button', { name: /^Confirm the link/ })).toHaveCount(0)
    await expect(timing.getByRole('button', { name: 'Remove the link to Store invoice numbers…' })).toBeVisible()

    await page.getByRole('switch', { name: 'Show closed reports' }).click()
    const shut = card(page, closed.id)
    await expect(shut.getByRole('button', { name: 'Reopen report' })).toBeVisible()
    await expect(shut.getByRole('button')).toHaveCount(1)
    await expect(shut.getByRole('link', { name: 'Start a fix task' })).toHaveCount(0)
  })

  for (const role of ['viewer', 'contributor'] as const) {
    test(`a ${role} without create_tasks sees the report and no act control`, async ({ page }) => {
      await mockReportServer(page, { reports: [lineLinked, pending, closed], role })
      await page.goto('/w/demo/monitor')
      await expect(card(page, lineLinked.id)).toBeVisible()
      for (const id of [lineLinked.id, pending.id]) {
        await expect(card(page, id).getByRole('button')).toHaveCount(0)
        await expect(card(page, id).getByRole('link', { name: 'Start a fix task' })).toHaveCount(0)
      }
      await page.getByRole('switch', { name: 'Show closed reports' }).click()
      await expect(card(page, closed.id)).toBeVisible()
      await expect(card(page, closed.id).getByRole('button')).toHaveCount(0)
    })
  }
})

// Hostile values (class 3): Markdown, HTML, long backtick runs, very long
// words, line breaks, and right-to-left marks.
const longWord = 'x'.repeat(300)
const hostile = (label: string) =>
  `**${label}** <img src=x onerror="window.__xss=1"> [link](javascript:alert(1)) \`\`\`\`\`\` fence\nsecond line ${longWord} ‮evil‬`
const hostileReport = report('rpt-261127-b00001', {
  summary: hostile('summary'),
  key: `key <b>bold</b> ‮${'k'.repeat(150)}`,
  service: `svc <script>window.__xss=2</script> ${'s'.repeat(60)}`,
  file: `src/<i>x</i>/${'d/'.repeat(240)}file.go`,
  line: 7,
  environment: { ...staging, name: `env <u>u</u> ‮${'e'.repeat(80)}` },
  links: [
    taskLink(
      'lnk-hostile',
      '261120-a01',
      `title <em>em</em> \`\`\`\` ${'t'.repeat(120)}`,
      { evidence: [{ ...lineEvidence, file: `ev/<b>b</b>/${'f'.repeat(200)}.go` }] },
      { id: 'crate-billing', name: `crate **bold** ‮${'c'.repeat(90)}` },
    ),
  ],
  fix_task: { id: '261127-fix', title: `fix <img src=y> ${'f'.repeat(100)}` },
})
const hostileClosed = report('rpt-261127-b00002', {
  summary: 'Closed hostile report',
  state: 'closed',
  closed: { at: '2026-11-27T11:30:00Z', by: 'usr_operator', reason: hostile('reason') },
})

async function expectNoMarkup(locator: Locator) {
  await expect(locator.locator('img, script, strong, em, b, i, u, code, pre')).toHaveCount(0)
}

async function expectNoOverflow(page: Page) {
  const overflow = await page.evaluate(() => {
    const root = document.scrollingElement as HTMLElement
    const wide = [...document.querySelectorAll<HTMLElement>('[data-report-card], [data-fix-task]')].filter(
      (element) => element.scrollWidth > element.clientWidth + 1,
    )
    return {
      page: root.scrollWidth > window.innerWidth + 1,
      cards: wide.map((element) => element.outerHTML.slice(0, 80)),
    }
  })
  expect(overflow).toEqual({ page: false, cards: [] })
}

test.describe('untrusted text', () => {
  for (const width of [375, 1280]) {
    test(`every report text prints as plain text at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 })
      await mockReportServer(page, { reports: [hostileReport, hostileClosed] })
      await page.goto('/w/demo/monitor')
      const item = card(page, hostileReport.id)
      await expect(item.locator('[data-report-summary]')).toHaveText(hostileReport.summary)
      await expect(item.locator('[data-report-key]')).toHaveText(hostileReport.key)
      await expect(item.locator('[data-report-service]')).toHaveText(hostileReport.service ?? '')
      await expect(item.locator('[data-report-file]')).toHaveText(`${hostileReport.file}, line 7`)
      await expect(item.locator('[data-report-environment]')).toHaveText(hostileReport.environment.name)
      const link = item.locator('[data-report-link="lnk-hostile"]')
      await expect(link).toContainText(hostileReport.links[0].task?.title ?? '')
      await expect(link).toContainText(hostileReport.links[0].task?.crate?.name ?? '')
      await expect(item.locator('[data-report-fix-task]')).toContainText(hostileReport.fix_task?.title ?? '')
      // Every value sits in its own bidi isolate, so a right-to-left mark
      // cannot reorder the words around it.
      await expect(item.locator('[data-report-summary] bdi')).toHaveCount(1)
      await expectNoMarkup(item)
      expect(await page.evaluate(() => (window as unknown as { __xss?: number }).__xss)).toBeUndefined()
      await expectNoOverflow(page)

      await page.getByRole('switch', { name: 'Show closed reports' }).click()
      const shut = card(page, hostileClosed.id)
      await expect(shut.locator('[data-report-closed] bdi')).toHaveText(hostileClosed.closed?.reason ?? '')
      await expectNoMarkup(shut)
      await expectNoOverflow(page)
    })
  }
})

// Every act against every answer (class 2). Each act walks the same answers in
// one page: 503 retryable, a refusal, a 500 with a slow fresh read, no answer,
// and a slow 200 that a second press cannot send twice. Typed input stays in
// place after each failure, and after a 500 or no answer the page reads the
// report again and never sends the act again by itself.
interface ActCase {
  name: string
  reports: FeedbackReport[]
  closedList?: boolean
  // Opens the form and types, when the act has one.
  prepare?: (page: Page) => Promise<void>
  // The control that sends the act.
  button: (page: Page) => Locator
  // A typed control whose value must survive each failure.
  typed?: (page: Page) => Locator
  typedValue?: string
  expected: Omit<Act, 'body'> & { body: unknown }
  // The served change after success.
  done: (page: Page) => Promise<void>
}

const actReport = report('rpt-261127-c00001', {
  summary: 'Act target',
  links: [taskLink('lnk-line', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] })],
})
const closedActReport = report('rpt-261127-c00002', {
  summary: 'Closed act target',
  state: 'closed',
  closed: { at: '2026-11-27T11:30:00Z', by: 'usr_operator', reason: 'Done.' },
})
const actPath = `/v1/feedback-reports/${actReport.id}`

const actCases: ActCase[] = [
  {
    name: 'confirm a possible link',
    reports: [actReport],
    button: (page) =>
      card(page, actReport.id).getByRole('button', { name: 'Confirm the link to Send invoices by email' }),
    expected: { method: 'POST', path: `${actPath}/links`, body: { task_id: '261120-a01' } },
    done: async (page) =>
      expect(card(page, actReport.id).locator('[data-report-link="lnk-line"]')).toHaveAttribute(
        'data-link-state',
        'confirmed',
      ),
  },
  {
    name: 'remove a link',
    reports: [actReport],
    prepare: async (page) => {
      await card(page, actReport.id).getByRole('button', { name: 'Remove the link to Send invoices by email…' }).click()
    },
    button: (page) =>
      card(page, actReport.id).getByRole('button', { name: 'Remove the link to Send invoices by email', exact: true }),
    expected: { method: 'DELETE', path: `${actPath}/links/lnk-line`, body: undefined },
    done: async (page) => expect(card(page, actReport.id).locator('[data-report-link]')).toHaveCount(0),
  },
  {
    name: 'link a merged task',
    reports: [actReport],
    prepare: async (page) => {
      await card(page, actReport.id).getByRole('button', { name: 'Link a task or crate' }).click()
      await card(page, actReport.id).getByRole('textbox', { name: 'Search merged tasks' }).fill('numbers')
    },
    button: (page) => card(page, actReport.id).getByRole('button', { name: 'Link Store invoice numbers' }),
    typed: (page) => card(page, actReport.id).getByRole('textbox', { name: 'Search merged tasks' }),
    typedValue: 'numbers',
    expected: { method: 'POST', path: `${actPath}/links`, body: { task_id: '261120-b02' } },
    done: async (page) =>
      expect(card(page, actReport.id).locator('[data-report-link="lnk-261120-b02"]')).toHaveAttribute(
        'data-link-state',
        'confirmed',
      ),
  },
  {
    name: 'link a crate',
    reports: [actReport],
    prepare: async (page) => {
      await card(page, actReport.id).getByRole('button', { name: 'Link a task or crate' }).click()
      await card(page, actReport.id).getByRole('radio', { name: 'A crate' }).check()
      await card(page, actReport.id).getByRole('textbox', { name: 'Search crates' }).fill('sea')
    },
    button: (page) => card(page, actReport.id).getByRole('button', { name: 'Link Search' }),
    typed: (page) => card(page, actReport.id).getByRole('textbox', { name: 'Search crates' }),
    typedValue: 'sea',
    expected: { method: 'POST', path: `${actPath}/links`, body: { crate_id: 'crate-search' } },
    done: async (page) =>
      expect(card(page, actReport.id).locator('[data-report-link="lnk-crate-search"]')).toBeVisible(),
  },
  {
    name: 'close with a reason',
    reports: [actReport],
    prepare: async (page) => {
      await card(page, actReport.id).getByRole('button', { name: 'Close report' }).click()
      await card(page, actReport.id)
        .getByRole('textbox', { name: 'Reason to close the report' })
        .fill('Fixed by the retry.')
    },
    button: (page) =>
      card(page, actReport.id)
        .getByRole('form', { name: 'Close the report' })
        .getByRole('button', { name: /^(Close report|Closing…)$/ }),
    typed: (page) => card(page, actReport.id).getByRole('textbox', { name: 'Reason to close the report' }),
    typedValue: 'Fixed by the retry.',
    expected: { method: 'POST', path: `${actPath}/close`, body: { reason: 'Fixed by the retry.' } },
    done: async (page) => expect(card(page, actReport.id)).toHaveCount(0),
  },
  {
    name: 'reopen a closed report',
    reports: [closedActReport],
    closedList: true,
    button: (page) => card(page, closedActReport.id).getByRole('button', { name: /^(Reopen report|Reopening…)$/ }),
    expected: { method: 'POST', path: `/v1/feedback-reports/${closedActReport.id}/reopen`, body: undefined },
    done: async (page) => expect(card(page, closedActReport.id)).toHaveCount(0),
  },
]

test.describe('acts and their answers', () => {
  for (const item of actCases) {
    test(`${item.name}: the request and every answer`, async ({ page }) => {
      const server = await mockReportServer(page, { reports: item.reports })
      await page.goto(item.closedList ? '/w/demo/monitor?show=closed' : '/w/demo/monitor')
      await expect(page.locator('[data-report-card]')).toHaveCount(1)
      await item.prepare?.(page)
      const alert = page.locator('[data-report-card] [data-report-act-error]')
      const expectTyped = async () => {
        if (item.typed) await expect(item.typed(page)).toHaveValue(item.typedValue ?? '')
      }

      // 503 retryable: nothing was recorded.
      server.actAnswers.push({
        status: 503,
        json: { error: 'retryable', message: 'Send it again.' },
        headers: { 'Retry-After': '1' },
      })
      await item.button(page).click()
      await expect(alert).toHaveText('Conveyor recorded nothing. Try again.')
      expect(server.acts).toHaveLength(1)
      expect(server.acts[0]).toEqual(item.expected)
      await expectTyped()

      // A refusal records nothing and says why.
      server.actAnswers.push({
        status: 400,
        json: { error: 'invalid_report_act', field: 'body', message: 'must be one JSON object' },
      })
      await item.button(page).click()
      await expect(alert).toHaveText('Conveyor refused the request: the request must be one JSON object.')
      expect(server.acts).toHaveLength(2)
      await expectTyped()

      // A 500: the page reads the report again before the person can act, and
      // never sends the act again by itself.
      const held = gate()
      const before = listReads(server).length
      server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
      server.listGate = held.promise
      await item.button(page).click()
      await expect(page.getByRole('status').filter({ hasText: 'Conveyor reads the report again' })).toBeVisible()
      await expect(item.button(page)).toBeDisabled()
      held.open()
      server.listGate = null
      await expect(alert).toContainText('Conveyor does not know whether it recorded this.')
      expect(listReads(server).length).toBeGreaterThan(before)
      await expect(item.button(page)).toBeEnabled()
      await expectTyped()

      // No answer at all is the same as a 500.
      const beforeAbort = listReads(server).length
      server.actAnswers.push('abort')
      await item.button(page).click()
      await expect(alert).toContainText('Conveyor does not know whether it recorded this.')
      expect(listReads(server).length).toBeGreaterThan(beforeAbort)
      await page.waitForTimeout(300)
      expect(server.acts).toHaveLength(4)
      await expectTyped()

      // A slow answer: a second press sends nothing.
      const slow = gate()
      server.actGate = slow.promise
      const beforeSuccess = listReads(server).length
      await item.button(page).click()
      await expect(item.button(page)).toBeDisabled()
      await item.button(page).click({ force: true })
      expect(server.acts).toHaveLength(5)
      slow.open()
      server.actGate = null
      // 200: the reads refresh and the card prints the served change.
      await item.done(page)
      await expect(alert).toHaveCount(0)
      expect(server.acts).toHaveLength(5)
      expect(server.acts.every((act) => JSON.stringify(act) === JSON.stringify(item.expected))).toBe(true)
      expect(listReads(server).length).toBeGreaterThan(beforeSuccess)
    })
  }

  const refusals: Array<[string, number, string, string]> = [
    ['report_closed', 409, 'The report is closed. Reopen it first.', 'This report is closed now. Reopen it first.'],
    ['report_not_found', 404, 'The report was not found.', 'This report is not in the workspace.'],
    ['link_not_found', 404, 'must name a link of the report', 'This link is not on the report any more.'],
    ['task_not_merged', 409, 'must name a merged task', 'That task is not merged. You can link only a merged task.'],
    ['task_not_found', 404, 'must name a task of the workspace', 'That task is not in this workspace.'],
    ['crate_not_found', 404, 'must name a crate of the workspace', 'That crate is not in this workspace.'],
    ['workspace_not_found', 404, 'not found', 'You cannot record acts on reports in this workspace.'],
  ]
  for (const [code, status, message, shown] of refusals) {
    test(`a ${code} refusal is stated and records nothing`, async ({ page }) => {
      const server = await mockReportServer(page, { reports: [actReport] })
      await page.goto('/w/demo/monitor')
      const before = listReads(server).length
      server.actAnswers.push({ status, json: { error: code, message } })
      await card(page, actReport.id).getByRole('button', { name: 'Confirm the link to Send invoices by email' }).click()
      await expect(card(page, actReport.id).locator('[data-report-act-error]')).toHaveText(shown)
      expect(server.acts).toHaveLength(1)
      // A refusal that says the report or link changed reads the report again.
      if (['report_closed', 'report_not_found', 'link_not_found'].includes(code))
        await expect.poll(() => listReads(server).length).toBeGreaterThan(before)
    })
  }

  test('a body over 16 KiB is told as too large', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    server.actAnswers.push({ status: 413, json: { error: 'request_too_large', message: 'request body too large' } })
    await card(page, actReport.id).getByRole('button', { name: 'Confirm the link to Send invoices by email' }).click()
    await expect(card(page, actReport.id).locator('[data-report-act-error]')).toHaveText(
      'The request is too large. Make it shorter.',
    )
  })

  test('an act on the crate page refreshes the reports and the crate counts', async ({ page }) => {
    const member = report('rpt-261127-c00003', {
      summary: 'Crate member report',
      links: [
        taskLink('lnk-member', '261120-a01', 'Send invoices by email', {}, { id: 'crate-billing', name: 'Billing v2' }),
      ],
    })
    const server = await mockReportServer(page, {
      reports: [member],
      crate: crate([crateLine({ open_report_count: 1 })]),
    })
    await page.goto('/w/demo/crates/crate-billing')
    await expect(card(page, member.id)).toBeVisible()
    const crateBefore = crateReads(server)
    const listBefore = listReads(server).length
    await card(page, member.id).getByRole('button', { name: 'Confirm the link to Send invoices by email' }).click()
    await expect(card(page, member.id).locator('[data-report-link="lnk-member"]')).toHaveAttribute(
      'data-link-state',
      'confirmed',
    )
    await expect.poll(() => crateReads(server)).toBeGreaterThan(crateBefore)
    expect(listReads(server).length).toBeGreaterThan(listBefore)
  })
})

// The fix-task flow, step by step (class 4; invariant 18; AC-7.4;
// req-security-boundaries AC-4.1).
const fixReport = report('rpt-261127-d00001', {
  summary: 'Checkout **timed out** <b>x</b> ```````seven``` and\nsecond line',
  links: [
    taskLink('lnk-a', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] }),
    taskLink('lnk-b', '261120-b02', 'Store invoice numbers', { state: 'confirmed', evidence: [timingEvidence] }),
    { id: 'lnk-c', state: 'confirmed', crate: { id: 'crate-search', name: 'Search' }, evidence: [] },
  ],
})
const fixAddress = `/w/demo/?create=true&report=${fixReport.id}`
const fixPath = `/v1/feedback-reports/${fixReport.id}/fix-task`

// The description in the wording of RenderUntrustedBlock: the longest run of
// backticks in the summary is seven, so the fence is eight.
function expectedDescription(item: FeedbackReport, fence: string) {
  const body = [
    `Kind: ${item.kind}`,
    `Summary: ${item.summary}`,
    `Environment: ${item.environment.name}`,
    `Service: ${item.service ?? 'not stated'}`,
    `Count: ${item.count}`,
    `First seen: ${item.first_seen}`,
    `Last seen: ${item.last_seen}`,
  ].join('\n')
  return `Feedback report (untrusted data from origin "feedback report ${item.id}", not operator instruction):\n\n${fence}text\n${body}\n${fence}\n`
}

const sheet = (page: Page) => page.getByRole('dialog', { name: 'New task' })
const description = (page: Page) => sheet(page).getByRole('textbox', { name: 'Task description' })
const createButton = (page: Page) => sheet(page).getByRole('button', { name: /^(Create task|Creating…|Task created)$/ })
const recordButton = (page: Page) => sheet(page).getByRole('button', { name: 'Record it on the report' })
const fixPuts = (server: Server) => server.acts.filter((act) => act.method === 'PUT')

async function createFixTask(page: Page, causes: string[] = ['Send invoices by email']) {
  await expect(description(page)).not.toHaveValue('')
  await sheet(page).getByRole('combobox', { name: 'Repository' }).selectOption('app')
  for (const title of causes)
    await sheet(page)
      .getByRole('checkbox', { name: new RegExp(title) })
      .check()
  await createButton(page).click()
}

test.describe('starting a fix task', () => {
  test('prefills once, preselects nothing, creates, and records the fix task', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto('/w/demo/monitor')
    await card(page, fixReport.id).getByRole('link', { name: 'Start a fix task' }).click()
    await expect(page).toHaveURL(fixAddress)
    await expect(sheet(page)).toBeVisible()
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText(fixReport.summary)
    await expect(description(page)).toHaveValue(expectedDescription(fixReport, '````````'))
    // Nothing is preselected: no repository, no crate, and no cause.
    await expect(sheet(page).getByRole('combobox', { name: 'Repository' })).toHaveValue('')
    await expect(sheet(page).locator('[data-intake-crate]')).toHaveCount(0)
    const causes = sheet(page).getByRole('group', { name: 'Possible causes' }).getByRole('checkbox')
    await expect(causes).toHaveCount(2)
    await expect(causes.nth(0)).not.toBeChecked()
    await expect(causes.nth(1)).not.toBeChecked()
    await expect(sheet(page)).toContainText('You can tick up to 50 tasks.')
    await expect(createButton(page)).toBeDisabled()

    // A refresh of the report never replaces what the person typed.
    await description(page).press('End')
    await description(page).pressSequentially('My note.')
    const reads = reportReads(server, fixReport.id)
    await server.stream.ready()
    await expect.poll(() => reportReads(server, fixReport.id)).toBeGreaterThan(reads)
    await expect(description(page)).toHaveValue(`${expectedDescription(fixReport, '````````')}My note.`)

    await createFixTask(page)
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
    const created = server.created[0]
    expect(created.repo).toBe('app')
    expect(created.crate_id).toBeUndefined()
    expect(Object.keys(created).sort()).toEqual(['body', 'repo', 'source'])
    expect(created.body).toBe(`${expectedDescription(fixReport, '````````')}My note.`)
    expect(fixPuts(server)).toEqual([
      { method: 'PUT', path: fixPath, body: { task_id: '261127-fix', cause_task_ids: ['261120-a01'] } },
    ])
    // The ticked cause is confirmed with its evidence; the other links stay.
    const stored = server.reports.find((item) => item.id === fixReport.id)
    expect(stored?.links.find((link) => link.id === 'lnk-a')).toMatchObject({
      state: 'confirmed',
      evidence: [lineEvidence],
    })
    expect(stored?.links.find((link) => link.id === 'lnk-b')).toEqual(fixReport.links[1])
    expect(stored?.links.find((link) => link.id === 'lnk-c')).toEqual(fixReport.links[2])
    expect(
      await page.evaluate(() => Object.keys(sessionStorage).filter((key) => key.startsWith('conveyor-fix-task:'))),
    ).toEqual([])
  })

  test('no ticked cause sends the fix task alone', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await createFixTask(page, [])
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(fixPuts(server)).toEqual([{ method: 'PUT', path: fixPath, body: { task_id: '261127-fix' } }])
  })

  test('a 503 record answer is told, and a new attempt sends only the record request', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    server.actAnswers.push({
      status: 503,
      json: { error: 'retryable', message: 'Send it again.' },
      headers: { 'Retry-After': '1' },
    })
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toHaveText('The report recorded nothing. Try again.')
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText(
      'The task Fix the checkout timeout exists.',
    )
    await expect(createButton(page)).toBeDisabled()
    await expect(createButton(page)).toHaveText('Task created')
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
    expect(fixPuts(server)).toHaveLength(2)
    expect(fixPuts(server)[1]).toEqual(fixPuts(server)[0])
  })

  test('a 500 record answer reads the report again and never sends the request again by itself', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    const reads = reportReads(server, fixReport.id)
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toHaveText(
      'The report did not record the task. Record it again.',
    )
    expect(reportReads(server, fixReport.id)).toBeGreaterThan(reads)
    await page.waitForTimeout(300)
    expect(fixPuts(server)).toHaveLength(1)
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
    expect(fixPuts(server)).toHaveLength(2)
  })

  test('a 500 after the report recorded the task opens the task without a second request', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    server.actAnswers.push(() => {
      const index = server.reports.findIndex((item) => item.id === fixReport.id)
      server.reports[index] = {
        ...server.reports[index],
        fix_task: { id: '261127-fix', title: 'Fix the checkout timeout' },
      }
      return { status: 500, json: { error: 'internal error' } }
    })
    await createFixTask(page)
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(fixPuts(server)).toHaveLength(1)
    expect(server.created).toHaveLength(1)
  })

  test('no answer to the record request is handled as a 500', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    server.actAnswers.push('abort')
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toHaveText(
      'The report did not record the task. Record it again.',
    )
    await page.waitForTimeout(300)
    expect(fixPuts(server)).toHaveLength(1)
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
  })

  test('when the fresh read fails too, the page reads again before it offers to send', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    server.readAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toContainText('the report did not load again')
    await expect(recordButton(page)).toHaveCount(0)
    server.readAnswer = null
    await sheet(page).getByRole('button', { name: 'Read the report again' }).click()
    await expect(recordButton(page)).toBeVisible()
    expect(fixPuts(server)).toHaveLength(1)
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
  })

  test('a report closed by someone else meanwhile is told, and the task is recorded after a reopening', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    // Someone else closes the report between the prefill and the record.
    server.reports[0] = {
      ...server.reports[0],
      state: 'closed',
      closed: { at: '2026-11-27T12:00:00Z', by: 'usr_other', reason: 'Gone.' },
    }
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toHaveText(
      'The report is closed now, so it did not record the task. Reopen the report, then record the task again.',
    )
    expect(server.created).toHaveLength(1)
    server.reports[0] = { ...server.reports[0], state: 'open', closed: null }
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
    expect(fixPuts(server)).toHaveLength(2)
  })

  test('a closed sheet or a reloaded page offers to record the same task and never to create another', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    server.actAnswers.push({ status: 503, json: { error: 'retryable' } })
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toBeVisible()
    await sheet(page).getByRole('button', { name: 'Close' }).first().click()
    await expect(sheet(page)).toHaveCount(0)

    // Opening the sheet again from the report.
    await page.goto(fixAddress)
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('The report does not record it yet.')
    await expect(createButton(page)).toBeDisabled()
    // Reloading the page.
    await page.reload()
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('The report does not record it yet.')
    await expect(createButton(page)).toBeDisabled()
    await expect(description(page)).toHaveValue('')
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created).toHaveLength(1)
    expect(fixPuts(server)).toHaveLength(2)
    expect(fixPuts(server)[1].body).toEqual({ task_id: '261127-fix', cause_task_ids: ['261120-a01'] })
  })

  test('a slow record request shows its progress and cannot be sent twice', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    const slow = gate()
    server.actGate = slow.promise
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('Conveyor records it on the report now…')
    await expect(recordButton(page)).toHaveCount(0)
    await expect(createButton(page)).toBeDisabled()
    await createButton(page).click({ force: true })
    expect(fixPuts(server)).toHaveLength(1)
    expect(server.created).toHaveLength(1)
    slow.open()
    server.actGate = null
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
  })

  test('at most 50 causes can be ticked, and the form says why', async ({ page }) => {
    const many = report('rpt-261127-d00002', {
      summary: 'Many links',
      links: Array.from({ length: 51 }, (_, index) =>
        taskLink(
          `lnk-${index}`,
          `261120-t${String(index).padStart(2, '0')}`,
          `Linked task ${String(index).padStart(2, '0')}`,
        ),
      ),
    })
    const server = await mockReportServer(page, { reports: [many] })
    await page.goto(`/w/demo/?create=true&report=${many.id}`)
    const boxes = sheet(page).getByRole('group', { name: 'Possible causes' }).getByRole('checkbox')
    await expect(boxes).toHaveCount(51)
    for (let index = 0; index < 50; index++) await boxes.nth(index).check()
    await expect(boxes.nth(50)).toBeDisabled()
    await expect(sheet(page).getByRole('status').filter({ hasText: 'You ticked 50 tasks.' })).toBeVisible()
    await expect(sheet(page)).toContainText('50 of 50 ticked.')
    await boxes.nth(0).uncheck()
    await expect(boxes.nth(50)).toBeEnabled()
    await boxes.nth(50).check()
    await sheet(page).getByRole('combobox', { name: 'Repository' }).selectOption('conveyor')
    await createButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    const body = fixPuts(server)[0].body as { cause_task_ids: string[] }
    expect(body.cause_task_ids).toHaveLength(50)
    expect(new Set(body.cause_task_ids).size).toBe(50)
    expect(body.cause_task_ids).not.toContain('261120-t00')
  })

  test('a report in the address takes the place of a crate', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(`/w/demo/?create=true&crate=crate-billing&report=${fixReport.id}`)
    await expect(sheet(page).locator('[data-fix-task]')).toBeVisible()
    await expect(sheet(page).locator('[data-intake-crate]')).toHaveCount(0)
    await createFixTask(page, [])
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.created[0].crate_id).toBeUndefined()
  })

  test('a report that does not load stops the flow and offers ordinary intake', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [] })
    await page.goto('/w/demo/?create=true&report=rpt-261127-ffffff')
    await expect(sheet(page).locator('[data-fix-task]')).toContainText(
      'The report did not load, so Conveyor did not fill in the description.',
    )
    await expect(description(page)).toHaveValue('')
    await description(page).fill('Fix it anyway')
    await sheet(page).getByRole('combobox', { name: 'Repository' }).selectOption('conveyor')
    await expect(createButton(page)).toBeDisabled()
    await sheet(page).getByRole('button', { name: 'Create a task without the report' }).click()
    await expect(page).toHaveURL('/w/demo/?create=true')
    await expect(sheet(page).locator('[data-fix-task]')).toHaveCount(0)
    await expect(description(page)).toHaveValue('Fix it anyway')
    await createButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(fixPuts(server)).toHaveLength(0)
  })

  test('a closed report takes no fix task', async ({ page }) => {
    await mockReportServer(page, { reports: [closed] })
    await page.goto(`/w/demo/?create=true&report=${closed.id}`)
    await expect(sheet(page).locator('[data-fix-task]')).toContainText(
      'This report is closed. Reopen it before you start a fix task from it.',
    )
    await expect(description(page)).toHaveValue('')
    await sheet(page).getByRole('combobox', { name: 'Repository' }).selectOption('conveyor')
    await description(page).fill('Text')
    await expect(createButton(page)).toBeDisabled()
  })

  test('ordinary intake keeps its first repository and sends no record request', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto('/w/demo/?create=true')
    await expect(sheet(page).getByRole('combobox', { name: 'Repository' })).toHaveValue('conveyor')
    await expect(sheet(page).locator('[data-fix-task]')).toHaveCount(0)
    await description(page).fill('Ordinary task')
    await createButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.acts).toHaveLength(0)
    expect(reportReads(server, fixReport.id)).toBe(0)
  })
})

// Where reports show (invariant 26; AC-6.1 to AC-6.3).
const taskReport = report('rpt-261127-e00001', {
  summary: 'Task detail report',
  links: [taskLink('lnk-task', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] })],
})

test.describe('the three pages', () => {
  test('task detail lists a merged task’s open reports after its live status, in the sheet and the full page', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [taskReport, { ...closed, links: taskReport.links }] })
    await page.goto('/w/demo/tasks/261120-a01')
    const list = page.getByRole('region', { name: 'Open reports' })
    await expect(list).toBeVisible()
    await expect(list.getByRole('heading', { name: 'Open reports' })).toBeVisible()
    await expect(list.locator('[data-report-card]')).toHaveCount(1)
    await expect(card(page, taskReport.id)).toBeVisible()
    expect(listReads(server).some((read) => read.includes('task_id=261120-a01'))).toBe(true)

    await page.goto('/w/demo/tasks/261120-a01/full')
    await expect(page.getByRole('region', { name: 'Open reports' }).locator('[data-report-card]')).toHaveCount(1)
  })

  test('a merged task with no open report shows no new part, and a task that is not merged reads none', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [] })
    await page.goto('/w/demo/tasks/261120-b02/full')
    await expect(page.getByRole('heading', { name: 'Store invoice numbers', level: 1 })).toBeVisible()
    await expect.poll(() => listReads(server).length).toBe(1)
    await expect(page.getByText('Open reports')).toHaveCount(0)
    await expect(page.locator('[data-report-card], [data-report-list-error]')).toHaveCount(0)

    await page.goto('/w/demo/tasks/running-1/full')
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    await page.waitForTimeout(300)
    expect(listReads(server).filter((read) => read.includes('task_id=running-1'))).toHaveLength(0)
  })

  test('a failed task report read says so and reads again on request', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [taskReport] })
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await page.goto('/w/demo/tasks/261120-a01/full')
    const failure = page.locator('[data-report-list-error]')
    await expect(failure).toContainText('The reports did not load.')
    server.listAnswer = null
    await failure.getByRole('button', { name: 'Try again' }).click()
    await expect(card(page, taskReport.id)).toBeVisible()
  })

  test('the crate page lists its open reports with their member task and shows only counts above 0', async ({
    page,
  }) => {
    const viaMember = report('rpt-261127-e00002', {
      summary: 'Through a member',
      last_seen: '2026-11-27T11:59:59Z',
      links: [
        taskLink('lnk-m1', '261120-a01', 'Send invoices by email', {}, { id: 'crate-billing', name: 'Billing v2' }),
        taskLink('lnk-m2', '261120-b02', 'Store invoice numbers', {}, { id: 'crate-billing', name: 'Billing v2' }),
      ],
    })
    const direct = report('rpt-261127-e00003', {
      summary: 'Linked to the crate',
      links: [{ id: 'lnk-d', state: 'confirmed', crate: { id: 'crate-billing', name: 'Billing v2' }, evidence: [] }],
    })
    const outside = report('rpt-261127-e00004', { summary: 'Another crate' })
    await mockReportServer(page, {
      reports: [viaMember, direct, outside],
      crate: crate([
        crateLine({ open_report_count: 2 }),
        crateLine({ environment: { id: 'env-staging', name: 'staging' }, open_report_count: 0, listening: false }),
        crateLine({ environment: { id: 'env-qa', name: 'qa' } }),
      ]),
    })
    await page.goto('/w/demo/crates/crate-billing')
    await expect(page.getByRole('heading', { name: 'Open reports' })).toBeVisible()
    const list = page.getByRole('region', { name: 'Open reports of this crate' })
    await expect(list.locator('[data-report-card]')).toHaveCount(2)
    await expect(card(page, viaMember.id).locator('[data-report-crate-member]')).toHaveText(
      'In this crate through the member tasks Send invoices by email, Store invoice numbers.',
    )
    await expect(card(page, direct.id).locator('[data-report-crate-member]')).toHaveText(
      'The report links to this crate itself.',
    )
    await expect(card(page, outside.id)).toHaveCount(0)
    const lines = page.locator('[data-crate-live-environment]')
    await expect(lines.nth(0).locator('[data-crate-live-reports]')).toHaveText('2 open reports.')
    await expect(lines.nth(1).locator('[data-crate-live-reports]')).toHaveCount(0)
    await expect(lines.nth(1).locator('[data-watcher-state="not-listening"]')).toBeVisible()
    await expect(lines.nth(2).locator('[data-crate-live-reports]')).toHaveCount(0)
  })

  test('a crate with no open report shows no report part', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [] })
    await page.goto('/w/demo/crates/crate-billing')
    await expect(page.locator('[data-crate-live-status]')).toBeVisible()
    await expect.poll(() => listReads(server).length).toBe(1)
    await expect(page.getByRole('heading', { name: 'Open reports' })).toHaveCount(0)
  })
})

// The address (class 6). Each row: the address the person opens, what the
// filters show, and the address the page keeps.
interface AddressRow {
  query: string
  environment: string
  noCrate: boolean
  closed: boolean
  kept: string
  unknown?: boolean
}
const addressRows: AddressRow[] = [
  { query: '', environment: '', noCrate: false, closed: false, kept: '' },
  {
    query: 'environment=env-staging',
    environment: 'env-staging',
    noCrate: false,
    closed: false,
    kept: 'environment=env-staging',
  },
  { query: 'environment=', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'environment=%20%20', environment: '', noCrate: false, closed: false, kept: '' },
  {
    query: 'environment=env-unknown',
    environment: 'env-unknown',
    noCrate: false,
    closed: false,
    kept: 'environment=env-unknown',
    unknown: true,
  },
  {
    query: 'environment=env-staging&environment=env-production',
    environment: '',
    noCrate: false,
    closed: false,
    kept: '',
  },
  { query: 'environment=123', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'environment=%7B%22a%22%3A1%7D', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'no_crate=true', environment: '', noCrate: true, closed: false, kept: 'no_crate=true' },
  { query: 'no_crate=false', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'no_crate=yes', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'no_crate=', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'no_crate=true&no_crate=true', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'show=closed', environment: '', noCrate: false, closed: true, kept: 'show=closed' },
  { query: 'show=open', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'show=CLOSED', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'show=', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'show=closed&show=closed', environment: '', noCrate: false, closed: false, kept: '' },
  { query: 'unknown=1&show=closed', environment: '', noCrate: false, closed: true, kept: 'show=closed' },
  {
    query: 'show=closed&no_crate=true&environment=env-production',
    environment: 'env-production',
    noCrate: true,
    closed: true,
    // Valid values keep the order the address gave them.
    kept: 'show=closed&no_crate=true&environment=env-production',
  },
]

test.describe('the Monitor address', () => {
  for (const row of addressRows) {
    test(`?${row.query || '(none)'} shows its filters and keeps ?${row.kept || '(none)'}`, async ({ page }) => {
      const server = await mockReportServer(page, { reports: [lineLinked, closed] })
      await page.goto(`/w/demo/monitor${row.query ? `?${row.query}` : ''}`)
      await expect(page.getByRole('combobox', { name: 'Environment' })).toHaveValue(row.environment)
      await expect(page.getByRole('checkbox', { name: 'Only reports in no crate' })).toBeChecked({
        checked: row.noCrate,
      })
      await expect(page.getByRole('switch', { name: 'Show closed reports' })).toHaveAttribute(
        'aria-checked',
        String(row.closed),
      )
      await expect(page).toHaveURL(`/w/demo/monitor${row.kept ? `?${row.kept}` : ''}`)
      await expect(page.getByText('The address names an environment that is not in the list')).toHaveCount(
        row.unknown ? 1 : 0,
      )
      // The list read sends exactly the kept filters.
      await expect.poll(() => listReads(server).length).toBeGreaterThan(0)
      const params = new URLSearchParams(listReads(server).at(-1)?.split('?')[1])
      expect(params.get('environment_id') ?? '').toBe(row.environment)
      expect(params.get('no_crate')).toBe(row.noCrate ? 'true' : null)
      expect(params.get('state')).toBe(row.closed ? 'closed' : null)
    })
  }

  test('choosing filters writes the address, and reload and back keep each step', async ({ page }) => {
    await mockReportServer(page, { reports: [lineLinked, timingLinked, noLink, closed] })
    await page.goto('/w/demo/monitor')
    await expect(page.locator('[data-report-card]')).toHaveCount(3)
    await page.getByRole('combobox', { name: 'Environment' }).selectOption('env-staging')
    await expect(page).toHaveURL('/w/demo/monitor?environment=env-staging')
    await expect(page.locator('[data-report-card]')).toHaveCount(1)
    await page.getByRole('checkbox', { name: 'Only reports in no crate' }).check()
    await expect(page).toHaveURL('/w/demo/monitor?environment=env-staging&no_crate=true')
    // The staging report belongs to a crate through its task.
    await expect(page.getByText('No open report matches these filters.')).toBeVisible()
    await page.reload()
    await expect(page.getByRole('combobox', { name: 'Environment' })).toHaveValue('env-staging')
    await expect(page.getByRole('checkbox', { name: 'Only reports in no crate' })).toBeChecked()
    await page.goBack()
    await expect(page).toHaveURL('/w/demo/monitor?environment=env-staging')
    await expect(page.locator('[data-report-card]')).toHaveCount(1)
    await page.getByRole('combobox', { name: 'Environment' }).selectOption('')
    await page.getByRole('switch', { name: 'Show closed reports' }).click()
    await expect(page).toHaveURL('/w/demo/monitor?show=closed')
    await expect(page.getByRole('heading', { name: 'Closed reports' })).toBeVisible()
    await expect(page.locator('[data-report-card]')).toHaveCount(1)
  })

  const reportRows: Array<[string, boolean]> = [
    ['report=rpt-261127-d00001', true],
    ['', false],
    ['report=', false],
    ['report=rpt-261127-d00001&report=rpt-261127-d00001', false],
    ['report=5', false],
  ]
  for (const [query, flow] of reportRows) {
    test(`the Board address ?create=true${query ? `&${query}` : ''} ${flow ? 'starts' : 'does not start'} a fix task`, async ({
      page,
    }) => {
      await mockReportServer(page, { reports: [fixReport] })
      await page.goto(`/w/demo/?create=true${query ? `&${query}` : ''}`)
      await expect(sheet(page)).toBeVisible()
      await expect(sheet(page).locator('[data-fix-task]')).toHaveCount(flow ? 1 : 0)
      await expect(sheet(page).getByRole('combobox', { name: 'Repository' })).toHaveValue(flow ? '' : 'conveyor')
      // Closing the sheet drops every intake value from the address.
      await sheet(page).getByRole('button', { name: 'Close' }).first().click()
      await expect(page).toHaveURL('/w/demo/')
    })
  }
})

// Timers and refresh (class 5, invariant 27). Time is controlled and each
// step leaves a wide margin around a delay, so a slower machine cannot move a
// read across a step.
async function settle(page: Page) {
  await page.evaluate(() => new Promise((resolve) => setTimeout(resolve, 200)))
}

async function hideableTab(page: Page) {
  await page.addInitScript(() => {
    let hidden = false
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (hidden ? 'hidden' : 'visible'),
    })
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
    ;(window as unknown as { setTabHidden: (value: boolean) => void }).setTabHidden = (value: boolean) => {
      hidden = value
      document.dispatchEvent(new Event('visibilitychange', { bubbles: true }))
    }
  })
}

async function setTabHidden(page: Page, hidden: boolean) {
  await page.evaluate(
    (value) => (window as unknown as { setTabHidden: (value: boolean) => void }).setTabHidden(value),
    hidden,
  )
}

const taskListReads = (server: Server) => listReads(server).filter((read) => read.includes('task_id=261120-a01')).length

async function step(page: Page, reads: () => number, ms: number, expected: number) {
  const before = reads()
  await page.clock.fastForward(ms)
  if (expected > 0) await expect.poll(reads).toBe(before + expected)
  await settle(page)
  expect(reads()).toBe(before + expected)
}

async function openTask(page: Page, server: Server) {
  await page.goto('/w/demo/tasks/261120-a01/full')
  await expect.poll(() => taskListReads(server)).toBe(1)
  await settle(page)
}

test.describe('report timers', () => {
  test('an open report reads again every 60 seconds while the tab is visible, and stops after the page is left', async ({
    page,
  }) => {
    await page.clock.install({ time: now })
    await hideableTab(page)
    const server = await mockReportServer(page, { reports: [taskReport] })
    await openTask(page, server)
    const reads = () => taskListReads(server)
    await step(page, reads, 45_000, 0)
    await step(page, reads, 20_000, 1)
    // A hidden tab reads nothing, and showing it again waits the whole delay.
    await setTabHidden(page, true)
    await step(page, reads, 130_000, 0)
    await setTabHidden(page, false)
    await step(page, reads, 45_000, 0)
    await step(page, reads, 20_000, 1)
    // Offline reads nothing; online again waits the whole delay.
    await page.context().setOffline(true)
    await step(page, reads, 130_000, 0)
    await page.context().setOffline(false)
    await step(page, reads, 45_000, 0)
    await step(page, reads, 20_000, 1)
    // Leaving the page leaves no timer.
    await page.getByRole('link', { name: 'Crates' }).first().click()
    await expect(page).toHaveURL('/w/demo/crates')
    await step(page, reads, 200_000, 0)
  })

  test('pending links read after 10 seconds six times in a row, then only every 60 seconds', async ({ page }) => {
    await page.clock.install({ time: now })
    const server = await mockReportServer(page, { reports: [{ ...taskReport, links_pending: true }] })
    await openTask(page, server)
    const reads = () => taskListReads(server)
    for (let read = 1; read <= 6; read++) {
      await step(page, reads, 5_000, 0)
      await step(page, reads, 6_000, 1)
    }
    await step(page, reads, 40_000, 0)
    await step(page, reads, 25_000, 1)
    await step(page, reads, 45_000, 0)
    await step(page, reads, 20_000, 1)
  })

  test('an answer that is no longer pending ends the 10-second reads', async ({ page }) => {
    await page.clock.install({ time: now })
    const server = await mockReportServer(page, { reports: [{ ...taskReport, links_pending: true }] })
    await openTask(page, server)
    const reads = () => taskListReads(server)
    server.reports[0] = { ...server.reports[0], links_pending: false }
    await step(page, reads, 11_000, 1)
    await expect(card(page, taskReport.id).locator('[data-report-links-pending]')).toHaveCount(0)
    await step(page, reads, 45_000, 0)
    await step(page, reads, 20_000, 1)
  })

  test('a page that shows no report sets no timer', async ({ page }) => {
    await page.clock.install({ time: now })
    const server = await mockReportServer(page, { reports: [] })
    await openTask(page, server)
    await step(page, () => taskListReads(server), 200_000, 0)
  })

  test('a list of closed reports sets no 60-second timer, and pending links end after six reads', async ({ page }) => {
    await page.clock.install({ time: now })
    const server = await mockReportServer(page, { reports: [closed, { ...closedActReport, links_pending: true }] })
    await page.goto('/w/demo/monitor?show=closed')
    await expect.poll(() => listReads(server).length).toBe(1)
    await settle(page)
    const reads = () => listReads(server).length
    for (let read = 1; read <= 6; read++) await step(page, reads, 11_000, 1)
    await step(page, reads, 200_000, 0)
  })

  test('a closed list with nothing pending reads on no timer', async ({ page }) => {
    await page.clock.install({ time: now })
    const server = await mockReportServer(page, { reports: [closed] })
    await page.goto('/w/demo/monitor?show=closed')
    await expect.poll(() => listReads(server).length).toBe(1)
    await settle(page)
    await step(page, () => listReads(server).length, 200_000, 0)
  })

  test('a stream frame reads the reports again, and a frame during a read reads once more after it', async ({
    page,
  }) => {
    await page.clock.install({ time: now })
    const server = await mockReportServer(page, { reports: [taskReport] })
    await openTask(page, server)
    const reads = () => taskListReads(server)
    await server.stream.ready()
    await step(page, reads, 1_000, 1)
    await server.stream.activity('261120-zzz')
    await step(page, reads, 1_000, 1)
    // A frame while a read is on its way: one more read after it settles.
    const held = gate()
    server.listGate = held.promise
    await server.stream.activity()
    await step(page, reads, 1_000, 1)
    await server.stream.activity()
    await step(page, reads, 1_000, 0)
    held.open()
    server.listGate = null
    await expect.poll(reads).toBeGreaterThan(0)
    await settle(page)
    await expect.poll(reads).toBe(5)
    await settle(page)
    expect(reads()).toBe(5)
  })
})

// Bounds (class 8): the close reason is 1 to 500 characters, counted as the
// server counts them.
test.describe('close reason bounds', () => {
  const cases: Array<[string, string, boolean, string]> = [
    ['no character', '', false, '0 of 500 used'],
    ['500 characters', 'a'.repeat(500), true, '500 of 500 used'],
    ['501 characters', 'a'.repeat(501), false, '501 of 500 used'],
    ['500 emoji, each one character', '😀'.repeat(500), true, '500 of 500 used'],
    ['501 emoji', '😀'.repeat(501), false, '501 of 500 used'],
    ['three spaces', '   ', true, '3 of 500 used'],
  ]
  for (const [name, reason, sent, counter] of cases) {
    test(`${name} ${sent ? 'is sent' : 'is stopped before the server'}`, async ({ page }) => {
      const server = await mockReportServer(page, { reports: [actReport] })
      await page.goto('/w/demo/monitor')
      const item = card(page, actReport.id)
      await item.getByRole('button', { name: 'Close report' }).click()
      await expect(item).toContainText('Write 1 to 500 characters.')
      await item.getByRole('textbox', { name: 'Reason to close the report' }).fill(reason)
      await expect(item).toContainText(counter)
      const submit = item.getByRole('form', { name: 'Close the report' }).getByRole('button', { name: 'Close report' })
      if (reason.length > 0 && !sent) {
        await expect(submit).toBeDisabled()
        await expect(item.getByRole('alert')).toContainText(
          'The reason has 501 characters. Make it 500 characters or fewer.',
        )
      } else {
        await submit.click()
      }
      if (sent) {
        await expect(card(page, actReport.id)).toHaveCount(0)
        expect(server.acts).toEqual([{ method: 'POST', path: `${actPath}/close`, body: { reason } }])
      } else {
        if (reason.length === 0)
          await expect(item.getByRole('alert')).toHaveText('Write a reason of 1 to 500 characters.')
        expect(server.acts).toHaveLength(0)
      }
    })
  }
})

// Words and access (class 9).
test.describe('keyboard and names', () => {
  test('every act control is reached and used with the keyboard and shows focus', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    await expect(item).toBeVisible()
    const controls = item.locator('a, button, input, textarea, select')
    const names: string[] = []
    for (let index = 0; index < (await controls.count()); index++) {
      const control = controls.nth(index)
      await control.focus()
      const outline = await control.evaluate((element) => getComputedStyle(element).outlineStyle)
      names.push((await control.getAttribute('aria-label')) ?? ((await control.textContent()) ?? '').trim())
      expect(outline, `focus ring on ${names.at(-1)}`).not.toBe('none')
    }
    expect(names.every((name) => name.length > 0)).toBe(true)

    // Close through the keyboard only.
    await item.getByRole('button', { name: 'Close report' }).focus()
    await page.keyboard.press('Enter')
    const reason = item.getByRole('textbox', { name: 'Reason to close the report' })
    await reason.focus()
    await page.keyboard.type('Closed from the keyboard.')
    await page.keyboard.press('Tab')
    await page.keyboard.press('Enter')
    await expect(card(page, actReport.id)).toHaveCount(0)
    expect(server.acts).toEqual([
      { method: 'POST', path: `${actPath}/close`, body: { reason: 'Closed from the keyboard.' } },
    ])
  })

  test('no shortcut key records an act', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    await expect(card(page, actReport.id)).toBeVisible()
    await page.locator('body').click({ position: { x: 5, y: 5 } })
    for (const key of ['c', 'r', 'x', 'd', 'l', 'f', 'Enter', 'Delete', 'Backspace', 'Shift+Enter', 'Control+Enter']) {
      await page.keyboard.press(key)
    }
    await card(page, actReport.id).locator('[data-report-summary]').click()
    for (const key of ['c', 'r', 'Enter', 'Delete']) await page.keyboard.press(key)
    await page.waitForTimeout(300)
    expect(server.acts).toHaveLength(0)
  })
})

// Layout (class 7): the longest values the design allows, at 375 px and at a
// desktop width. The report surfaces add no dialog, popover, or menu: every
// form opens in place inside the card, and the fix-task part lives inside the
// existing create sheet.
const longest = report('rpt-261127-g00001', {
  summary: `${'Long summary words '.repeat(52)}${'y'.repeat(12)}`.slice(0, 1000),
  key: 'k'.repeat(200),
  service: 's'.repeat(100),
  file: `${'dir/'.repeat(124)}file`.slice(0, 500),
  line: 1_000_000,
  count: 1_000_000_000,
  environment: { ...staging, name: 'n'.repeat(100) },
  links: Array.from({ length: 30 }, (_, index) =>
    taskLink(
      `lnk-long-${index}`,
      `261120-l${String(index).padStart(2, '0')}`,
      `${'Very long task title '.repeat(10)}${index}`,
      { evidence: [{ ...lineEvidence, file: `${'path/'.repeat(100)}f.go` }, timingEvidence] },
      { id: 'crate-billing', name: 'c'.repeat(120) },
    ),
  ),
  notes: [
    {
      kind: 'too_many_tasks',
      deploy: {
        ...deploy,
        service: 'q'.repeat(100),
        version: { repository: 'app', commit: null, version: 'v'.repeat(200) },
      },
      task_count: 999,
    },
  ],
})

test.describe('layout', () => {
  for (const width of [375, 1280]) {
    test(`the longest values stay inside every report surface at ${width} px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 800 })
      await mockReportServer(page, { reports: [longest], crate: crate([crateLine({ open_report_count: 1_000 })]) })
      for (const address of ['/w/demo/monitor', '/w/demo/tasks/261120-l00/full', '/w/demo/crates/crate-billing']) {
        await page.goto(address)
        await expect(card(page, longest.id)).toBeVisible()
        await expectNoOverflow(page)
        // The close and link forms open in place and stay inside the card.
        await card(page, longest.id).getByRole('button', { name: 'Close report' }).click()
        await expect(card(page, longest.id).getByRole('form', { name: 'Close the report' })).toBeVisible()
        await expectNoOverflow(page)
        await card(page, longest.id).getByRole('button', { name: 'Link a task or crate' }).click()
        await expect(card(page, longest.id).getByRole('region', { name: 'Link a task or crate' })).toBeVisible()
        await expectNoOverflow(page)
      }
      await page.goto(`/w/demo/?create=true&report=${longest.id}`)
      await expect(sheet(page).locator('[data-fix-task]')).toBeVisible()
      await expectNoOverflow(page)
      // The sheet slides in; measure it where it stops.
      await page.evaluate(() => Promise.all(document.getAnimations().map((animation) => animation.finished)))
      const box = await sheet(page).locator('[data-fix-task]').boundingBox()
      expect(box).not.toBeNull()
      expect(box && box.x >= 0 && box.x + box.width <= width + 1).toBe(true)
    })
  }
})

// Screenshots for the pull request: set CONVEYOR_REPORT_SCREENSHOTS to a
// directory outside the checkout to write them. They are never committed.
const screenshotDir = process.env.CONVEYOR_REPORT_SCREENSHOTS
test.describe('screenshots', () => {
  test.skip(!screenshotDir, 'CONVEYOR_REPORT_SCREENSHOTS is not set')
  for (const width of [375, 1280]) {
    test(`renders each report surface at ${width} px`, async ({ page }) => {
      await page.clock.setFixedTime(now)
      await page.setViewportSize({ width, height: 1000 })
      await mockReportServer(page, {
        reports: [
          lineLinked,
          { ...timingLinked, links: [...timingLinked.links, ...lineLinked.links] },
          noLink,
          pending,
          closed,
        ],
        crate: crate([
          crateLine({ open_report_count: 1 }),
          crateLine({ environment: { id: 'env-staging', name: 'staging' }, listening: false, open_report_count: 0 }),
        ]),
      })
      const shots: Array<[string, string, string]> = [
        ['task-detail', '/w/demo/tasks/261120-a01/full', `[data-report-card="${lineLinked.id}"]`],
        ['crate', '/w/demo/crates/crate-billing', `[data-report-card="${timingLinked.id}"]`],
        ['monitor', '/w/demo/monitor', `[data-report-card="${lineLinked.id}"]`],
        ['fix-task-sheet', `/w/demo/?create=true&report=${lineLinked.id}`, '[data-fix-task]'],
      ]
      for (const [name, address, ready] of shots) {
        await page.goto(address)
        await expect(page.locator(ready).first()).toBeVisible()
        if (name === 'fix-task-sheet') {
          // The sheet is ready once the report filled it in.
          await expect(description(page)).not.toHaveValue('')
          await expect(sheet(page).getByRole('group', { name: 'Possible causes' })).toBeVisible()
          await page.evaluate(() => Promise.all(document.getAnimations().map((animation) => animation.finished)))
        }
        await page.locator(ready).first().scrollIntoViewIfNeeded()
        await page.screenshot({ path: `${screenshotDir}/${name}-${width}.png`, fullPage: name !== 'fix-task-sheet' })
      }
    })
  }
})

// The findings of the first automated review, each with its test.
test.describe('review findings', () => {
  test('another report in the address starts the flow again, and each report keeps its own saved task', async ({
    page,
  }) => {
    const other = report('rpt-261127-d00009', {
      summary: 'Another report',
      links: [taskLink('lnk-o', '261120-b02', 'Store invoice numbers')],
    })
    const server = await mockReportServer(page, { reports: [fixReport, other] })
    await page.goto(fixAddress)
    server.actAnswers.push({ status: 503, json: { error: 'retryable' } })
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toBeVisible()
    // The address moves to the other report while the sheet stays open.
    await page.evaluate((id) => {
      history.pushState({}, '', `/w/demo/?create=true&report=${id}`)
      dispatchEvent(new PopStateEvent('popstate'))
    }, other.id)
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText('Another report')
    await expect(sheet(page).locator('[data-fix-task-created]')).toHaveCount(0)
    await expect(recordButton(page)).toHaveCount(0)
    await expect(description(page)).toHaveValue(expectedDescription(other, '```'))
    const causes = sheet(page).getByRole('group', { name: 'Possible causes' }).getByRole('checkbox')
    await expect(causes).toHaveCount(1)
    await expect(causes.first()).not.toBeChecked()
    // Back to the first report: its saved task returns, checked by a fresh read.
    await page.goBack()
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('The report does not record it yet.')
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(fixPuts(server).map((act) => act.path)).toEqual([fixPath, fixPath])
    expect(server.created).toHaveLength(1)
  })

  test('a restored task waits for a fresh read, and a failed read offers only another read', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    server.actAnswers.push({ status: 503, json: { error: 'retryable' } })
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-error]')).toBeVisible()
    server.readAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await page.reload()
    await expect(sheet(page).locator('[data-fix-task-error]')).toHaveText(
      'The report did not load. Read the report again before you record the task.',
    )
    await expect(recordButton(page)).toHaveCount(0)
    server.readAnswer = null
    await sheet(page).getByRole('button', { name: 'Read the report again' }).click()
    await expect(recordButton(page)).toBeVisible()
    expect(fixPuts(server)).toHaveLength(1)
  })

  test('text typed while the report loads stays after the report block', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    const held = gate()
    server.readGate = held.promise
    await page.goto(fixAddress)
    await description(page).fill('Typed first.')
    await expect(createButton(page)).toBeDisabled()
    held.open()
    server.readGate = null
    await expect(description(page)).toHaveValue(`${expectedDescription(fixReport, '````````')}\nTyped first.`)
  })

  test('the form controls of a card wait while an act is on its way', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    await item.getByRole('button', { name: 'Close report' }).click()
    await item.getByRole('textbox', { name: 'Reason to close the report' }).fill('Kept reason')
    const slow = gate()
    server.actGate = slow.promise
    server.actAnswers.push({ status: 503, json: { error: 'retryable' } })
    await item
      .getByRole('form', { name: 'Close the report' })
      .getByRole('button', { name: 'Closing…' })
      .or(item.getByRole('form', { name: 'Close the report' }).getByRole('button', { name: 'Close report' }))
      .click()
    await expect(item.getByRole('button', { name: 'Link a task or crate' })).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Close report', exact: true }).first()).toBeDisabled()
    slow.open()
    server.actGate = null
    await expect(item.locator('[data-report-act-error]')).toHaveText('Conveyor recorded nothing. Try again.')
    await expect(item.getByRole('textbox', { name: 'Reason to close the report' })).toHaveValue('Kept reason')
    await expect(item.getByRole('button', { name: 'Link a task or crate' })).toBeEnabled()
  })

  test('a failed task search offers to load the list again and keeps the search text', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    server.failTaskSearch = true
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    await item.getByRole('button', { name: 'Link a task or crate' }).click()
    const search = (status: number) =>
      page.waitForResponse(
        (response) =>
          new URL(response.url()).pathname === '/v1/tasks' &&
          new URL(response.url()).searchParams.get('q') === 'numbers' &&
          response.status() === status,
      )
    // The search for "numbers" itself fails, not only the first empty search.
    const failed = search(500)
    await item.getByRole('textbox', { name: 'Search merged tasks' }).fill('numbers')
    await failed
    await expect(item).toContainText('The list did not load.')
    server.failTaskSearch = false
    // The control repeats that same search, and it answers.
    const answered = search(200)
    await item.getByRole('button', { name: 'Load the list again' }).click()
    await answered
    await expect(item.getByRole('button', { name: 'Link Store invoice numbers' })).toBeVisible()
    await expect(item.getByRole('textbox', { name: 'Search merged tasks' })).toHaveValue('numbers')
  })

  test('a failed refresh of an empty list is told', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [] })
    await page.goto('/w/demo/monitor')
    await expect(page.getByText('No open report. Reports appear here when a watcher sends one.')).toBeVisible()
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await server.stream.ready()
    await expect(page.locator('[data-report-refresh-error]')).toHaveText(
      'The last refresh of the reports failed. This list shows the reports from the read before it.',
    )
    await expect(page.getByText('No open report. Reports appear here when a watcher sends one.')).toBeVisible()
  })

  test('after an unknown answer, a failed read keeps every act blocked until a read succeeds', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    const confirm = item.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirm).toBeEnabled()
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await confirm.click()
    await expect(item.locator('[data-report-act-error]')).toContainText(
      'Conveyor does not know whether it recorded this, and the report did not load again.',
    )
    await expect(confirm).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Close report' })).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Link a task or crate' })).toBeDisabled()
    // A read that fails again keeps the block.
    await item.getByRole('button', { name: 'Read the report again' }).click()
    await expect(item.getByRole('button', { name: 'Read the report again' })).toBeEnabled()
    await expect(confirm).toBeDisabled()
    server.listAnswer = null
    await item.getByRole('button', { name: 'Read the report again' }).click()
    await expect(item.locator('[data-report-act-error]')).toHaveText(
      'Conveyor does not know whether it recorded this. Conveyor read the report again. Look at it before you try again.',
    )
    await expect(confirm).toBeEnabled()
    expect(server.acts).toHaveLength(1)
  })

  test('a task a report names on the crate page opens in the crate page’s task sheet', async ({ page }) => {
    const member = report('rpt-261127-c00009', {
      summary: 'Crate member report',
      links: [
        taskLink('lnk-member', '261120-a01', 'Send invoices by email', {}, { id: 'crate-billing', name: 'Billing v2' }),
      ],
      fix_task: { id: '261120-b02', title: 'Store invoice numbers' },
    })
    await mockReportServer(page, { reports: [member] })
    await page.goto('/w/demo/crates/crate-billing')
    await card(page, member.id)
      .locator('[data-report-link="lnk-member"]')
      .getByRole('link', { name: 'Send invoices by email' })
      .click()
    await expect(page).toHaveURL('/w/demo/crates/crate-billing?task=261120-a01')
    await expect(page.getByRole('dialog')).toBeVisible()
    // The crate page stays mounted under the sheet, and the sheet's task
    // detail lists the same report.
    await expect(card(page, member.id)).toHaveCount(2)
    await page.goto('/w/demo/crates/crate-billing')
    await card(page, member.id).locator('[data-report-fix-task]').getByRole('link').click()
    await expect(page).toHaveURL('/w/demo/crates/crate-billing?task=261120-b02')
    // Monitor keeps the full-page link.
    await page.goto('/w/demo/monitor')
    await expect(card(page, member.id).locator('[data-report-fix-task] a')).toHaveAttribute(
      'href',
      '/w/demo/tasks/261120-b02',
    )
  })
})

// The follow-up review of the fixes.
test.describe('review follow-ups', () => {
  const other = report('rpt-261127-d00010', {
    summary: 'Second report',
    links: [taskLink('lnk-s', '261120-b02', 'Store invoice numbers')],
  })
  const moveTo = (page: Page, id: string) =>
    page.evaluate((reportId) => {
      history.pushState({}, '', `/w/demo/?create=true&report=${reportId}`)
      dispatchEvent(new PopStateEvent('popstate'))
    }, id)
  const saved = (page: Page) =>
    page.evaluate(() => Object.keys(sessionStorage).filter((key) => key.startsWith('conveyor-fix-task:')))
  const savedEntry = (page: Page, id: string) =>
    page.evaluate((key) => {
      const raw = sessionStorage.getItem(key)
      return raw ? (JSON.parse(raw) as Record<string, unknown>) : null
    }, `conveyor-fix-task:demo:${id}`)

  for (const outcome of ['recorded', 'refused'] as const) {
    test(`a record request ${outcome} after the address moved to another report changes nothing there`, async ({
      page,
    }) => {
      const server = await mockReportServer(page, { reports: [fixReport, other] })
      await page.goto(fixAddress)
      const held = gate()
      server.actGate = held.promise
      if (outcome === 'refused') server.actAnswers.push({ status: 503, json: { error: 'retryable' } })
      await createFixTask(page)
      await expect(sheet(page).locator('[data-fix-task-created]')).toContainText(
        'Conveyor records it on the report now…',
      )
      await moveTo(page, other.id)
      await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText('Second report')
      const answered = page.waitForResponse(
        (response) => response.request().method() === 'PUT' && new URL(response.url()).pathname === fixPath,
      )
      held.open()
      server.actGate = null
      await answered
      // The page has handled the answer once the first report's saved task
      // shows its result: removed when recorded, `not_recorded` when refused.
      await expect
        .poll(() => savedEntry(page, fixReport.id))
        .toEqual(outcome === 'recorded' ? null : expect.objectContaining({ status: 'not_recorded' }))
      expect(fixPuts(server)).toHaveLength(1)
      // The second report's flow is untouched: no navigation, no created
      // task, no error, and its prefill and Create stay as they were.
      await expect(page).toHaveURL(`/w/demo/?create=true&report=${other.id}`)
      await expect(sheet(page).locator('[data-fix-task-created], [data-fix-task-error]')).toHaveCount(0)
      await expect(description(page)).toHaveValue(expectedDescription(other, '```'))
      // The first report keeps its saved task only while it is not recorded.
      expect(await saved(page)).toEqual(outcome === 'recorded' ? [] : [`conveyor-fix-task:demo:${fixReport.id}`])
      expect(server.created).toHaveLength(1)
    })
  }

  test('after an unknown answer, a read that waits offline keeps every act blocked until a read answers', async ({
    page,
    context,
  }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    const confirm = item.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirm).toBeEnabled()
    // Offline, the act gets no answer and the fresh read waits paused. A
    // paused read is not an answer, so every act stays blocked.
    server.actAnswers.push('abort')
    await context.setOffline(true)
    await confirm.click()
    await expect(item.locator('[data-report-act-error]')).toContainText(
      'Conveyor does not know whether it recorded this, and the report did not load again.',
    )
    await expect(confirm).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Close report' })).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Link a task or crate' })).toBeDisabled()
    // Coming back online unblocks nothing by itself.
    await context.setOffline(false)
    await page.waitForTimeout(500)
    await expect(confirm).toBeDisabled()
    // A read that answers lets the person act again.
    await item.getByRole('button', { name: 'Read the report again' }).click()
    await expect(item.locator('[data-report-act-error]')).toHaveText(
      'Conveyor does not know whether it recorded this. Conveyor read the report again. Look at it before you try again.',
    )
    await expect(confirm).toBeEnabled()
    // The act was sent once and never again by itself.
    expect(server.acts).toHaveLength(1)
  })
})

// The second follow-up review.
test.describe('review follow-ups, second round', () => {
  const second = report('rpt-261127-d00011', {
    summary: 'Second report',
    links: [taskLink('lnk-s2', '261120-b02', 'Store invoice numbers')],
  })

  test('A, then B, then A with both record requests on their way sends no second request for A', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport, second] })
    await page.goto(fixAddress)
    const held = gate()
    server.actGate = held.promise
    await createFixTask(page)
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('Conveyor records it on the report now…')
    // B starts its own fix task while A's request waits.
    await page.evaluate((id) => {
      history.pushState({}, '', `/w/demo/?create=true&report=${id}`)
      dispatchEvent(new PopStateEvent('popstate'))
    }, second.id)
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText('Second report')
    await createFixTask(page, [])
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('Conveyor records it on the report now…')
    // Back to A: its request is still on its way, so nothing can send another.
    await page.goBack()
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText(fixReport.summary)
    await expect(sheet(page).locator('[data-fix-task-created]')).toBeVisible()
    await page.waitForTimeout(300)
    await expect(recordButton(page)).toHaveCount(0)
    await expect(createButton(page)).toBeDisabled()
    expect(fixPuts(server).map((act) => act.path)).toEqual([fixPath, `/v1/feedback-reports/${second.id}/fix-task`])
    held.open()
    server.actGate = null
    // A's own answer finishes A's flow; B's answer clears B's saved task.
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    await page.waitForTimeout(300)
    expect(fixPuts(server)).toHaveLength(2)
    expect(server.created).toHaveLength(2)
    expect(
      await page.evaluate(() => Object.keys(sessionStorage).filter((key) => key.startsWith('conveyor-fix-task:'))),
    ).toEqual([])
  })

  test('with a fixed clock, a failed read after an unknown answer still keeps every act blocked', async ({ page }) => {
    await page.clock.setFixedTime(now)
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    const confirm = item.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirm).toBeEnabled()
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await confirm.click()
    await expect(item.locator('[data-report-act-error]')).toContainText(
      'Conveyor does not know whether it recorded this, and the report did not load again.',
    )
    await expect(confirm).toBeDisabled()
    server.listAnswer = null
    await item.getByRole('button', { name: 'Read the report again' }).click()
    await expect(confirm).toBeEnabled()
    expect(server.acts).toHaveLength(1)
  })
})

// The third follow-up review.
test.describe('review follow-ups, third round', () => {
  test('a list read already on its way when an act gets no answer never counts as the fresh read', async ({ page }) => {
    const reports = Array.from({ length: 60 }, (_, index) =>
      report(`rpt-261127-j${String(index).padStart(5, '0')}`, {
        summary: `Report ${index}`,
        last_seen: new Date(Date.UTC(2026, 10, 27, 11, 0, index)).toISOString(),
        links:
          index === 59
            ? [taskLink('lnk-first', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] })]
            : [],
      }),
    )
    const server = await mockReportServer(page, { reports })
    await page.goto('/w/demo/monitor')
    const first = card(page, 'rpt-261127-j00059')
    const confirm = first.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirm).toBeEnabled()
    // "Next page" starts a read that waits. The first page stays shown.
    const before = gate()
    server.listGate = before.promise
    await page.getByRole('button', { name: 'Next page' }).click()
    await expect.poll(() => listReads(server).length).toBe(2)
    // The act gets a 500 while that read still waits. The fresh read waits too.
    const after = gate()
    server.listGate = after.promise
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    await confirm.click()
    await expect.poll(() => listReads(server).length).toBe(3)
    // The read from before the act answers: it counts for nothing.
    before.open()
    await page.waitForTimeout(500)
    await expect(first.getByRole('status').filter({ hasText: 'Conveyor reads the report again' })).toBeVisible()
    await expect(confirm).toBeDisabled()
    // The read that started after the act answers. The second page shows,
    // and the report's state stays with the report: back on the first page,
    // its card shows the note and can act again.
    after.open()
    server.listGate = null
    await expect(page.locator('[data-report-list-count]')).toHaveText(
      'This list shows reports 51 to 60 of 60, newest first.',
    )
    await page.getByRole('button', { name: 'Previous page' }).click()
    await expect(first.locator('[data-report-act-error]')).toHaveText(
      'Conveyor does not know whether it recorded this. Conveyor read the report again. Look at it before you try again.',
    )
    await expect(confirm).toBeEnabled()
    expect(server.acts).toHaveLength(1)
  })
})

// Class: lists. Every list reaches every report the server holds, in the
// server's order, each once, through the served offset and total.
function many(count: number, prefix: string, overrides: (index: number) => Partial<FeedbackReport> = () => ({})) {
  return Array.from({ length: count }, (_, index) =>
    report(`rpt-261127-${prefix}${String(index).padStart(5, '0')}`, {
      summary: `${prefix} ${index}`,
      last_seen: new Date(Date.UTC(2026, 10, 27, 8, 0, 0) + index * 1000).toISOString(),
      ...overrides(index),
    }),
  )
}
const memberLink = (index: number) => [
  taskLink(`lnk-m${index}`, '261120-a01', 'Send invoices by email', {}, { id: 'crate-billing', name: 'Billing v2' }),
]
const summariesOn = (page: Page) => page.locator('[data-report-card] [data-report-summary]').allTextContents()
const countText = (page: Page) => page.locator('[data-report-list-count]')

interface ListSurface {
  name: string
  address: string
  reports: FeedbackReport[]
  // The summaries the list holds, newest first.
  expected: string[]
}
function surfaces(): ListSurface[] {
  const open = many(205, 'o')
  const inStaging = many(105, 's', () => ({ environment: staging }))
  const noCrate = many(105, 'n')
  const inCrate = many(5, 'c', (index) => ({ links: memberLink(index) }))
  const shut = many(105, 'x', () => ({
    state: 'closed',
    closed: { at: '2026-11-27T11:30:00Z', by: 'usr_operator', reason: 'Done.' },
  }))
  const task = many(105, 't', (index) => ({ links: memberLink(index) }))
  const newestFirst = (items: FeedbackReport[]) => [...items].reverse().map((item) => item.summary)
  return [
    { name: 'Monitor, open reports', address: '/w/demo/monitor', reports: open, expected: newestFirst(open) },
    {
      name: 'Monitor, one environment',
      address: '/w/demo/monitor?environment=env-staging',
      reports: [...inStaging, ...many(3, 'p')],
      expected: newestFirst(inStaging),
    },
    {
      name: 'Monitor, reports in no crate',
      address: '/w/demo/monitor?no_crate=true',
      reports: [...noCrate, ...inCrate],
      expected: newestFirst(noCrate),
    },
    {
      name: 'Monitor, closed reports',
      address: '/w/demo/monitor?show=closed',
      reports: [...shut, ...many(3, 'q')],
      expected: newestFirst(shut),
    },
    {
      name: 'task detail',
      address: '/w/demo/tasks/261120-a01/full',
      reports: [...task, ...many(3, 'r')],
      expected: newestFirst(task),
    },
    {
      name: 'the crate page',
      address: '/w/demo/crates/crate-billing',
      reports: [...task, ...many(3, 'r')],
      expected: newestFirst(task),
    },
  ]
}

test.describe('lists reach every report', () => {
  for (const surface of surfaces()) {
    test(`${surface.name}: every report in server order, each once, page by page`, async ({ page }) => {
      test.setTimeout(90_000)
      const server = await mockReportServer(page, { reports: surface.reports })
      await page.goto(surface.address)
      const seen: string[] = []
      const total = surface.expected.length
      for (let offset = 0; offset < total; offset += 50) {
        const end = Math.min(offset + 50, total)
        await expect(countText(page)).toHaveText(
          `This list shows reports ${offset + 1} to ${end} of ${total}, newest first.`,
        )
        seen.push(...(await summariesOn(page)))
        if (end < total) await page.getByRole('button', { name: 'Next page' }).click()
      }
      await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled()
      expect(seen).toEqual(surface.expected)
      const offsets = listReads(server).map((read) => new URLSearchParams(read.split('?')[1]))
      expect(offsets.every((params) => params.get('limit') === '50')).toBe(true)
      // Back to the first page, one page at a time, each page rendered
      // before the next click.
      for (let offset = Math.floor((total - 1) / 50) * 50 - 50; offset >= 0; offset -= 50) {
        await page.getByRole('button', { name: 'Previous page' }).click()
        await expect(countText(page)).toHaveText(
          `This list shows reports ${offset + 1} to ${offset + 50} of ${total}, newest first.`,
        )
      }
      await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled()
    })
  }

  test('a refresh keeps the page, and a total that shrank moves to the last page it holds', async ({ page }) => {
    const server = await mockReportServer(page, { reports: many(105, 'o') })
    await page.goto('/w/demo/monitor')
    await page.getByRole('button', { name: 'Next page' }).click()
    await expect(countText(page)).toHaveText('This list shows reports 51 to 100 of 105, newest first.')
    const reads = listReads(server).length
    await server.stream.ready()
    await expect.poll(() => listReads(server).length).toBeGreaterThan(reads)
    expect(new URLSearchParams(listReads(server).at(-1)?.split('?')[1]).get('offset')).toBe('50')
    await expect(countText(page)).toHaveText('This list shows reports 51 to 100 of 105, newest first.')
    // Third page, then the server holds only 95 open reports.
    await page.getByRole('button', { name: 'Next page' }).click()
    await expect(countText(page)).toHaveText('This list shows reports 101 to 105 of 105, newest first.')
    server.reports = server.reports.slice(10)
    await server.stream.activity()
    await expect(countText(page)).toHaveText('This list shows reports 51 to 95 of 95, newest first.')
    expect(await summariesOn(page)).toEqual(
      [...server.reports]
        .reverse()
        .slice(50)
        .map((item) => item.summary),
    )
  })

  test('another filter starts at the first page and never shows the reports of the filter before it', async ({
    page,
  }) => {
    const server = await mockReportServer(page, {
      reports: [...many(105, 'o'), ...many(3, 's', () => ({ environment: staging }))],
    })
    await page.goto('/w/demo/monitor')
    await page.getByRole('button', { name: 'Next page' }).click()
    await expect(countText(page)).toHaveText('This list shows reports 51 to 100 of 108, newest first.')
    const held = gate()
    server.listGate = held.promise
    await page.getByRole('combobox', { name: 'Environment' }).selectOption('env-staging')
    // While the staging read waits, no report of the filter before shows.
    await expect(page.locator('[data-report-list-loading]')).toBeVisible()
    await expect(page.locator('[data-report-card]')).toHaveCount(0)
    held.open()
    server.listGate = null
    await expect(page.locator('[data-report-card]')).toHaveCount(3)
    expect(await summariesOn(page)).toEqual(['s 2', 's 1', 's 0'])
    const params = new URLSearchParams(listReads(server).at(-1)?.split('?')[1])
    expect([params.get('environment_id'), params.get('offset')]).toEqual(['env-staging', '0'])
  })

  test('filters that change and change back start again at the first page', async ({ page }) => {
    const server = await mockReportServer(page, {
      reports: [...many(105, 'o'), ...many(3, 's', () => ({ environment: staging }))],
    })
    await page.goto('/w/demo/monitor')
    await page.getByRole('button', { name: 'Next page' }).click()
    await expect(countText(page)).toHaveText('This list shows reports 51 to 100 of 108, newest first.')
    const offset50 = () =>
      listReads(server).filter((read) => new URLSearchParams(read.split('?')[1]).get('offset') === '50').length
    const before = offset50()
    await page.getByRole('combobox', { name: 'Environment' }).selectOption('env-staging')
    await expect(page.locator('[data-report-card]')).toHaveCount(3)
    await page.getByRole('combobox', { name: 'Environment' }).selectOption('')
    await expect(countText(page)).toHaveText('This list shows reports 1 to 50 of 108, newest first.')
    // The page before is never read again for the filters that came back.
    await page.waitForTimeout(300)
    expect(offset50()).toBe(before)
  })

  test('the next page of the same filters keeps the page before it while it loads, with paging blocked', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: many(105, 'o') })
    await page.goto('/w/demo/monitor')
    await expect(countText(page)).toHaveText('This list shows reports 1 to 50 of 105, newest first.')
    const held = gate()
    server.listGate = held.promise
    await page.getByRole('button', { name: 'Next page' }).click()
    await expect(countText(page)).toHaveText('Loading the next page…')
    expect((await summariesOn(page))[0]).toBe('o 104')
    await expect(page.getByRole('button', { name: 'Next page' })).toBeDisabled()
    await expect(page.getByRole('button', { name: 'Previous page' })).toBeDisabled()
    held.open()
    server.listGate = null
    await expect(countText(page)).toHaveText('This list shows reports 51 to 100 of 105, newest first.')
    expect((await summariesOn(page))[0]).toBe('o 54')
  })
})

// Class: late answers. Each answer reaches only what asked for it.
test.describe('late answers', () => {
  const second = report('rpt-261127-d00021', {
    summary: 'Second report',
    links: [taskLink('lnk-s21', '261120-b02', 'Store invoice numbers')],
  })
  const moveTo = (page: Page, address: string) =>
    page.evaluate((to) => {
      history.pushState({}, '', to)
      dispatchEvent(new PopStateEvent('popstate'))
    }, address)
  const savedEntry = (page: Page, id: string) =>
    page.evaluate((key) => {
      const raw = sessionStorage.getItem(key)
      return raw ? (JSON.parse(raw) as Record<string, unknown>) : null
    }, `conveyor-fix-task:demo:${id}`)

  test('a prefill read of report A that answers after the address names report B never fills B', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport, second] })
    const held = gate()
    server.readGate = held.promise
    await page.goto(fixAddress)
    await expect.poll(() => reportReads(server, fixReport.id)).toBe(1)
    server.readGate = null
    await moveTo(page, `/w/demo/?create=true&report=${second.id}`)
    await expect(description(page)).toHaveValue(expectedDescription(second, '```'))
    held.open()
    await page.waitForTimeout(500)
    await expect(description(page)).toHaveValue(expectedDescription(second, '```'))
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText('Second report')
  })

  test('a prefill read that answers after the sheet closed fills the reopened sheet once', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    const held = gate()
    server.readGate = held.promise
    await page.goto(fixAddress)
    await expect.poll(() => reportReads(server, fixReport.id)).toBe(1)
    await sheet(page).getByRole('button', { name: 'Close' }).first().click()
    await expect(sheet(page)).toHaveCount(0)
    server.readGate = null
    held.open()
    await page.waitForTimeout(300)
    await moveTo(page, fixAddress)
    await expect(description(page)).toHaveValue(expectedDescription(fixReport, '````````'))
    await server.stream.ready()
    await page.waitForTimeout(300)
    await expect(description(page)).toHaveValue(expectedDescription(fixReport, '````````'))
  })

  test("task intake for report A that answers after the address names report B records only on A, with A's causes", async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [fixReport, second] })
    await page.goto(fixAddress)
    const held = gate()
    server.createGate = held.promise
    await createFixTask(page, ['Send invoices by email'])
    await expect.poll(() => server.created.length).toBe(1)
    // A second Create cannot start while intake is on its way.
    await expect(createButton(page)).toHaveText('Creating…')
    await expect(createButton(page)).toBeDisabled()
    await moveTo(page, `/w/demo/?create=true&report=${second.id}`)
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText('Second report')
    const recorded = page.waitForResponse(
      (response) => response.request().method() === 'PUT' && new URL(response.url()).pathname === fixPath,
    )
    held.open()
    server.createGate = null
    await recorded
    await expect.poll(() => savedEntry(page, fixReport.id)).toBeNull()
    expect(fixPuts(server)).toEqual([
      { method: 'PUT', path: fixPath, body: { task_id: '261127-fix', cause_task_ids: ['261120-a01'] } },
    ])
    // Report B's sheet is untouched.
    await expect(page).toHaveURL(`/w/demo/?create=true&report=${second.id}`)
    await expect(sheet(page).locator('[data-fix-task-created], [data-fix-task-error]')).toHaveCount(0)
    await expect(description(page)).toHaveValue(expectedDescription(second, '```'))
    expect(await savedEntry(page, second.id)).toBeNull()
    expect(server.reports.find((item) => item.id === second.id)?.fix_task).toBeNull()
  })

  test('task intake that answers after the sheet closed records on its report and opens nothing', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    const held = gate()
    server.createGate = held.promise
    await createFixTask(page, [])
    await expect.poll(() => server.created.length).toBe(1)
    await sheet(page).getByRole('button', { name: 'Close' }).first().click()
    await expect(page).toHaveURL('/w/demo/')
    held.open()
    server.createGate = null
    await expect.poll(() => fixPuts(server).length).toBe(1)
    await expect.poll(() => savedEntry(page, fixReport.id)).toBeNull()
    await expect(page).toHaveURL('/w/demo/')
    expect(fixPuts(server)[0].body).toEqual({ task_id: '261127-fix' })
  })

  test('after an unknown record answer, a report read that started before it never sets the next step', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    const put = gate()
    server.actGate = put.promise
    await createFixTask(page, [])
    await expect.poll(() => fixPuts(server).length).toBe(1)
    // A stream frame starts a report read that holds the report as it was
    // before the record.
    const before = structuredClone(server.reports[0])
    const old = gate()
    server.readGate = old.promise
    let staleOnce = true
    server.readAnswer = () => {
      if (!staleOnce) return undefined
      staleOnce = false
      return { status: 200, json: before }
    }
    const reads = reportReads(server, fixReport.id)
    await server.stream.ready()
    await expect.poll(() => reportReads(server, fixReport.id)).toBe(reads + 1)
    // The record is stored, but its answer is lost.
    const fresh = gate()
    server.readGate = fresh.promise
    server.actAnswers.push(() => {
      server.reports[0] = { ...server.reports[0], fix_task: { id: '261127-fix', title: 'Fix the checkout timeout' } }
      return { status: 500, json: { error: 'internal error' } }
    })
    put.open()
    server.actGate = null
    await expect.poll(() => reportReads(server, fixReport.id)).toBe(reads + 2)
    // The old read answers with the report from before: it decides nothing.
    old.open()
    await page.waitForTimeout(500)
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText(
      'Conveyor reads the report again to find out if it recorded the task…',
    )
    await expect(recordButton(page)).toHaveCount(0)
    // The fresh read answers: the task is recorded, and it opens.
    fresh.open()
    server.readGate = null
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(fixPuts(server)).toHaveLength(1)
  })

  test('a list read of one workspace that answers after the address names another workspace shows nothing there', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: many(3, 'o') })
    const held = gate()
    server.listGate = held.promise
    await page.goto('/w/demo/monitor')
    await expect.poll(() => listReads(server).length).toBe(1)
    server.listGate = null
    await moveTo(page, '/w/other/monitor')
    await expect(page.getByText('No open report. Reports appear here when a watcher sends one.')).toBeVisible()
    held.open()
    await page.waitForTimeout(500)
    await expect(page.locator('[data-report-card]')).toHaveCount(0)
  })

  test('an act whose unknown answer arrives after the page was left leaves no old report to act on', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const confirm = card(page, actReport.id).getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirm).toBeEnabled()
    const held = gate()
    server.actGate = held.promise
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    await confirm.click()
    await moveTo(page, '/w/demo/crates')
    await expect(page.locator('[data-report-card]')).toHaveCount(0)
    held.open()
    server.actGate = null
    await page.waitForTimeout(500)
    // Back on Monitor, the page reads the report before it shows it.
    const reads = listReads(server).length
    await moveTo(page, '/w/demo/monitor')
    await expect.poll(() => listReads(server).length).toBeGreaterThan(reads)
    await expect(card(page, actReport.id).locator('[data-report-act-error]')).toContainText(
      'Conveyor does not know whether it recorded this.',
    )
    await expect(confirm).toBeEnabled()
    expect(server.acts).toHaveLength(1)
  })
})

// Class: unknown outcomes. The page knows an unknown outcome for the workspace
// and the report, wherever the report shows, until a fresh read answers.
test.describe('unknown outcomes', () => {
  test('a removal with an unknown answer and a failed read keeps the control at card level after the row goes', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    await item.getByRole('button', { name: 'Remove the link to Send invoices by email…' }).click()
    // The removal is stored, but its answer is lost, and the fresh read fails.
    server.actAnswers.push(() => {
      server.reports[0] = { ...server.reports[0], links: [] }
      return { status: 500, json: { error: 'internal error' } }
    })
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await item.getByRole('button', { name: 'Remove the link to Send invoices by email', exact: true }).click()
    await expect(item.locator('[data-report-act-stale]')).toBeVisible()
    // A later refresh answers and removes the link row.
    server.listAnswer = null
    await server.stream.ready()
    await expect(item.locator('[data-report-link]')).toHaveCount(0)
    // The control to read again stays on the card, and every act waits.
    await expect(item.locator('[data-report-act-stale]')).toContainText('Read the report again before you act.')
    await expect(item.getByRole('button', { name: 'Close report' })).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Link a task or crate' })).toBeDisabled()
    // Starting a fix task records an act on the report, so it waits too.
    await expect(item.getByRole('button', { name: 'Start a fix task' })).toBeDisabled()
    await expect(item.locator('a[data-report-start-fix-task]')).toHaveCount(0)
    await item.getByRole('button', { name: 'Read the report again' }).click()
    await expect(item.locator('[data-report-act-error]')).toHaveText(
      'Conveyor does not know whether it recorded this. Conveyor read the report again. Look at it before you try again.',
    )
    await expect(item.getByRole('button', { name: 'Close report' })).toBeEnabled()
    await expect(item.getByRole('link', { name: 'Start a fix task' })).toHaveAttribute(
      'href',
      `/w/demo/?create=true&report=${actReport.id}`,
    )
    expect(server.acts).toHaveLength(1)
  })

  test('two copies of one report share the state: neither can act while the other waits or is blocked', async ({
    page,
  }) => {
    const member = report('rpt-261127-k00001', {
      summary: 'Shown twice',
      links: [
        taskLink('lnk-k1', '261120-a01', 'Send invoices by email', {}, { id: 'crate-billing', name: 'Billing v2' }),
      ],
    })
    const server = await mockReportServer(page, { reports: [member] })
    await page.goto('/w/demo/crates/crate-billing?task=261120-a01')
    const copies = card(page, member.id)
    await expect(copies).toHaveCount(2)
    const inPage = copies.nth(0)
    const inSheet = copies.nth(1)
    const confirmIn = (copy: Locator) =>
      copy.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    // An act on its way in the sheet blocks the copy in the page.
    const held = gate()
    server.actGate = held.promise
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await confirmIn(inSheet).click()
    await expect(confirmIn(inPage)).toBeDisabled()
    // The page copy lies under the sheet, so the click goes to the button
    // itself. A disabled button sends nothing.
    await confirmIn(inPage).evaluate((button: HTMLButtonElement) => button.click())
    held.open()
    server.actGate = null
    // The answer is unknown and the fresh read fails: both copies block and
    // both offer to read again.
    for (const copy of [inPage, inSheet]) {
      await expect(copy.locator('[data-report-act-stale]')).toBeVisible()
      await expect(confirmIn(copy)).toBeDisabled()
    }
    expect(server.acts).toHaveLength(1)
    // The sheet closes: the copy that sent the act is gone, and the copy in
    // the page still blocks and offers the read.
    await page.getByRole('button', { name: 'Close panel' }).click()
    await expect(copies).toHaveCount(1)
    await expect(inPage.locator('[data-report-act-stale]')).toBeVisible()
    await expect(confirmIn(inPage)).toBeDisabled()
    // A read from the copy in the page frees the report.
    server.listAnswer = null
    await inPage.getByRole('button', { name: 'Read the report again' }).click()
    await expect(confirmIn(inPage)).toBeEnabled()
    expect(server.acts).toHaveLength(1)
  })

  test('an unknown fix-task record whose fresh read fails blocks the report on every page until a read answers', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    server.readAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await createFixTask(page, [])
    await expect(sheet(page).locator('[data-fix-task-error]')).toContainText('the report did not load again')
    server.readAnswer = null
    await page.evaluate(() => {
      history.pushState({}, '', '/w/demo/monitor')
      dispatchEvent(new PopStateEvent('popstate'))
    })
    const item = card(page, fixReport.id)
    await expect(item.locator('[data-report-act-stale]')).toBeVisible()
    await expect(item.getByRole('button', { name: 'Close report' })).toBeDisabled()
    await item.getByRole('button', { name: 'Read the report again' }).click()
    await expect(item.getByRole('button', { name: 'Close report' })).toBeEnabled()
    expect(fixPuts(server)).toHaveLength(1)
  })
})

test.describe('unknown outcomes, replaced reads', () => {
  test('a fresh read replaced by another refresh still frees the report when the replacement answers', async ({
    page,
  }) => {
    const other = report('rpt-261127-k00002', {
      summary: 'Another report',
      last_seen: '2026-11-27T11:00:00Z',
      links: [taskLink('lnk-k2', '261120-b02', 'Store invoice numbers')],
    })
    const server = await mockReportServer(page, { reports: [actReport, other] })
    await page.goto('/w/demo/monitor')
    const first = card(page, actReport.id)
    const confirmFirst = first.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirmFirst).toBeEnabled()
    // The act on the first report gets a 500, and its fresh read waits.
    const held = gate()
    server.listGate = held.promise
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    const reads = listReads(server).length
    await confirmFirst.click()
    await expect.poll(() => listReads(server).length).toBe(reads + 1)
    // An act on the other report succeeds. Its refresh cancels the waiting
    // read and starts a new one, which waits too.
    await card(page, other.id).getByRole('button', { name: 'Confirm the link to Store invoice numbers' }).click()
    await expect.poll(() => listReads(server).length).toBe(reads + 2)
    held.open()
    server.listGate = null
    // The new read started after the unknown answer and answered: the first
    // report is free, with the note, and never blocked.
    await expect(first.locator('[data-report-act-error]')).toHaveText(
      'Conveyor does not know whether it recorded this. Conveyor read the report again. Look at it before you try again.',
    )
    await expect(first.locator('[data-report-act-stale]')).toHaveCount(0)
    await expect(confirmFirst).toBeEnabled()
  })
})

test.describe('screenshots of pages and blocked cards', () => {
  test.skip(!screenshotDir, 'CONVEYOR_REPORT_SCREENSHOTS is not set')
  for (const width of [375, 1280]) {
    test(`renders the page controls and a blocked card at ${width} px`, async ({ page }) => {
      await page.clock.setFixedTime(now)
      await page.setViewportSize({ width, height: 1000 })
      const server = await mockReportServer(page, {
        reports: [
          ...many(58, 'o', (index) => ({ summary: `Checkout timeout on node ${index}` })),
          { ...actReport, last_seen: '2026-11-27T11:59:59Z' },
        ],
      })
      await page.goto('/w/demo/monitor')
      await page.getByRole('button', { name: 'Next page' }).click()
      await expect(countText(page)).toHaveText('This list shows reports 51 to 59 of 59, newest first.')
      await countText(page).scrollIntoViewIfNeeded()
      await page.screenshot({ path: `${screenshotDir}/monitor-pages-${width}.png` })
      await page.getByRole('button', { name: 'Previous page' }).click()
      const item = card(page, actReport.id)
      server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
      server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
      await item.getByRole('button', { name: 'Confirm the link to Send invoices by email' }).click()
      await expect(item.locator('[data-report-act-stale]')).toBeVisible()
      await item.scrollIntoViewIfNeeded()
      await item.screenshot({ path: `${screenshotDir}/card-blocked-${width}.png` })
    })
  }
})

// The fix-task record takes part in the shared act state of its report.
test.describe('unknown outcomes and the fix-task record', () => {
  const linked = report('rpt-261127-m00001', {
    summary: 'Record and act',
    links: [taskLink('lnk-m1', '261120-a01', 'Send invoices by email', { evidence: [lineEvidence] })],
  })
  const address = `/w/demo/?create=true&report=${linked.id}`
  const path = `/v1/feedback-reports/${linked.id}/fix-task`
  const moveTo = (page: Page, to: string) =>
    page.evaluate((target) => {
      history.pushState({}, '', target)
      dispatchEvent(new PopStateEvent('popstate'))
    }, to)

  test('a record request waits while another act of the report is unresolved, and sends after a read answers', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [linked] })
    await page.goto('/w/demo/monitor')
    const item = card(page, linked.id)
    const confirm = item.getByRole('button', { name: 'Confirm the link to Send invoices by email' })
    await expect(confirm).toBeEnabled()
    server.actAnswers.push({ status: 500, json: { error: 'internal error' } })
    server.listAnswer = () => ({ status: 500, json: { error: 'internal error' } })
    await confirm.click()
    await expect(item.locator('[data-report-act-stale]')).toBeVisible()
    server.listAnswer = null
    await moveTo(page, address)
    await createFixTask(page, [])
    await expect(sheet(page).locator('[data-fix-task-error]')).toHaveText(
      'Another act on this report is not settled yet. Read the report again before you record the task.',
    )
    expect(server.acts.filter((act) => act.path === path)).toHaveLength(0)
    await expect(recordButton(page)).toHaveCount(0)
    await sheet(page).getByRole('button', { name: 'Read the report again' }).click()
    await recordButton(page).click()
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.acts.filter((act) => act.path === path)).toHaveLength(1)
    expect(server.created).toHaveLength(1)
  })

  test('a record request on its way blocks every card of the report until it answers', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [linked] })
    await page.goto(address)
    const held = gate()
    server.actGate = held.promise
    await createFixTask(page, [])
    await expect.poll(() => server.acts.filter((act) => act.path === path).length).toBe(1)
    await moveTo(page, '/w/demo/monitor')
    const item = card(page, linked.id)
    await expect(item).toBeVisible()
    await expect(item.getByRole('button', { name: 'Confirm the link to Send invoices by email' })).toBeDisabled()
    await expect(item.getByRole('button', { name: 'Close report' })).toBeDisabled()
    held.open()
    server.actGate = null
    await expect(item.getByRole('button', { name: 'Close report' })).toBeEnabled()
    await expect(item.locator('[data-report-fix-task]')).toContainText('Fix task:')
    expect(server.acts).toHaveLength(1)
  })
})

// Intake keys of fix tasks.
test.describe('fix-task intake keys', () => {
  const second = report('rpt-261127-d00031', {
    summary: 'Second report',
    links: [taskLink('lnk-s31', '261120-b02', 'Store invoice numbers')],
  })
  const moveTo = (page: Page, to: string) =>
    page.evaluate((target) => {
      history.pushState({}, '', target)
      dispatchEvent(new PopStateEvent('popstate'))
    }, to)

  test('a reload while intake is on its way sends the same request again and never makes a second task', async ({
    page,
  }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    const held = gate()
    server.createGate = held.promise
    await createFixTask(page, [])
    await expect.poll(() => server.intakeKeys.length).toBe(1)
    await page.reload()
    held.open()
    server.createGate = null
    await expect(sheet(page).locator('[data-fix-task-intake-sent]')).toContainText(
      'A request to create this task was sent before',
    )
    await createFixTask(page, [])
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(server.intakeKeys).toHaveLength(2)
    expect(server.intakeKeys[1]).toBe(server.intakeKeys[0])
    expect(fixPuts(server)).toEqual([{ method: 'PUT', path: fixPath, body: { task_id: '261127-fix' } }])
  })

  test('each report has its own intake key in one sheet, so a second report gets its own task', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport, second] })
    await page.goto(fixAddress)
    // Report A's record is refused, so A keeps its task and its key.
    server.actAnswers.push({ status: 503, json: { error: 'retryable' } })
    await createFixTask(page, [])
    await expect(sheet(page).locator('[data-fix-task-error]')).toBeVisible()
    await moveTo(page, `/w/demo/?create=true&report=${second.id}`)
    await expect(sheet(page).locator('[data-fix-task-summary]')).toHaveText('Second report')
    await createFixTask(page, [])
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix2')
    expect(server.intakeKeys).toHaveLength(2)
    expect(server.intakeKeys[1]).not.toBe(server.intakeKeys[0])
    expect(fixPuts(server).map((act) => [act.path, (act.body as { task_id: string }).task_id])).toEqual([
      [fixPath, '261127-fix'],
      [`/v1/feedback-reports/${second.id}/fix-task`, '261127-fix2'],
    ])
  })
})

test.describe('unknown outcomes after the page is left', () => {
  test('a report read that did not answer after the act is dropped when its page is left', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [actReport] })
    await page.goto('/w/demo/monitor')
    const item = card(page, actReport.id)
    await item.getByRole('button', { name: 'Close report' }).click()
    await item.getByRole('textbox', { name: 'Reason to close the report' }).fill('Fixed.')
    // The close is stored, its answer is lost, and the fresh read waits.
    server.actAnswers.push(() => {
      server.reports[0] = {
        ...server.reports[0],
        state: 'closed',
        closed: { at: '2026-11-27T12:00:30Z', by: 'usr_operator', reason: 'Fixed.' },
      }
      return { status: 500, json: { error: 'internal error' } }
    })
    const held = gate()
    server.listGate = held.promise
    const reads = listReads(server).length
    await item.getByRole('form', { name: 'Close the report' }).getByRole('button', { name: 'Close report' }).click()
    await expect.poll(() => listReads(server).length).toBe(reads + 1)
    // The page is left while that read waits.
    await page.evaluate(() => {
      history.pushState({}, '', '/w/demo/crates')
      dispatchEvent(new PopStateEvent('popstate'))
    })
    await expect(page.locator('[data-report-card]')).toHaveCount(0)
    await page.waitForTimeout(500)
    // Back on Monitor, the old open card never shows while the new read waits.
    await page.evaluate(() => {
      history.pushState({}, '', '/w/demo/monitor')
      dispatchEvent(new PopStateEvent('popstate'))
    })
    await expect(page.locator('[data-report-list-loading]')).toBeVisible()
    await expect(page.locator('[data-report-card]')).toHaveCount(0)
    held.open()
    server.listGate = null
    await expect(page.getByText('No open report. Reports appear here when a watcher sends one.')).toBeVisible()
    expect(server.acts).toHaveLength(1)
  })
})

test.describe('fix-task reads that overlap', () => {
  test('a fresh read overtaken by a newer read neither overwrites it nor decides the next step', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    const before = structuredClone(server.reports[0])
    // The record is stored, but its answer is lost.
    server.actAnswers.push(() => {
      server.reports[0] = { ...server.reports[0], fix_task: { id: '261127-fix', title: 'Fix the checkout timeout' } }
      return { status: 500, json: { error: 'internal error' } }
    })
    // The flow's own fresh read arrives first and holds the report from before.
    const held = gate()
    server.readGate = held.promise
    let first = true
    server.readAnswer = () => {
      if (!first) return undefined
      first = false
      return { status: 200, json: before }
    }
    const reads = reportReads(server, fixReport.id)
    await createFixTask(page, [])
    await expect.poll(() => reportReads(server, fixReport.id)).toBe(reads + 1)
    // A newer read of the report starts later and answers first.
    server.readGate = null
    await server.stream.ready()
    await expect.poll(() => reportReads(server, fixReport.id)).toBe(reads + 2)
    held.open()
    // The newer answer stands: the task is recorded, and it opens.
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
    expect(fixPuts(server)).toHaveLength(1)
  })

  test('a refresh that already shows the recorded task waits for the pending record answer', async ({ page }) => {
    const server = await mockReportServer(page, { reports: [fixReport] })
    await page.goto(fixAddress)
    await expect(description(page)).not.toHaveValue('')
    const held = gate()
    server.actGate = held.promise
    await createFixTask(page, [])
    await expect.poll(() => fixPuts(server).length).toBe(1)
    // The server committed the record; a refresh shows it before the answer.
    server.reports[0] = { ...server.reports[0], fix_task: { id: '261127-fix', title: 'Fix the checkout timeout' } }
    const reads = reportReads(server, fixReport.id)
    await server.stream.ready()
    await expect.poll(() => reportReads(server, fixReport.id)).toBeGreaterThan(reads)
    await page.waitForTimeout(300)
    await expect(page).toHaveURL(fixAddress)
    await expect(sheet(page).locator('[data-fix-task-created]')).toContainText('Conveyor records it on the report now…')
    held.open()
    server.actGate = null
    await expect(page).toHaveURL('/w/demo/tasks/261127-fix')
  })
})
