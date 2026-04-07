import { ApolloLink } from '@apollo/client/core'
import { useAuthStore } from '@/store/auth'

// AuthLink attaches the Bearer token to every GraphQL request.
// Reads the access token from Zustand store.
// If no token → sends the request without Authorization header.
// The backend will return 401 for protected operations.
//
// Special case: initiateAuth mutation is public.
// Member 4's route handler reads the X-Public-Operation header
// to skip auth middleware for this specific mutation.
// Without this header, the backend returns 401 on initiateAuth
// because auth middleware runs and finds no JWT.
export const authLink = new ApolloLink((operation, forward) => {
  const token = useAuthStore.getState().accessToken

  // Set the X-Public-Operation header for public mutations
  // so Member 4's routeGraphQL handler bypasses auth middleware.
  const isPublicOperation = operation.operationName === 'InitiateAuth'

  operation.setContext(({ headers = {} }) => ({
    headers: {
      ...headers,
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(isPublicOperation
        ? { 'X-Public-Operation': 'initiateAuth' }
        : {}),
    },
  }))

  return forward(operation)
})
