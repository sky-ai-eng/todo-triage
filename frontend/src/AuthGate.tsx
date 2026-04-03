import { Navigate } from 'react-router-dom'
import { useAuthStatus } from './hooks/useAuthStatus'

export default function AuthGate({ children }: { children: React.ReactNode }) {
  const { configured, loading } = useAuthStatus()

  if (loading) {
    return (
      <div className="min-h-screen bg-surface flex items-center justify-center">
        <p className="text-text-tertiary text-sm">Loading...</p>
      </div>
    )
  }

  if (!configured) {
    return <Navigate to="/setup" replace />
  }

  return <>{children}</>
}
