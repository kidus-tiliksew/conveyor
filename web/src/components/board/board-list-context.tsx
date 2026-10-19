import { Link } from '@tanstack/react-router'
import { FileCode2, Workflow } from 'lucide-react'
import {
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
  type Ref,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from 'react'
import type { Task } from '../../lib/types'

type AttachedDocument = {
  kind: 'requirement' | 'system_design'
  id: string
  label: string
}

const popoverWidth = 288
const popoverGap = 4

// A Board list row's Context cell (req-task-centric-operations-view AC-1.3;
// component-web-task-surfaces, Board and Tasks list). The first served
// requirement, or the first governing System Design when none is served, is
// one neutral chip; "+N" counts the rest. Hover or keyboard focus anywhere in
// the cell opens a popover that lists and links every attached document. The
// popover holds links, so it is a group the pointer and focus can move into,
// never a tooltip that closes as soon as focus leaves its trigger. It is
// positioned against the viewport so the table's own scroller cannot clip it.
export function BoardListContext({ task }: { task: Task }) {
  const documents: AttachedDocument[] = [
    ...(task.context?.requirements ?? []).map((requirement) => ({
      kind: 'requirement' as const,
      id: requirement.id,
      label: `${requirement.title} v${requirement.version}`,
    })),
    ...(task.context?.designs ?? []).map((design) => ({
      kind: 'system_design' as const,
      id: design.id,
      label: `${design.title} v${design.version}`,
    })),
  ]
  const id = useId()
  const root = useRef<HTMLDivElement>(null)
  const chip = useRef<HTMLAnchorElement>(null)
  const popover = useRef<HTMLDivElement>(null)
  const hovered = useRef(false)
  // Escape dismisses until focus leaves the cell or the pointer comes back,
  // so returning focus to the chip does not reopen what was just closed.
  const dismissed = useRef(false)
  const [open, setOpen] = useState(false)
  const [position, setPosition] = useState<{ left: number; top: number; above: boolean } | null>(null)

  // Place the popover under the cell, flipped above it near the viewport's
  // bottom and shifted to stay inside its width. Its box starts exactly at
  // the cell's edge: the visible panel sits behind a transparent bridge of
  // popoverGap pixels on the side facing the cell, so a pointer moving from
  // the chip to a document link never leaves the cell and its popover.
  useLayoutEffect(() => {
    if (!open) return
    const place = () => {
      const cell = root.current?.getBoundingClientRect()
      if (!cell) return
      const width = Math.min(popoverWidth, window.innerWidth - 16)
      const height = popover.current?.offsetHeight ?? 0
      const left = Math.max(8, Math.min(cell.left, window.innerWidth - 8 - width))
      const above = cell.bottom + height > window.innerHeight - 8 && cell.top - height >= 8
      setPosition({ left, top: above ? cell.top - height : cell.bottom, above })
    }
    place()
    window.addEventListener('resize', place)
    document.addEventListener('scroll', place, true)
    return () => {
      window.removeEventListener('resize', place)
      document.removeEventListener('scroll', place, true)
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const outside = (event: PointerEvent) => {
      if (event.target instanceof Node && !root.current?.contains(event.target)) {
        hovered.current = false
        setOpen(false)
      }
    }
    document.addEventListener('pointerdown', outside, true)
    return () => document.removeEventListener('pointerdown', outside, true)
  }, [open])

  if (documents.length === 0) return <span className="text-xs text-faint">No context</span>
  const [first] = documents
  const more = documents.length - 1

  const onKeyDown = (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || !open) return
    event.preventDefault()
    event.stopPropagation()
    dismissed.current = true
    setOpen(false)
    chip.current?.focus()
  }

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: The cell tracks hover and focus across its chip and the popover of links it opens.
    <div
      ref={root}
      className="flex min-w-0 items-center gap-1.5"
      onPointerEnter={(event) => {
        if (event.pointerType === 'touch') return
        hovered.current = true
        dismissed.current = false
        setOpen(true)
      }}
      onPointerLeave={() => {
        hovered.current = false
        if (!root.current?.contains(document.activeElement)) setOpen(false)
      }}
      onFocus={() => {
        if (!dismissed.current) setOpen(true)
      }}
      onBlur={(event) => {
        if (event.relatedTarget instanceof Node && root.current?.contains(event.relatedTarget)) return
        dismissed.current = false
        if (!hovered.current) setOpen(false)
      }}
      onKeyDown={onKeyDown}
    >
      <DocumentLink
        ref={chip}
        document={first}
        aria-describedby={`${id}-popover`}
        className="inline-flex min-w-0 max-w-full items-center rounded-md bg-raised px-1.5 py-0.5 text-[11px] leading-4 text-muted hover:text-foreground"
      >
        <span className="truncate">{first.label}</span>
      </DocumentLink>
      {more > 0 && (
        <span className="shrink-0 text-[11px] text-faint" title={`${more} more attached`}>
          +{more}
        </span>
      )}
      {open && (
        // biome-ignore lint/a11y/useSemanticElements: A fieldset groups form controls; this is a labelled group of document links.
        <div
          ref={popover}
          id={`${id}-popover`}
          role="group"
          aria-label="Attached context"
          data-placement={position?.above ? 'above' : 'below'}
          className="fixed z-40"
          style={{
            width: `min(${popoverWidth}px, calc(100vw - 16px))`,
            left: position?.left ?? 0,
            top: position?.top ?? 0,
            paddingTop: position?.above ? 0 : popoverGap,
            paddingBottom: position?.above ? popoverGap : 0,
            visibility: position ? 'visible' : 'hidden',
          }}
        >
          <div data-popover-panel className="rounded-lg border border-border bg-card p-1.5 shadow-lg">
            {documents.map((document) => (
              <DocumentLink
                key={`${document.kind}:${document.id}`}
                document={document}
                className="flex min-w-0 items-center gap-2 rounded-md px-2 py-1.5 text-xs text-muted hover:bg-raised hover:text-foreground focus-visible:bg-raised"
              >
                {document.kind === 'requirement' ? (
                  <Workflow className="size-3.5 shrink-0 text-faint" aria-hidden="true" />
                ) : (
                  <FileCode2 className="size-3.5 shrink-0 text-faint" aria-hidden="true" />
                )}
                <span className="sr-only">{document.kind === 'requirement' ? 'Requirement: ' : 'System Design: '}</span>
                <span className="min-w-0 truncate">{document.label}</span>
              </DocumentLink>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

// Every entry opens the document through its existing route, exactly as the
// requirement and System Design pages link it elsewhere.
function DocumentLink({
  document,
  className,
  children,
  ref,
  ...rest
}: {
  document: AttachedDocument
  className: string
  children: ReactNode
  ref?: Ref<HTMLAnchorElement>
  'aria-describedby'?: string
}) {
  const props = {
    ref,
    className,
    title: document.label,
    onClick: (event: MouseEvent) => event.stopPropagation(),
    ...rest,
  }
  return document.kind === 'requirement' ? (
    <Link to="/requirements" search={{ requirement: document.id }} {...props}>
      {children}
    </Link>
  ) : (
    <Link to="/system-design" search={{ document: document.id }} {...props}>
      {children}
    </Link>
  )
}
