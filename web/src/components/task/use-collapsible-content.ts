import { useLayoutEffect, useRef, useState } from 'react'

// Collapsing earns its toggle only when it hides a real amount of text. Below
// this, the toggle and fade cost more room than they save, so the text shows
// whole.
const minimumHidden = 120

// A clean cut this far above the limit would hide too much of the preview, so
// a block taller than the rest of the preview is cut through instead.
const shortestCleanCut = 0.5

const headings = new Set(['H1', 'H2', 'H3', 'H4', 'H5', 'H6'])

// The flow's top-level blocks: a prose root's children, or a wrapper's children
// with any prose inside it opened up. A section wrapper that is not prose, such
// as the plan's acceptance criteria, opens up too, so the preview can end
// between its heading and its list entries.
function blocksOf(root: Element) {
  if (root.classList.contains('markdown')) return [...root.children]
  return [...root.children].flatMap((child) =>
    child.classList.contains('markdown') || (child.tagName === 'DIV' && child.children.length > 0)
      ? [...child.children]
      : [child],
  )
}

// A block, or a list's entries one by one: the places a preview may end.
function stopsOf(blocks: Element[]) {
  return blocks.flatMap((block) => (block.tagName === 'UL' || block.tagName === 'OL' ? [...block.children] : [block]))
}

// Where the preview ends: the last block boundary at or above the limit, never
// straight after a heading whose text is cut away. A list can end between its
// items. A block too tall for that keeps the plain limit, marked as not clean
// so the caller can fade through its text, unless `reach` allows ending at the
// first boundary below the limit instead.
function previewHeight(root: Element, limit: number, reach: number, total: number) {
  const top = root.getBoundingClientRect().top
  const bottomOf = (element: Element) => element.getBoundingClientRect().bottom - top
  const blocks = blocksOf(root)
  let last = -1
  let cut = 0
  let next: Element | undefined
  for (let index = 0; index < blocks.length; index += 1) {
    const block = blocks[index]
    if (bottomOf(block) <= limit) {
      last = index
      cut = bottomOf(block)
      continue
    }
    next = block
    if (block.tagName === 'UL' || block.tagName === 'OL') {
      const fitting = [...block.children].filter((entry) => bottomOf(entry) <= limit)
      const final = fitting.at(-1)
      if (final) {
        cut = bottomOf(final)
        last = index
        next = final.nextElementSibling ?? block
      }
    }
    break
  }
  while (last >= 0 && headings.has(blocks[last].tagName)) {
    next = blocks[last]
    last -= 1
    cut = last >= 0 ? bottomOf(blocks[last]) : 0
  }
  // Keep the block's own bottom margin, up to where the next one starts.
  const withMargin = (end: number, following: Element | undefined) => {
    const gap = following ? following.getBoundingClientRect().top - top - end : 8
    return Math.ceil(end + Math.max(0, Math.min(gap, 8)))
  }
  if (cut >= limit * shortestCleanCut) return { height: withMargin(cut, next), clean: true }
  // A plain cut here can land in the space around a heading and show a faded
  // band with no text in it. With `reach`, the preview runs on to the first
  // boundary below the limit that does not follow a heading, when that boundary
  // is near and still leaves enough hidden.
  const furthest = Math.min(limit + reach, total - minimumHidden)
  const stops = stopsOf(blocks)
  for (let index = 0; index < stops.length; index += 1) {
    const stop = stops[index]
    const end = bottomOf(stop)
    if (end <= limit || headings.has(stop.tagName)) continue
    if (end > furthest) break
    return { height: withMargin(end, stops[index + 1]), clean: true }
  }
  return { height: limit, clean: false }
}

// Measures one collapsible region. The viewport's first child holds the content
// at its natural height, so the reading does not depend on the clamp. `reach`
// is how far below the limit the preview may run to end at a block boundary
// when the boundary above the limit would leave too short a preview.
export function useCollapsibleContent(limit: number, expanded: boolean, enabled = true, reach = 0) {
  const viewportRef = useRef<HTMLDivElement>(null)
  const [preview, setPreview] = useState<{ height: number; clean: boolean } | null>(null)

  // The observer re-measures whenever the content or the width changes its
  // height; the preview itself never changes the content's height.
  useLayoutEffect(() => {
    const root = viewportRef.current?.firstElementChild
    if (!enabled || !root) {
      setPreview(null)
      return
    }
    const measure = () => {
      const total = root.getBoundingClientRect().height
      const next = total - limit > minimumHidden ? previewHeight(root, limit, reach, total) : null
      setPreview((current) => (current?.height === next?.height && current?.clean === next?.clean ? current : next))
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(root)
    return () => observer.disconnect()
  }, [enabled, limit, reach])

  const clamped = preview != null && !expanded
  return {
    viewportRef,
    collapsible: preview != null,
    clamped,
    // A clean cut ends in the block's blank margin, so its fade stays clear of
    // the last line of text.
    fadeClass: preview?.clean ? 'h-2' : 'h-8',
    viewportStyle: clamped ? { maxHeight: preview.height, overflow: 'hidden' as const } : undefined,
  }
}
