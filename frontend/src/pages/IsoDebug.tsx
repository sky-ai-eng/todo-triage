import { useEffect, useRef, useState } from 'react'
import {
  createIsoDebugScene,
  type ClickedStationInfo,
  type IsoDebugSceneHandle,
} from '../factory/iso-debug'

// Visual sandbox for the 3D rewrite (SKY-196 / SKY-197). Mounts a
// Babylon scene with a floor grid + one station. Default camera is
// top-down ortho. Babylon's ArcRotateCamera handles input directly:
// LMB-drag = orbit, RMB-drag (or ctrl+LMB) = pan, wheel = zoom. The
// Reset button snaps back to the initial top-down view.

export default function IsoDebug() {
  const containerRef = useRef<HTMLDivElement>(null)
  const sceneRef = useRef<IsoDebugSceneHandle | null>(null)
  const [picked, setPicked] = useState<ClickedStationInfo | null>(null)

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let cancelled = false
    let unsubClick: (() => void) | null = null
    createIsoDebugScene(container).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      sceneRef.current = scene
      unsubClick = scene.onStationClick(setPicked)
    })
    return () => {
      cancelled = true
      unsubClick?.()
      sceneRef.current?.destroy()
      sceneRef.current = null
    }
  }, [])

  // Esc closes the drawer — common video-game-y dismiss gesture.
  useEffect(() => {
    if (!picked) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setPicked(null)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [picked])

  return (
    <div className="relative -mx-8 -my-8">
      <div
        ref={containerRef}
        className="relative w-full overflow-hidden"
        style={{ height: 'calc(100vh - 69px)' }}
      />
      <button
        type="button"
        onClick={() => sceneRef.current?.resetView()}
        className="absolute bottom-4 right-4 rounded-md bg-white/92 px-3 py-2 text-[11px] font-semibold text-text-primary shadow transition hover:bg-white"
      >
        Reset view
      </button>
      <StationDrawer info={picked} onClose={() => setPicked(null)} />
    </div>
  )
}

// Bottom slide-up sheet — Halo-style HUD pane that takes the lower
// third of the viewport when a station is clicked, leaving the
// factory visible above. v1 placeholder content; the real run /
// queue lists land when the data layer is wired in.
function StationDrawer({
  info,
  onClose,
}: {
  info: ClickedStationInfo | null
  onClose: () => void
}) {
  const open = info != null
  return (
    <div
      className={`pointer-events-none absolute inset-x-0 bottom-0 z-40 transition-transform duration-300 ease-out ${
        open ? 'translate-y-0' : 'translate-y-full'
      }`}
      style={{ height: '38vh' }}
      aria-hidden={!open}
    >
      <div className="pointer-events-auto h-full bg-surface-raised/95 backdrop-blur-xl border-t border-border-glass shadow-2xl shadow-black/[0.12] flex flex-col">
        <div className="flex items-center justify-between px-6 py-4 border-b border-border-subtle">
          <div className="flex items-baseline gap-3">
            <h2 className="text-lg font-semibold text-text-primary tracking-tight">
              {info?.label ?? '—'}
            </h2>
            <span className="text-[11px] uppercase tracking-wider text-text-tertiary font-mono">
              station
            </span>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-md px-3 py-1.5 text-[11px] font-semibold text-text-secondary hover:bg-black/5 transition"
          >
            Close (esc)
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-6 py-4 grid grid-cols-2 gap-6">
          <DrawerSection title="Active runs" count={info?.runCount ?? 0} accent="#7aa3ff">
            <div className="text-[12px] text-text-tertiary italic">
              Run list lands when run-binding ships. For now: {info?.runCount ?? 0} chip
              {info?.runCount === 1 ? '' : 's'} on the work tray.
            </div>
          </DrawerSection>
          <DrawerSection title="Queued" count={info?.queuedCount ?? 0} accent="#ff9c3a">
            <div className="text-[12px] text-text-tertiary italic">
              Entity list lands with the data layer. {info?.queuedCount ?? 0} chip
              {info?.queuedCount === 1 ? '' : 's'} on the intake tray.
            </div>
          </DrawerSection>
        </div>
      </div>
    </div>
  )
}

function DrawerSection({
  title,
  count,
  accent,
  children,
}: {
  title: string
  count: number
  accent: string
  children: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-2">
      <header className="flex items-baseline gap-2">
        <span
          aria-hidden
          className="inline-block h-2 w-2 rounded-full"
          style={{ background: accent }}
        />
        <span className="text-[11px] uppercase tracking-wider text-text-secondary font-semibold">
          {title}
        </span>
        <span className="ml-auto font-mono text-[12px] text-text-primary">{count}</span>
      </header>
      <div className="rounded-lg border border-border-subtle bg-white/40 p-3 min-h-[80px]">
        {children}
      </div>
    </section>
  )
}
