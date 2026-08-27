import type { PriceOption } from './types'

function isSelectablePrice(item: PriceOption): boolean {
  return item.price !== undefined && (item.stock === undefined || item.stock > 0)
}

function hasSameOfferDetails(left: PriceOption, right: PriceOption): boolean {
  return left.provider === right.provider
    && left.price === right.price
    && (left.tierDerived || right.tierDerived || left.tier === right.tier)
}

export function findRefreshedPrice(
  previous: PriceOption,
  nextPrices: PriceOption[],
): PriceOption | undefined {
  const selectablePrices = nextPrices.filter(isSelectablePrice)
  const exact = selectablePrices.find((item) => (
    item.value === previous.value && hasSameOfferDetails(item, previous)
  ))
  if (exact) return exact

  const compatible = selectablePrices.filter((item) => hasSameOfferDetails(item, previous))
  return compatible.length === 1 ? compatible[0] : undefined
}
