import { expect, type Locator } from '@playwright/test'

// What a next action or a pending document proposal keeps off task detail and
// the Inbox (req-operator-next-action AC-1.9; component-next-action invariant
// 41): the actor as text, the question, the if-ignored sentence, and a list of
// the statement's identifiers and reason codes. The stubs still send every
// field, because the server and the CLI and MCP reads keep them, so these
// checks read the rendered DOM. They read hidden text and the attributes that
// carry a tooltip or an accessible name or description too, so a field cannot
// hide behind a disclosure, a menu, or a screen-reader-only node.
export type NextActionOmissions = {
  // The statement's actor. No element may print it as its whole text.
  actor?: string
  // Server sentences that must not appear anywhere in the surface.
  sentences?: (string | undefined)[]
  // Identifiers and reason codes from `details` and `current_order` that must
  // not appear anywhere in the surface.
  identifiers?: (string | undefined)[]
}

const fixedPhrases = ['If ignored', 'If nobody acts', 'If you do nothing', 'Show IDs', 'Hide IDs']

const removedMarkers = [
  '[data-actor]',
  '[data-if-ignored]',
  '[data-next-action-ids]',
  '[data-next-action-id]',
  '[data-decision-question]',
  '[data-footer-strip]',
]

export async function expectNoNextActionOmissions(surface: Locator, omissions: NextActionOmissions = {}) {
  await expect(surface).toBeVisible()
  for (const marker of removedMarkers) await expect(surface.locator(marker)).toHaveCount(0)
  const forbidden = [...fixedPhrases, ...(omissions.sentences ?? []), ...(omissions.identifiers ?? [])].filter(
    (value): value is string => Boolean(value?.trim()),
  )
  const found = await surface.evaluate(
    (root, { forbidden, actor }) => {
      const hits: string[] = []
      // Hidden and screen-reader-only text counts: textContent reads both.
      const text = root.textContent ?? ''
      for (const value of forbidden) if (text.includes(value)) hits.push(`text: ${value}`)
      const elements = [root, ...Array.from(root.querySelectorAll('*'))]
      for (const element of elements) {
        for (const name of ['title', 'aria-label', 'aria-description', 'data-tooltip']) {
          const value = element.getAttribute(name)
          if (!value) continue
          for (const phrase of forbidden) if (value.includes(phrase)) hits.push(`${name}: ${phrase}`)
          if (
            actor &&
            value
              .toLowerCase()
              .split(/[\s,]+/)
              .includes(actor.toLowerCase())
          )
            hits.push(`${name} names the actor: ${value}`)
        }
        // An accessible description resolves through aria-describedby, which
        // can point outside the surface.
        for (const id of (element.getAttribute('aria-describedby') ?? '').split(/\s+/).filter(Boolean)) {
          const description = document.getElementById(id)?.textContent ?? ''
          for (const phrase of forbidden) if (description.includes(phrase)) hits.push(`description: ${phrase}`)
        }
        // The actor as a standalone word, such as the retired "operator" span.
        if (actor && element.children.length === 0) {
          const own = (element.textContent ?? '').trim().toLowerCase()
          if (own === actor.toLowerCase()) hits.push(`actor text: ${own}`)
        }
      }
      return hits
    },
    { forbidden, actor: omissions.actor ?? '' },
  )
  expect(found, 'a field AC-1.9 withholds reached the dashboard').toEqual([])
}
