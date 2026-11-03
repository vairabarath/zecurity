// Country-rule editor state and checks (PENDING-20); mirrors the controller.

export type CountryRuleValue = { enabled: boolean; allow: string[]; deny: string[] }

export const EMPTY_COUNTRY_RULE: CountryRuleValue = { enabled: false, allow: [], deny: [] }

/** Why the rule can't be saved yet, or null. Mirrors the controller's checks. */
export function countryRuleProblem(rule: CountryRuleValue): string | null {
  if (!rule.enabled) return null
  if (rule.allow.length === 0 && rule.deny.length === 0) {
    return 'Choose at least one country to allow or block.'
  }
  return null
}

function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && [...a].sort().join(',') === [...b].sort().join(',')
}

export function countryRuleChanged(a: CountryRuleValue, b: CountryRuleValue): boolean {
  return a.enabled !== b.enabled || !sameList(a.allow, b.allow) || !sameList(a.deny, b.deny)
}
