import type { GoPayLoginStatusView } from './types'

export function phoneLookupKey(phone: string): string {
  return phone.replace(/\D/g, '')
}

export function indexAccountLoginStatusesByPhone(
  accounts: readonly GoPayLoginStatusView[],
): Map<string, GoPayLoginStatusView> {
  const statuses = new Map<string, GoPayLoginStatusView>()
  for (const account of accounts) {
    const key = phoneLookupKey(account.phone)
    if (key) statuses.set(key, account)
  }
  return statuses
}

export function findAccountLoginStatusByPhone(
  statuses: ReadonlyMap<string, GoPayLoginStatusView>,
  phone: string,
): GoPayLoginStatusView | undefined {
  const key = phoneLookupKey(phone)
  return key ? statuses.get(key) : undefined
}
