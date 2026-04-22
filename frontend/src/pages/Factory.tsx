import { useEffect, useRef, useState } from 'react'
import { createFactoryScene, type SchemaIndex } from '../factory/scene'

type Phase = 'loading' | 'ready' | 'error'

export default function Factory() {
  const containerRef = useRef<HTMLDivElement>(null)
  const [phase, setPhase] = useState<Phase>('loading')
  const [error, setError] = useState('')
  const [schemas, setSchemas] = useState<SchemaIndex | null>(null)

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

    let destroy: (() => void) | null = null
    let cancelled = false

    createFactoryScene(container, schemas).then((scene) => {
      if (cancelled) {
        scene.destroy()
        return
      }
      destroy = scene.destroy
    })

    return () => {
      cancelled = true
      destroy?.()
    }
  }, [phase, schemas])

  return (
    <div className="-mx-8 -my-8">
      <div ref={containerRef} className="relative w-full" style={{ height: 'calc(100vh - 69px)' }}>
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
      </div>
    </div>
  )
}
