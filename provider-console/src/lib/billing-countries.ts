// Billing countries and their default currency (Sprint 26, D-64).
// PLACEHOLDER: the controller's P1b settings API owns this table; when it
// serves the list, the settings form takes it from there and this file goes.

export interface BillingCountry {
  code: string // ISO 3166-1 alpha-2
  name: string
  currency: string // ISO 4217
}

export const BILLING_COUNTRIES: BillingCountry[] = [
  { code: 'US', name: 'United States', currency: 'USD' },
  { code: 'IN', name: 'India', currency: 'INR' },
  { code: 'GB', name: 'United Kingdom', currency: 'GBP' },
  { code: 'DE', name: 'Germany', currency: 'EUR' },
  { code: 'FR', name: 'France', currency: 'EUR' },
  { code: 'JP', name: 'Japan', currency: 'JPY' },
  { code: 'SG', name: 'Singapore', currency: 'SGD' },
  { code: 'AE', name: 'United Arab Emirates', currency: 'AED' },
  { code: 'AU', name: 'Australia', currency: 'AUD' },
  { code: 'CA', name: 'Canada', currency: 'CAD' },
]
