// Frontend-only theme: 'light' | 'dark' | 'auto'.
// Stored in localStorage; applied by toggling `.dark` on <html>.
// The inline script in index.html applies the saved choice before
// React mounts so there's no flash on load.

export type ThemeMode = 'light' | 'dark' | 'auto'

const STORAGE_KEY = 'tf-theme'

export function getStoredTheme(): ThemeMode {
  const v = localStorage.getItem(STORAGE_KEY)
  return v === 'light' || v === 'dark' || v === 'auto' ? v : 'auto'
}

function resolve(mode: ThemeMode): 'light' | 'dark' {
  if (mode === 'auto') {
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  }
  return mode
}

export function applyTheme(mode: ThemeMode) {
  const effective = resolve(mode)
  document.documentElement.classList.toggle('dark', effective === 'dark')
  document.documentElement.style.colorScheme = effective
}

export function setTheme(mode: ThemeMode) {
  localStorage.setItem(STORAGE_KEY, mode)
  applyTheme(mode)
}

// Re-apply when the OS preference changes, but only while in 'auto'.
let mediaListenerAttached = false
export function watchSystemTheme() {
  if (mediaListenerAttached) return
  mediaListenerAttached = true
  const mql = window.matchMedia('(prefers-color-scheme: dark)')
  mql.addEventListener('change', () => {
    if (getStoredTheme() === 'auto') applyTheme('auto')
  })
}

// React hook: subscribe to the effective theme. Components that need to
// pick colors at render time (e.g. motion useTransform with inline style,
// which bypasses CSS class overrides) should consume this.
import { useSyncExternalStore } from 'react'

const themeSubscribers = new Set<() => void>()
let observer: MutationObserver | null = null

function ensureObserver() {
  if (observer) return
  observer = new MutationObserver(() => {
    themeSubscribers.forEach((cb) => cb())
  })
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
}

function subscribe(cb: () => void) {
  ensureObserver()
  themeSubscribers.add(cb)
  return () => {
    themeSubscribers.delete(cb)
  }
}

function getSnapshot(): 'light' | 'dark' {
  return document.documentElement.classList.contains('dark') ? 'dark' : 'light'
}

export function useEffectiveTheme(): 'light' | 'dark' {
  return useSyncExternalStore(subscribe, getSnapshot, () => 'light')
}
