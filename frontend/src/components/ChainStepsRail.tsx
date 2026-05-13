import { Link } from 'react-router-dom'
import type { AgentRun } from '../types'
import { isActiveStatus, isFailedStatus, statusLabel } from '../lib/runStatus'

interface Props {
  steps: AgentRun[]
  currentRunID: string
  currentStepIndex?: number
  linkable?: boolean
}

// Horizontal step indicator for chain runs. When `linkable` is true each
// step links to its own detail page.
export default function ChainStepsRail({ steps, currentRunID, currentStepIndex, linkable }: Props) {
  const fallbackIndex = steps.findIndex((s) => s.ID === currentRunID)
  const currentIndex = currentStepIndex ?? fallbackIndex

  return (
    <div>
      <div className="flex items-center gap-2">
        {steps.map((step, i) => {
          const isActive = isActiveStatus(step.Status)
          const isDone = step.Status === 'completed'
          const isFailed = isFailedStatus(step.Status)
          const isCurrent = step.ID === currentRunID || i === currentStepIndex
          const node = (
            <div className="relative shrink-0" title={`Step ${i + 1}: ${statusLabel(step.Status)}`}>
              {isActive && (
                <svg
                  className="absolute -inset-1 animate-spin text-delegate"
                  viewBox="0 0 32 32"
                  fill="none"
                  aria-hidden
                >
                  <circle
                    cx="16"
                    cy="16"
                    r="14"
                    stroke="currentColor"
                    strokeOpacity="0.2"
                    strokeWidth="2"
                  />
                  <path
                    d="M16 2 a14 14 0 0 1 14 14"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                  />
                </svg>
              )}
              <div
                className={`flex items-center justify-center rounded-full text-[10px] font-bold leading-none transition-all ${
                  isCurrent && !isActive ? 'w-6 h-6 ring-2 ring-accent/30' : 'w-5 h-5'
                } ${
                  isDone
                    ? 'bg-claim/15 text-claim'
                    : isActive
                      ? 'bg-delegate/15 text-delegate'
                      : isFailed
                        ? 'bg-dismiss/15 text-dismiss'
                        : 'bg-black/[0.04] text-text-tertiary'
                }`}
              >
                {isDone ? '✓' : isFailed ? '✗' : i + 1}
              </div>
            </div>
          )
          return (
            <div key={step.ID} className="flex items-center gap-2 first:flex-none flex-1 min-w-0">
              {i > 0 && (
                <div
                  className={`h-0.5 flex-1 rounded-full ${
                    isDone || isActive
                      ? 'bg-accent/40'
                      : isFailed
                        ? 'bg-dismiss/40'
                        : 'bg-border-subtle'
                  }`}
                />
              )}
              {linkable && !step.ID.startsWith('__pending-') ? (
                <Link to={`/board/runs/${step.ID}`}>{node}</Link>
              ) : (
                node
              )}
            </div>
          )
        })}
      </div>
      {currentIndex >= 0 && (
        <div className="mt-1.5 text-[10px] text-text-tertiary">
          Step {currentIndex + 1} of {steps.length}
        </div>
      )}
    </div>
  )
}
