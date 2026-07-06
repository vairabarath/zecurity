import { RetryLink } from '@apollo/client/link/retry'
import { ServerError } from '@apollo/client/errors'
import type { ApolloLink } from '@apollo/client/core'
import { Kind, OperationTypeNode } from 'graphql'

// True if the operation is a mutation. Mutations are not idempotent, so they
// must never be transparently retried — a retried "create" could double-write.
function isMutation(operation: ApolloLink.Operation): boolean {
  return operation.query.definitions.some(
    (def) =>
      def.kind === Kind.OPERATION_DEFINITION &&
      def.operation === OperationTypeNode.MUTATION,
  )
}

// RetryLink retries QUERIES on transient network failures with exponential
// backoff + jitter. It deliberately does NOT retry:
//   - mutations (not idempotent), and
//   - HTTP 4xx responses, which are deterministic. In particular a 401 is left
//     to errorLink, which performs the token refresh + single replay; retrying
//     it here would race that flow.
// Transient network errors (fetch rejected / offline) and 5xx are retried.
export const retryLink = new RetryLink({
  delay: {
    initial: 300,
    max: 5_000,
    jitter: true,
  },
  attempts: {
    max: 3,
    retryIf: (error, operation) => {
      if (!error) return false
      if (isMutation(operation)) return false
      if (
        ServerError.is(error) &&
        error.statusCode >= 400 &&
        error.statusCode < 500
      ) {
        return false
      }
      return true
    },
  },
})
