import { useEffect, useRef, useState } from 'react'
import {
  createFactoryScene,
  type SceneHandle,
  type SchemaIndex,
  type ViewSnapshot,
} from '../factory/scene'
import StationDetailOverlay, { type StationRunSummary } from '../factory/StationDetailOverlay'
import RunDrawer from '../factory/RunDrawer'
import type { AgentRun, Task } from '../types'

type Phase = 'loading' | 'ready' | 'error'

export default function Factory() {
  const containerRef = useRef<HTMLDivElement>(null)
  const overlayRef = useRef<HTMLDivElement>(null)
  const [phase, setPhase] = useState<Phase>('loading')
  const [error, setError] = useState('')
  const [schemas, setSchemas] = useState<SchemaIndex | null>(null)
  const [snapshot, setSnapshot] = useState<ViewSnapshot | null>(null)
  const [drawer, setDrawer] = useState<{ task: Task; run: AgentRun } | null>(null)

  // Fetch the predicate-field schemas once up front. Stations render their
  // filter chips from this data; mounting the Pixi scene before it's loaded
  // would leave the chips empty and force an ugly re-build.
  useEffect(() => {
    let cancelled = false
    fetch('/api/event-schemas')
      .then((r) => {
        if (!r.ok) throw new Error(`Failed to load event schemas (${r.status})`)
        return r.json() as Promise<SchemaIndex>
      })
      .then((data) => {
        if (cancelled) return
        setSchemas(data)
        setPhase('ready')
      })
      .catch((err) => {
        if (cancelled) return
        setError(err instanceof Error ? err.message : String(err))
        setPhase('error')
      })
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    if (phase !== 'ready' || !schemas) return
    const container = containerRef.current
    if (!container) return

    let sceneHandle: SceneHandle | null = null
    let unsubscribeView: (() => void) | null = null
    let cancelled = false

    createFactoryScene(container, schemas).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneHandle = scene
      // Snapshot drives overlay placement. Setting via state is the simplest
      // integration — at ~13 stations and view events only firing during pan
      // and zoom interactions, React's reconciliation cost is negligible.
      unsubscribeView = scene.onView((snap) => {
        setSnapshot(snap)
      })
    })

    return () => {
      cancelled = true
      unsubscribeView?.()
      sceneHandle?.destroy()
    }
  }, [phase, schemas])

  const nearZoom = snapshot?.nearZoom ?? false
  const stations = snapshot?.stations ?? []

  const handleOpenRun = (summary: StationRunSummary) => {
    setDrawer({ task: summary.task, run: summary.run })
  }

  return (
    <div className="-mx-8 -my-8">
      {/* overflow-hidden on the canvas container clips the HTML overlay
          layer to the viewport. Without it, zooming in pushes stations at
          the edges of the world to screen coordinates like top: 2400px,
          and those absolutely-positioned overlays balloon the document's
          scrollable area — adding vertical scrollbars to the whole page. */}
      <div
        ref={containerRef}
        className="relative w-full overflow-hidden"
        style={{ height: 'calc(100vh - 69px)' }}
      >
        {phase === 'loading' && (
          <div className="absolute inset-0 flex items-center justify-center text-[13px] text-text-tertiary">
            Loading factory…
          </div>
        )}
        {phase === 'error' && (
          <div className="absolute inset-0 flex items-center justify-center text-[13px] text-dismiss">
            {error}
          </div>
        )}
        {/* Overlay layer — positioned absolutely over the Pixi canvas.
            Individual StationDetailOverlay instances render only at near
            zoom; `pointer-events: none` on this wrapper means the canvas
            still receives drag/scroll events through the gaps between
            overlays, while each overlay's interior re-enables its own
            pointer events for clickable run rows. */}
        <div
          ref={overlayRef}
          className="absolute inset-0 overflow-hidden pointer-events-none"
          style={{ display: nearZoom ? 'block' : 'none' }}
        >
          {nearZoom &&
            stations.map((placement) => (
              <StationDetailOverlay
                key={placement.id}
                placement={placement}
                onOpenRun={handleOpenRun}
              />
            ))}
        </div>
      </div>

      <RunDrawer
        task={drawer?.task ?? null}
        run={drawer?.run ?? null}
        onClose={() => setDrawer(null)}
      />
    </div>
  )
}
