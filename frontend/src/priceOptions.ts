import type { PriceOption } from './types'

function isSelectablePrice(item: PriceOption): boolean {
  return item.stock !== 0 && item.price !== undefined
}

function hasSameOfferDetails(left: PriceOption, right: PriceOption): boolean {
  return left.provider === right.provider
    && left.tier === right.tier
    && left.price === right.price
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
