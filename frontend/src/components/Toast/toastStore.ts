// A tiny in-module store for toast notifications — deliberately NOT a React
// hook, so non-React callers (like the websocket dispatch path) can fire a
// toast without hook ceremony. Components subscribe via useToast() when they
// need live state; most callers just do `toast.error("...")` and move on.

export type ToastLevel = 'info' | 'success' | 'warning' | 'error'

export interface ToastItem {
  id: string
  level: ToastLevel
  title?: string
  body: string
}

type Listener = (items: ToastItem[]) => void

let state: ToastItem[] = []
const listeners = new Set<Listener>()

function emit() {
  for (const l of listeners) l(state)
}

function nextID(): string {
  // crypto.randomUUID is stable across our target browsers; the fallback
  // keeps storybook/jsdom environments without crypto happy.
  if (typeof crypto !== 'undefined' && crypto.randomUUID) {
    return crypto.randomUUID()
  }
  return `toast-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
}

function push(item: Omit<ToastItem, 'id'> & { id?: string }): string {
  const id = item.id ?? nextID()
  // Dedup by id — if the backend sends the same toast twice (e.g. reconnect
  // replay), we keep the first. Callers relying on dedup should pass a
  // stable id; otherwise each call creates a new toast.
  if (state.some((t) => t.id === id)) return id
  state = [...state, { ...item, id }]
  emit()
  return id
}

function dismiss(id: string) {
  state = state.filter((t) => t.id !== id)
  emit()
}

function subscribe(listener: Listener): () => void {
  listeners.add(listener)
  listener(state)
  return () => {
    listeners.delete(listener)
  }
}

export const toastStore = {
  push,
  dismiss,
  subscribe,
  getState: () => state,
}

// Convenience API — the 99% surface. Body first because most toasts are
// body-only; optional title is the second arg for the rarer titled case.
export const toast = {
  info: (body: string, title?: string) => push({ level: 'info', body, title }),
  success: (body: string, title?: string) => push({ level: 'success', body, title }),
  warning: (body: string, title?: string) => push({ level: 'warning', body, title }),
  error: (body: string, title?: string) => push({ level: 'error', body, title }),
  dismiss,
  push,
}
