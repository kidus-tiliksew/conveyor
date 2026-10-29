import { Download, FileText, Image as ImageIcon, Paperclip, Video, X } from 'lucide-react'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { downloadArtifact, fetchArtifactObjectURL } from '../../lib/api'
import type { Artifact } from '../../lib/types'
import { absoluteTime, cn, formatBytes } from '../../lib/utils'
import { useWorkspaceSelection } from '../app-shell'
import { Button } from '../ui/button'
import { Card, CardContent, CardHeader } from '../ui/card'
import { Dialog } from '../ui/dialog'
import { SectionHeading } from './section-heading'

function isImage(contentType: string) {
  return contentType.startsWith('image/')
}

function isVideo(contentType: string) {
  return contentType.startsWith('video/')
}

// Fetch object URLs for media attachments so they can preview inline. The URLs
// are revoked on unmount.
function useMediaPreviews(attachments: Artifact[]) {
  const { workspace } = useWorkspaceSelection()
  const signature = attachments.map((attachment) => attachment.id).join(',')
  const [urls, setUrls] = useState<Record<string, string>>({})
  const [failed, setFailed] = useState<Record<string, boolean>>({})

  useEffect(() => {
    let active = true
    const created: string[] = []
    setUrls({})
    setFailed({})
    for (const attachment of attachments.filter(
      (entry) => isImage(entry.content_type) || isVideo(entry.content_type),
    )) {
      void fetchArtifactObjectURL(workspace, attachment)
        .then((url) => {
          if (!active) {
            URL.revokeObjectURL(url)
            return
          }
          created.push(url)
          setUrls((prev) => ({ ...prev, [attachment.id]: url }))
        })
        .catch(() => {
          if (active) setFailed((prev) => ({ ...prev, [attachment.id]: true }))
        })
    }
    return () => {
      active = false
      for (const url of created) URL.revokeObjectURL(url)
    }
    // Re-run only when the set of attachments changes, not on every
    // render — the array identity is otherwise stable across query refetches.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature])

  return { urls, failed }
}

// Tile track: a 7rem minimum column and the 0.75rem gap between tiles.
const tileMinWidth = 112
const tileGap = 12

// How many tiles fit on one row of the list, measured from the list's own
// width so the card fits the sheet and the full route alike.
function useRowCapacity(signature: string) {
  const listRef = useRef<HTMLUListElement>(null)
  const [capacity, setCapacity] = useState(Number.POSITIVE_INFINITY)
  useLayoutEffect(() => {
    const list = listRef.current
    if (!list) return
    const measure = () => setCapacity(Math.max(1, Math.floor((list.clientWidth + tileGap) / (tileMinWidth + tileGap))))
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(list)
    return () => observer.disconnect()
  }, [signature])
  return { listRef, capacity }
}

// The attachments section: operator-supplied task files previewed as small
// tiles directly below the spec, each expandable in-place. The card opens on
// one row; "+N more" reveals the rest in place (component-web-task-surfaces,
// Task detail composition). Omitted entirely when the task carries no
// attachments.
export function AttachmentsCard({ attachments, title = 'Attachments' }: { attachments: Artifact[]; title?: string }) {
  const { urls, failed } = useMediaPreviews(attachments)
  const signature = attachments.map((attachment) => attachment.id).join(',')
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [showAll, setShowAll] = useState(false)
  const { listRef, capacity } = useRowCapacity(signature)
  const expanded = useMemo(
    () => attachments.find((attachment) => attachment.id === expandedId) ?? null,
    [attachments, expandedId],
  )
  // A different set of attachments starts collapsed again.
  const [shownFor, setShownFor] = useState(signature)
  if (shownFor !== signature) {
    setShownFor(signature)
    setShowAll(false)
  }

  if (attachments.length === 0) return null
  const overflow = !showAll && attachments.length > capacity
  const visible = overflow ? attachments.slice(0, capacity - 1) : attachments
  const hidden = attachments.length - visible.length

  return (
    <Card className="rounded-lg">
      <CardHeader className="items-center">
        <SectionHeading count={attachments.length}>{title}</SectionHeading>
      </CardHeader>
      <CardContent className="py-4">
        <ul ref={listRef} className="grid grid-cols-[repeat(auto-fill,minmax(7rem,1fr))] gap-3">
          {visible.map((attachment) => (
            <li key={attachment.id} className="min-w-0">
              <AttachmentTile
                attachment={attachment}
                previewURL={urls[attachment.id]}
                previewFailed={failed[attachment.id]}
                onExpand={() => setExpandedId(attachment.id)}
              />
            </li>
          ))}
          {overflow && (
            <li className="min-w-0">
              <button
                type="button"
                aria-expanded={false}
                onClick={() => setShowAll(true)}
                className="grid h-full min-h-[7.5rem] w-full place-items-center rounded-md border border-dashed border-edge text-sm font-medium text-muted transition-colors hover:border-foreground/30 hover:text-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
              >
                +{hidden} more
              </button>
            </li>
          )}
        </ul>
        {showAll && attachments.length > capacity && (
          <button
            type="button"
            aria-expanded
            onClick={() => setShowAll(false)}
            className="mt-3 text-xs font-medium text-muted hover:text-foreground focus-visible:outline-2 focus-visible:outline-primary"
          >
            Show fewer
          </button>
        )}
      </CardContent>
      {expanded && (
        <AttachmentDialog
          attachment={expanded}
          previewURL={urls[expanded.id]}
          previewFailed={failed[expanded.id]}
          onClose={() => setExpandedId(null)}
        />
      )}
    </Card>
  )
}

function AttachmentTile({
  attachment,
  previewURL,
  previewFailed,
  onExpand,
}: {
  attachment: Artifact
  previewURL?: string
  previewFailed?: boolean
  onExpand: () => void
}) {
  const image = isImage(attachment.content_type)
  const video = isVideo(attachment.content_type)
  return (
    <button
      type="button"
      onClick={onExpand}
      title={attachment.name}
      aria-label={`Expand ${attachment.name}`}
      className="group flex w-full flex-col overflow-hidden rounded-md border border-border bg-surface text-left transition-colors hover:border-edge focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-primary"
    >
      <div className="grid h-20 w-full place-items-center overflow-hidden border-b border-border bg-raised/40">
        {image && previewURL ? (
          <img src={previewURL} alt={attachment.name} className="h-full w-full object-cover" />
        ) : video && previewURL ? (
          <video src={previewURL} aria-label={attachment.name} muted className="h-full w-full object-cover" />
        ) : (
          <TileIcon image={image} video={video} loading={(image || video) && !previewFailed} />
        )}
      </div>
      <div className="flex min-w-0 flex-col gap-0.5 px-2 py-1.5">
        <span className="truncate text-xs text-foreground/90">{attachment.name}</span>
        <span className="font-mono text-[10px] text-faint">{formatBytes(attachment.size_bytes)}</span>
      </div>
    </button>
  )
}

function TileIcon({ image, video, loading }: { image: boolean; video: boolean; loading: boolean }) {
  const Icon = image ? ImageIcon : video ? Video : FileText
  return <Icon className={cn('size-6 text-faint', loading && 'animate-pulse')} aria-hidden="true" />
}

function AttachmentDialog({
  attachment,
  previewURL,
  previewFailed,
  onClose,
}: {
  attachment: Artifact
  previewURL?: string
  previewFailed?: boolean
  onClose: () => void
}) {
  const { workspace } = useWorkspaceSelection()
  const image = isImage(attachment.content_type)
  const video = isVideo(attachment.content_type)
  const showImage = image && !!previewURL
  const showVideo = video && !!previewURL
  return (
    <Dialog onClose={onClose} label={attachment.name} className={cn((showImage || showVideo) && 'max-w-3xl')}>
      <header className="flex items-center gap-3 border-b border-border px-4 py-3">
        <Paperclip className="size-4 shrink-0 text-faint" aria-hidden="true" />
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium text-foreground">{attachment.name}</p>
          <p className="font-mono text-[11px] text-faint">
            {attachment.content_type || 'unknown'} · {formatBytes(attachment.size_bytes)} ·{' '}
            {absoluteTime(attachment.created_at)}
          </p>
        </div>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void downloadArtifact(workspace, attachment).catch(() => {})}
        >
          <Download />
          Download
        </Button>
        <Button variant="ghost" size="icon" aria-label="Close preview" onClick={onClose}>
          <X />
        </Button>
      </header>
      <div className="grid place-items-center p-4">
        {showImage ? (
          <img src={previewURL} alt={attachment.name} className="max-h-[70vh] w-auto rounded-md object-contain" />
        ) : showVideo ? (
          /* biome-ignore lint/a11y/useMediaCaption: uploaded evidence may not include a caption track; download remains available */
          <video
            src={previewURL}
            aria-label={attachment.name}
            controls
            preload="metadata"
            className="max-h-[70vh] w-full rounded-md"
          />
        ) : (
          <div className="flex flex-col items-center gap-3 py-8 text-center">
            <FileText className="size-10 text-faint" aria-hidden="true" />
            <p className="max-w-sm text-sm text-muted">
              {(image || video) && previewFailed
                ? 'This evidence could not be loaded for preview. Use the authorized download instead.'
                : 'No inline preview for this file type.'}
            </p>
          </div>
        )}
      </div>
    </Dialog>
  )
}
