import {
  ApolloClient,
  InMemoryCache,
  HttpLink,
  ApolloLink,
} from '@apollo/client/core'
import { authLink } from './links/auth'
import { errorLink } from './links/error'

const httpLink = new HttpLink({
  uri: '/graphql',
  // credentials: 'same-origin' so the browser sends
  // the httpOnly refresh cookie on same-origin requests.
  // For the /graphql endpoint itself, we use Bearer tokens.
  credentials: 'same-origin',
})

// Link chain order matters:
//   errorLink first  → catches errors from all downstream links
//   authLink second  → attaches token to requests
//   httpLink last    → sends the actual HTTP request
export const apolloClient = new ApolloClient({
  link: ApolloLink.from([errorLink, authLink, httpLink]),
  cache: new InMemoryCache(),
  defaultOptions: {
    watchQuery: {
      // Always fetch from network, use cache as fallback.
      // For an admin console, stale data is worse than a network call.
      fetchPolicy: 'cache-and-network',
    },
  },
})
