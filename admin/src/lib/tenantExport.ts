/** Reads `token` from a URL fragment like "#token=abc" (Sprint 23 S4). */
export function tokenFromHash(hash: string): string {
  return new URLSearchParams(hash.replace(/^#/, '')).get('token') ?? ''
}
