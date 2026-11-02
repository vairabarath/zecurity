import { toast } from 'sonner'
import { CombinedGraphQLErrors, ServerError } from '@apollo/client/errors'
import { isCapabilityRefusal } from '@/apollo/links/refusal'

// mutationErrorMessage is the text to show for a failed mutation, or null when
// it is shown elsewhere: capability refusals are toasted by the error link
// (PENDING-19) and a 401 is the error link's refresh/login flow.
export function mutationErrorMessage(error: unknown): string | null {
  if (isCapabilityRefusal(error)) return null
  if (ServerError.is(error) && error.statusCode === 401) return null
  if (CombinedGraphQLErrors.is(error)) {
    return error.errors.map((e) => e.message).find((m) => typeof m === 'string' && m.trim()) ?? 'The request failed.'
  }
  if (ServerError.is(error)) return `The server returned an error (HTTP ${error.statusCode}).`
  if (error instanceof Error && error.message) return error.message
  return 'The request failed.'
}

// toastMutationError is the onError for mutations whose page has no error
// display of its own, so a failed action is never silent.
export function toastMutationError(error: unknown): void {
  const message = mutationErrorMessage(error)
  if (message !== null) toast.error('Action failed', { description: message })
}
