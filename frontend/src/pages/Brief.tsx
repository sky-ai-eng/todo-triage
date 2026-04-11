export default function Brief() {
  return (
    <div className="max-w-2xl mx-auto space-y-6">
      <h1 className="text-[22px] font-semibold text-text-primary tracking-tight">Morning Brief</h1>
      <div className="backdrop-blur-xl bg-surface-raised border border-border-glass rounded-2xl p-8 shadow-sm shadow-black/[0.03]">
        <div className="flex flex-col items-center text-center py-6 gap-4">
          <div className="w-12 h-12 rounded-full bg-accent-soft flex items-center justify-center">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" className="text-accent">
              <path
                d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm-1 15v-4H7l5-7v4h4l-5 7z"
                fill="currentColor"
                opacity="0.7"
              />
            </svg>
          </div>
          <div>
            <p className="text-[14px] text-text-secondary leading-relaxed">
              Your daily brief will appear here once tasks have been ingested.
            </p>
            <p className="text-[12px] text-text-tertiary mt-1">
              Connect your accounts in Settings to get started.
            </p>
          </div>
        </div>
      </div>
    </div>
  )
}
