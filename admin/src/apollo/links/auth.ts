import { ApolloLink } from '@apollo/client/core'
import { useAuthStore } from '@/store/auth'

// Public operations that bypass auth middleware
const PUBLIC_OPERATIONS = [
  'InitiateAuth',
  'LookupWorkspace',
  'LookupWorkspacesByEmail',
]

// AuthLink attaches the Bearer token to every GraphQL request.
// Reads the access token from Zustand store.
// If no token → sends the request without Authorization header.
// The backend will return 401 for protected operations.
//
// Special case: public operations (InitiateAuth, LookupWorkspace) bypass auth middleware
// using the X-Public-Operation header.
export const authLink = new ApolloLink((operation, forward) => {
  const token = useAuthStore.getState().accessToken
  const opName = operation.operationName || ''

  // Check if this is a public operation
  const isPublicOperation = PUBLIC_OPERATIONS.includes(opName)

  operation.setContext(({ headers = {} }) => ({
    headers: {
      ...headers,
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(isPublicOperation
        ? { 'X-Public-Operation': opName }
        : {}),
    },
  }))

  return forward(operation)
})
