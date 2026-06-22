import { ApolloLink } from '@apollo/client/core'
import { useAuthStore } from '@/store/auth'

// AuthLink attaches the Bearer token to every GraphQL request when one exists.
//
// Public operations (login redirect, workspace lookup) are issued before a token
// exists and simply go out without an Authorization header. The server decides
// public-vs-protected by parsing the request itself (see routeGraphQL in the
// controller) — there is no longer an X-Public-Operation header or a client-side
// public-operation list to keep in sync.
export const authLink = new ApolloLink((operation, forward) => {
  const token = useAuthStore.getState().accessToken

  operation.setContext(({ headers = {} }) => ({
    headers: {
      ...headers,
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
  }))

  return forward(operation)
})
