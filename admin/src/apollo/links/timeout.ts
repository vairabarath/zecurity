import { ApolloLink } from '@apollo/client/core'
import { Observable } from 'rxjs'

// Per-attempt request deadline. If httpLink hasn't produced a result within
// this window the operation is errored and the underlying request aborted
// (unsubscribing the HttpLink subscription triggers its AbortController).
const TIMEOUT_MS = 25_000

// timeoutLink bounds the latency of each downstream attempt. It sits above
// authLink/httpLink and below retryLink, so a hung request fails within
// TIMEOUT_MS and retryLink can decide whether to try again.
export const timeoutLink = new ApolloLink((operation, forward) => {
  return new Observable<ApolloLink.Result>((observer) => {
    const timer = setTimeout(() => {
      observer.error(
        new Error(`GraphQL request timed out after ${TIMEOUT_MS}ms`),
      )
    }, TIMEOUT_MS)

    const subscription = forward(operation).subscribe({
      next: (result) => observer.next(result),
      error: (err) => {
        clearTimeout(timer)
        observer.error(err)
      },
      complete: () => {
        clearTimeout(timer)
        observer.complete()
      },
    })

    // Teardown: runs on timeout-error, downstream error, completion, or
    // upstream unsubscribe. Cancels the timer and the in-flight request.
    return () => {
      clearTimeout(timer)
      subscription.unsubscribe()
    }
  })
})
