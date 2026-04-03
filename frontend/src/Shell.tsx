import { NavLink, Outlet } from 'react-router-dom'

const navItems = [
  { to: '/', label: 'Triage' },
  { to: '/board', label: 'Board' },
  { to: '/brief', label: 'Brief' },
  { to: '/settings', label: 'Settings' },
]

export default function Shell() {
  return (
    <div className="min-h-screen bg-surface text-text-primary">
      <nav className="sticky top-0 z-50 backdrop-blur-xl bg-surface-overlay border-b border-border-subtle px-8 py-4 flex items-center gap-10">
        <span className="text-base font-semibold tracking-tight text-text-primary">
          Todo Tinder
        </span>
        <div className="flex gap-1">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                `text-[13px] font-medium px-4 py-1.5 rounded-full transition-all duration-200 ${
                  isActive
                    ? 'bg-accent-soft text-accent'
                    : 'text-text-tertiary hover:text-text-secondary hover:bg-black/[0.03]'
                }`
              }
            >
              {item.label}
            </NavLink>
          ))}
        </div>
      </nav>
      <main className="px-8 py-8">
        <Outlet />
      </main>
    </div>
  )
}
