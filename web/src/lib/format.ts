/* Formatting. Everything here renders into a tabular-numeral context, so
 * width must not depend on the value: pad, never trim. */

const DAY = ['SUN', 'MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT']
const MONTH = [
  'JAN', 'FEB', 'MAR', 'APR', 'MAY', 'JUN',
  'JUL', 'AUG', 'SEP', 'OCT', 'NOV', 'DEC',
]

const pad = (n: number) => String(n).padStart(2, '0')

/** 24-hour clock. The gutter carries one of these on every single row. */
export function clock(iso: string): string {
  const d = new Date(iso)
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** mm:ss for audio positions and durations. */
export function duration(sec: number): string {
  const s = Math.max(0, Math.round(sec))
  return `${Math.floor(s / 60)}:${pad(s % 60)}`
}

/** Date-separator label inside a thread: "15 AUG". */
export function dayLabel(iso: string): string {
  const d = new Date(iso)
  return `${d.getDate()} ${MONTH[d.getMonth()]}`
}

/** Rail timestamp. Today is a clock; this week is a weekday; older is a date.
 *  All three are the same width class, so the column never reflows. */
export function railStamp(iso: string, now = new Date()): string {
  const d = new Date(iso)
  if (sameDay(d, now)) return clock(iso)
  const days = Math.floor((startOfDay(now) - startOfDay(d)) / 86_400_000)
  if (days < 7) return DAY[d.getDay()]
  return `${d.getDate()} ${MONTH[d.getMonth()]}`
}

/** "15 AUG · 13:52" for search results, which span conversations and days. */
export function fullStamp(iso: string): string {
  return `${dayLabel(iso)} · ${clock(iso)}`
}

export function sameDay(a: Date, b: Date): boolean {
  return startOfDay(a) === startOfDay(b)
}

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

/** Uploads show bytes, not a spinner — progress in this product is numeric. */
export function megabytes(bytes: number): string {
  return `${(bytes / 1_048_576).toFixed(1)} MB`
}

export function countWords(text: string): number {
  return text.trim().split(/\s+/).filter(Boolean).length
}
