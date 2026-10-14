import { createApiClient, type ApiClient } from './client'
import { useSessionStore } from '../stores/session'

/** The API client for the signed-in session (it reads the token from the session store). */
export function useApi(): ApiClient {
  const session = useSessionStore()
  return createApiClient({ getToken: () => session.token })
}
