const exponents: Record<string, number> = { KES: 2, UGX: 0, TZS: 2, NGN: 2, USD: 2, EUR: 2, GBP: 2 }

export const exponent = (currency: string) => exponents[currency] ?? 2

/** Formats integer minor units, e.g. 123456 KES → "1,234.56". */
export function amount(minor: number, currency: string): string {
  const e = exponent(currency)
  return (minor / 10 ** e).toLocaleString('en-KE', { minimumFractionDigits: e, maximumFractionDigits: e })
}

export const money = (minor: number, currency: string) => `${currency} ${amount(minor, currency)}`

/** Parses a major-unit input ("1,250.50") into integer minor units. */
export function toMinor(major: string, currency: string): number {
  const value = Number(major.replace(/,/g, ''))
  if (!Number.isFinite(value) || value <= 0) throw new Error('Enter a positive amount')
  return Math.round(value * 10 ** exponent(currency))
}

export const lcy = (value: number, currency = 'KES') =>
  `${currency} ${value.toLocaleString('en-KE', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`

export function dateTime(iso?: string | null): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleString('en-KE', { dateStyle: 'medium', timeStyle: 'short' })
}

export function day(iso?: string | null): string {
  if (!iso) return '—'
  return new Date(`${iso}T00:00:00`).toLocaleDateString('en-KE', { weekday: 'short', day: 'numeric', month: 'short', year: 'numeric' })
}

export const short = (id?: string | null) => (id ? `${id.slice(0, 8)}…` : '—')

/** Splits PascalCase enum values for display: "PendingReview" → "Pending review". */
export const label = (value: string) => value.replace(/_/g, ' ').replace(/([a-z])([A-Z])/g, '$1 $2').replace(/^./, (c) => c.toUpperCase()).replace(/ ([A-Z])/g, (m) => m.toLowerCase())

/** Compact date for table cells: "14 Oct 2026". */
export function shortDay(iso?: string | null): string {
  if (!iso) return '—'
  return new Date(`${iso}T00:00:00`).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' })
}

// Labels for the core fields a version can put on its screen.
const fieldLabels: Record<string, string> = {
  debitAccount: 'Debit account', creditAccount: 'Credit account', amount: 'Amount', currency: 'Currency', narrative: 'Narrative',
  paymentDetails: 'Payment details', method: 'Method', source: 'Source', 'payer.firstName': 'Payer first name', 'payer.lastName': 'Payer last name',
  'payer.email': 'Payer email', 'payer.phone': 'Payer phone', 'payer.address': 'Payer address', 'payer.postalCode': 'Payer postal code',
  returnUrl: 'Return URL', rail: 'Rail', 'beneficiary.account': 'Beneficiary account', 'beneficiary.name': 'Beneficiary name',
  'beneficiary.bankCode': 'Beneficiary bank code', channelCode: 'Channel', reference: 'Reference', originalReference: 'Original transaction',
  reason: 'Reason', attributes: 'Channel fields',
}

export const fieldLabel = (path: string) => fieldLabels[path] ?? path
