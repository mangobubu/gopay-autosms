import assert from 'node:assert/strict'
import test from 'node:test'

import { matchesCatalogOption } from './catalogSearch.ts'
import { normalizeCountries } from './normalizers.ts'

function country({ value, label, raw = {} }) {
  return {
    key: value,
    value,
    label,
    raw,
  }
}

test('matches the country codes users commonly enter', () => {
  const cases = [
    [country({ value: '12', label: 'United States (virtual)', raw: {} }), 'US'],
    [country({ value: '16', label: 'United Kingdom', raw: {} }), 'GB'],
    [country({ value: '6', label: 'Indonesia', raw: {} }), 'ID'],
    [country({ value: '4', label: 'Philippines', raw: {} }), 'PH'],
  ]

  for (const [option, query] of cases) {
    assert.equal(matchesCatalogOption(option, query), true, `${query} should match ${option.label}`)
  }
})

test('matches codes case-insensitively and ignores surrounding whitespace', () => {
  const unitedStates = country({
    value: '12',
    label: 'United States (virtual)',
    raw: {},
  })

  assert.equal(matchesCatalogOption(unitedStates, ' us '), true)
})

test('keeps existing searches by country name and normalized value', () => {
  const unitedKingdom = country({
    value: 'GB',
    label: 'United Kingdom',
    raw: {},
  })

  assert.equal(matchesCatalogOption(unitedKingdom, 'kingdom'), true)
  assert.equal(matchesCatalogOption(unitedKingdom, 'gb'), true)
})

test('keeps the provider numeric ID when an API response also exposes an ISO code', () => {
  const [unitedStates] = normalizeCountries({
    countries: [{ id: 187, code: 'US', name: 'United States' }],
  })

  assert.equal(unitedStates.value, '187')
  assert.equal(matchesCatalogOption(unitedStates, 'US'), true)
})

test('normalizes provider English names without changing the numeric ID', () => {
  const [countryOption] = normalizeCountries({
    countries: [{ id: 18, eng: 'DR Congo' }],
  })

  assert.equal(countryOption.value, '18')
  assert.equal(countryOption.label, 'DR Congo')
  assert.equal(matchesCatalogOption(countryOption, 'CD'), true)
})

test('supports the expected API aliases for country code fields', () => {
  const aliases = ['code', 'country', 'iso', 'iso2', 'alpha2', 'alpha_2', 'country_code', 'countryCode']

  for (const field of aliases) {
    const option = country({ value: 'numeric-id', label: 'Example', raw: { [field]: 'ZZ' } })
    assert.equal(matchesCatalogOption(option, 'zz'), true, `${field} should be searchable`)
  }

  const explicitUnitedStatesCode = country({ value: 'numeric-id', label: 'Example', raw: { iso: 'US' } })
  assert.equal(matchesCatalogOption(explicitUnitedStatesCode, 'US'), true)
})

test('prefers an explicit ISO code over the numeric provider-ID fallback', () => {
  const [countryOption] = normalizeCountries({
    countries: [{ id: 6, iso: 'PH', eng: 'Conflicting upstream fixture' }],
  })

  assert.equal(matchesCatalogOption(countryOption, 'PH'), true)
  assert.equal(matchesCatalogOption(countryOption, 'ID'), false)
})

test('does not match unrelated country codes', () => {
  const unitedStates = country({ value: '12', label: 'United States', raw: { code: 'US' } })

  assert.equal(matchesCatalogOption(unitedStates, 'GB'), false)
})

test('treats an exact ISO code as a country code, not a name substring', () => {
  const unitedStates = country({ value: '187', label: 'United States', raw: {} })
  const virtualUnitedStates = country({ value: '12', label: 'United States (virtual)', raw: {} })
  const australia = country({ value: '36', label: 'Australia', raw: {} })
  const russia = country({ value: '0', label: 'Russia', raw: {} })
  const indonesia = country({ value: '6', label: 'Indonesia', raw: {} })
  const trinidadAndTobago = country({ value: ' Trinidad and Tobago ', label: 'Trinidad and Tobago', raw: {} })
  const britishIndianOceanTerritory = country({ value: '999', label: 'British Indian Ocean Territory', raw: { iso: 'IO' } })
  const nigeria = country({ value: '19', label: 'Nigeria', raw: {} })
  const democraticCongo = country({ value: '18', label: 'Congo (Dem. Republic)', raw: {} })
  const ivoryCoast = country({ value: '27', label: 'Ivory Coast', raw: {} })
  const czechRepublic = country({ value: '63', label: 'Czech Republic', raw: {} })

  assert.equal(matchesCatalogOption(unitedStates, 'US'), true)
  assert.equal(matchesCatalogOption(virtualUnitedStates, 'US'), true)
  assert.equal(matchesCatalogOption(australia, 'US'), false)
  assert.equal(matchesCatalogOption(russia, 'US'), false)
  assert.equal(matchesCatalogOption(indonesia, 'ID'), true)
  assert.equal(matchesCatalogOption(trinidadAndTobago, 'ID'), false)
  assert.equal(matchesCatalogOption(britishIndianOceanTerritory, 'IN'), false)
  assert.equal(matchesCatalogOption(nigeria, 'NE'), false)
  assert.equal(matchesCatalogOption(democraticCongo, 'CD'), true)
  assert.equal(matchesCatalogOption(ivoryCoast, 'CI'), true)
  assert.equal(matchesCatalogOption(czechRepublic, 'CZ'), true)
})

test('treats an empty query as showing every option', () => {
  const indonesia = country({ value: '10', label: 'Indonesia', raw: { code: 'ID' } })

  assert.equal(matchesCatalogOption(indonesia, '  '), true)
})
