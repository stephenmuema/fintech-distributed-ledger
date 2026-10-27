import { createContext, useContext } from 'react'
import type { Session } from './api'

/** The signed-in operator, provided by ConsoleLayout to every console page. */
export const SessionContext = createContext<Session | undefined>(undefined)

export function useSession(): Session {
  const session = useContext(SessionContext)
  if (!session) throw new Error('useSession outside the signed-in console')
  return session
}
