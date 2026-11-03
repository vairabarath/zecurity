import { useState } from 'react'
import { Globe2, X } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { countryLabel, searchCountries } from '@/lib/countries'
import { countryRuleProblem, type CountryRuleValue } from '@/lib/countryRule'

function CountryPicker({
  id,
  label,
  hint,
  selected,
  taken,
  onAdd,
  onRemove,
}: {
  id: string
  label: string
  hint: string
  selected: string[]
  taken: string[]
  onAdd: (code: string) => void
  onRemove: (code: string) => void
}) {
  const [query, setQuery] = useState('')
  const matches = query.trim() ? searchCountries(query, [...selected, ...taken]).slice(0, 6) : []
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id} className="text-xs font-semibold">{label}</Label>
      {selected.length > 0 && (
        <div className="flex flex-wrap gap-1.5" aria-label={`${label} list`}>
          {selected.map((code) => (
            <span
              key={code}
              className="inline-flex items-center gap-1 rounded-full border border-border bg-secondary px-2 py-0.5 text-xs"
            >
              {countryLabel(code)}
              <button
                type="button"
                onClick={() => onRemove(code)}
                aria-label={`Remove ${countryLabel(code)} from ${label}`}
                className="text-muted-foreground hover:text-foreground"
              >
                <X className="h-3 w-3" />
              </button>
            </span>
          ))}
        </div>
      )}
      <Input
        id={id}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        placeholder="Search a country or code…"
        className="h-9 text-sm"
        autoComplete="off"
      />
      {matches.length > 0 && (
        <ul className="rounded-lg border border-border bg-card p-1" role="listbox" aria-label={`${label} suggestions`}>
          {matches.map((code) => (
            <li key={code}>
              <button
                type="button"
                role="option"
                aria-selected={false}
                onClick={() => {
                  onAdd(code)
                  setQuery('')
                }}
                className="w-full rounded-md px-2 py-1 text-left text-sm hover:bg-secondary"
              >
                {countryLabel(code)}
              </button>
            </li>
          ))}
        </ul>
      )}
      <p className="text-[11px] text-muted-foreground">{hint}</p>
    </div>
  )
}

/**
 * The country check (PENDING-20): allow and block lists of countries, judged
 * against the device's public IP. Block wins; an empty allow list means "any
 * country not blocked"; an unknown location is always denied.
 */
export function CountryRuleEditor({
  value,
  onChange,
}: {
  value: CountryRuleValue
  onChange: (next: CountryRuleValue) => void
}) {
  const problem = countryRuleProblem(value)
  const add = (list: 'allow' | 'deny', code: string) => {
    const other = list === 'allow' ? 'deny' : 'allow'
    onChange({
      ...value,
      [list]: [...value[list], code].sort(),
      [other]: value[other].filter((c) => c !== code),
    })
  }
  const remove = (list: 'allow' | 'deny', code: string) =>
    onChange({ ...value, [list]: value[list].filter((c) => c !== code) })

  return (
    <div className="rounded-2xl border border-border p-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2 text-sm font-semibold">
          <Globe2 className="h-4 w-4 text-muted-foreground" />
          Device country (public IP)
        </div>
        <Switch
          checked={value.enabled}
          onCheckedChange={(enabled) => onChange({ ...value, enabled })}
          aria-label="Device country (public IP)"
        />
      </div>
      <p className="mt-2 text-xs text-muted-foreground">
        Admits devices only from the countries you choose, judged from the device's public IP
        address. A VPN or corporate proxy shows its own country; an unknown location is always
        denied.
      </p>
      {value.enabled && (
        <div className="mt-4 space-y-4">
          <CountryPicker
            id="country-allow"
            label="Allow only these countries"
            hint="Leave empty to allow every country you don't block."
            selected={value.allow}
            taken={value.deny}
            onAdd={(c) => add('allow', c)}
            onRemove={(c) => remove('allow', c)}
          />
          <CountryPicker
            id="country-deny"
            label="Block these countries"
            hint="Blocked countries are refused even if they are also allowed."
            selected={value.deny}
            taken={value.allow}
            onAdd={(c) => add('deny', c)}
            onRemove={(c) => remove('deny', c)}
          />
          {problem && <p className="text-xs text-destructive">{problem}</p>}
          <p className="text-[11px] text-muted-foreground">
            <a href="https://db-ip.com" target="_blank" rel="noreferrer" className="underline">
              IP geolocation by DB-IP
            </a>
          </p>
        </div>
      )}
    </div>
  )
}
