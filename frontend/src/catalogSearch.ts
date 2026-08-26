import type { CatalogOption } from './types'

const englishRegionNames = new Intl.DisplayNames(['en'], { type: 'region' })

// SMSBower follows the numeric catalogue used by SMS-Activate-compatible APIs.
// The API normally returns only an id and a display name, so keep this lookup
// in the UI as a fallback for responses that do not yet include `iso`.
const providerISOByID = new Map<number, string>(
  `RU UA KZ CN PH MM ID MY KE TZ VN KG US IL HK PL GB MG CD NG MO EG IN IE KH LA HT CI GM RS YE ZA RO CO EE AZ CA MA GH AR UZ CM TD DE LT HR SE IQ NL LV AT BY TH SA MX TW ES IR DZ SI BD SN TR CZ LK PE PK NZ GN ML VE ET MN BR AF UG AO CY FR PG MZ NP BE BG HU MD IT PY HN TN NI TL BO CR GT AE ZW PR SD TG KW SV LY JM TT EC SZ OM BA DO SY QA PA CU MR SL JO PT BB BI BJ BN BS BW BZ CF DM GD GE GR GW GY IS KM KN LR LS MW NA NE RW SK SR TJ MC BH RE ZM AM SO CG CL BF LB GA AL UY MU BT MV GP TM GF FI LC LU VC GQ DJ AG KY ME DK CH NO AU ER SS ST AW MS AI JP MK SC NC CV US PS FJ KR KP EH SB JE BM SG TO WS MT LI GI FO XK NU`
    .split(/\s+/)
    .map((code, id) => [id, code]),
)

const isoAlpha2Codes = new Set([
  ...providerISOByID.values(),
  ...`
    AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ
    BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ
    CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ
    DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR
    GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY
    HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP
    KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY
    MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ
    NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR
    PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN
    SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ
    UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW
  `.trim().split(/\s+/),
  'XK',
])

const searchableRawFields = [
  'code',
  'country',
  'country_code',
  'countryCode',
  'iso',
  'iso2',
  'alpha2',
  'alpha_2',
  'name',
  'eng',
  'english',
  'nameEn',
  'label',
  'title',
] as const

const searchableCodeFields = new Set([
  'code',
  'country',
  'country_code',
  'countryCode',
  'iso',
  'iso2',
  'alpha2',
  'alpha_2',
])

const countryNameAliases = new Map<string, string>([
  ['usa', 'US'],
  ['usa virtual', 'US'],
  ['united states virtual', 'US'],
  ['uk', 'GB'],
  ['england', 'GB'],
  ['uk england', 'GB'],
  ['dr congo', 'CD'],
  ['drc', 'CD'],
  ['democratic republic of the congo', 'CD'],
  ['congo dem republic', 'CD'],
  ['ivory coast', 'CI'],
  ['ivory', 'CI'],
  ['czech', 'CZ'],
  ['czech republic', 'CZ'],
  ['swaziland', 'SZ'],
  ['uae', 'AE'],
  ['united arab emirates', 'AE'],
  ['macao', 'MO'],
  ['macau', 'MO'],
  ['palestine', 'PS'],
  ['south korea', 'KR'],
  ['north korea', 'KP'],
  ['papua', 'PG'],
  ['salvador', 'SV'],
  ['bosnia', 'BA'],
  ['reunion', 'RE'],
  ['kosovo', 'XK'],
])

function normalized(value: unknown): string {
  return typeof value === 'string' || typeof value === 'number'
    ? String(value).trim().toLocaleLowerCase('en')
    : ''
}

function normalizedName(value: unknown): string {
  return normalized(value)
    .normalize('NFD')
    .replace(/\p{M}/gu, '')
    .replace(/[^a-z0-9]+/g, ' ')
    .trim()
}

function normalizeCode(value: unknown): string {
  const code = typeof value === 'string' || typeof value === 'number'
    ? String(value).trim().toUpperCase()
    : ''
  return /^[A-Z]{2}$/.test(code) ? code : ''
}

for (const code of isoAlpha2Codes) {
  try {
    const name = englishRegionNames.of(code)
    const key = normalizedName(name)
    if (name && name !== code && key && !countryNameAliases.has(key)) {
      countryNameAliases.set(key, code)
    }
  } catch {
    // Intl.DisplayNames does not know user-assigned codes such as XK.
  }
}

function rawValues(item: CatalogOption): unknown[] {
  return searchableRawFields.map((field) => item.raw[field])
}

function countryCodes(item: CatalogOption): Set<string> {
  const codes = new Set<string>()
  for (const field of searchableCodeFields) {
    const code = normalizeCode(item.raw[field])
    if (code) {
      codes.add(code)
      if (code === 'UK') codes.add('GB')
    }
  }

  const valueCode = normalizeCode(item.value)
  if (valueCode) {
    codes.add(valueCode)
    if (valueCode === 'UK') codes.add('GB')
  }

  // An upstream ISO field (or an alpha-2 option value) is authoritative.
  // Use the provider's numeric-ID catalogue only when no explicit code exists,
  // so a changed or gateway-specific ID mapping cannot create two matches.
  const value = normalized(item.value)
  const numericID = /^\d+$/.test(value) ? Number(value) : undefined
  if (codes.size === 0 && numericID !== undefined) {
    const code = providerISOByID.get(numericID)
    if (code) codes.add(code)
  }

  for (const candidate of [item.label, item.description, ...rawValues(item)]) {
    const alias = countryNameAliases.get(normalizedName(candidate))
    if (alias) codes.add(alias)
  }
  return codes
}

function canonicalQueryCode(value: string): string {
  const code = normalizeCode(value)
  if (code === 'UK') return 'GB'
  return code
}

/** Matches catalogue options by visible text, provider value, or ISO country code. */
export function matchesCatalogOption(item: CatalogOption, query: string): boolean {
  const term = normalized(query)
  if (!term) return true

  // A two-letter query is treated as a country code. Comparing structured
  // codes (including the provider-ID fallback) avoids substring false hits
  // such as IN -> British Indian Ocean Territory or NE -> Nigeria.
  const queryCode = canonicalQueryCode(query)
  if (queryCode) {
    return countryCodes(item).has(queryCode)
  }

  const candidates = [
    item.label,
    item.value,
    item.description,
    ...rawValues(item),
  ].map(normalized).filter(Boolean)
  return candidates.some((candidate) => candidate.includes(term))
}
