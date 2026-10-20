/* `npm run test:rail-new` — the rail's + NEW control (CANT-302, from the
 * CANT-301 plan).
 *
 *   the hit area   a 30px box plus `::before` insets above and below
 *                  reaches 44px or more; the control never shrinks (`flex: none`)
 *   the color      `--accent-mark` fill and the ink tokens named in the
 *                  `.new` rules, read from tokens.css per theme: at least
 *                  4.5:1 in both (the numbers are printed)
 *   the states     hover is a brightness step, focus-visible an outline,
 *                  disabled drops the fill; no raw hex anywhere in the rule
 *   `.who`         truncates with an ellipsis so the button never yields
 *   the bindings   @click, :disabled and reveal(openNew) as CANT-270 wrote
 *
 * It reads the source text of Rail.vue and tokens.css, as the other node:test
 * files read the Dart app; node:test over a vite-ssr bundle, chained into
 * `npm run test:transport` so `./verify.sh` runs it.
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const rail = readFileSync(resolve(process.cwd(), 'src/components/Rail.vue'), 'utf8')
const tokens = readFileSync(resolve(process.cwd(), 'src/styles/tokens.css'), 'utf8')
const style = rail.slice(rail.indexOf('<style'))

/** The declaration block of the one rule whose selector is exactly `selector`. */
const rule = (selector: string): string => {
  const esc = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const m = new RegExp(`(?:^|\\n)${esc}\\s*\\{([^}]*)\\}`).exec(style)
  assert.ok(m, `Rail.vue has a rule for ${selector}`)
  return m[1]
}
const decl = (body: string, prop: string): string | undefined =>
  new RegExp(`(?:^|[;\\s])${prop}:\\s*([^;]+);`).exec(body)?.[1].trim()

const themeBlock = (theme: 'dark' | 'light'): string => {
  const start = tokens.indexOf(`:root[data-theme='${theme}'] {`)
  assert.ok(start >= 0, `${theme} block`)
  return tokens.slice(start, tokens.indexOf('\n}', start))
}
const token = (theme: 'dark' | 'light', name: string): string => {
  const m = new RegExp(`${name}:\\s*(#[0-9a-fA-F]{6})`).exec(themeBlock(theme))
  assert.ok(m, `${name} in the ${theme} theme`)
  return m[1]
}
const varName = (value: string | undefined): string => {
  const m = /^var\((--[a-z-]+)\)$/.exec(value ?? '')
  assert.ok(m, `a var() reference, got ${value}`)
  return m[1]
}

const lum = (hex: string): number => {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255)
    .map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4))
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]
}
const contrast = (a: string, b: string): number => {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

test('the hit area is at least 44px: a 30px box plus the ::before insets', () => {
  const box = rule('.new')
  const h = parseFloat(decl(box, 'height')!)
  const m = /inset:\s*(-?\d+)px\s+0/.exec(rule('.new::before'))
  assert.ok(m, '::before insets vertically and not horizontally')
  assert.ok(h + 2 * -Number(m[1]) >= 44, `${h} + 2 x ${-Number(m[1])} >= 44`)
  assert.equal(h, 30)
  assert.equal(decl(box, 'position'), 'relative')
  assert.equal(decl(box, 'flex'), 'none')
  assert.equal(decl(box, 'font-size'), '12px')
  assert.equal(decl(box, 'font-weight'), '600')
})

test('the fill is --accent-mark and every color is a token, never a raw hex', () => {
  const rules = [rule('.new'), rule(".new::before"), rule('.new:hover:not(:disabled)'), rule('.new:focus-visible'),
    rule('.new:disabled'), rule(":root[data-theme='light'] .new:not(:disabled)")]
  assert.equal(varName(decl(rule('.new'), 'background')), '--accent-mark')
  for (const body of rules) assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(body), body)
})

for (const theme of ['dark', 'light'] as const) {
  test(`the label clears 4.5:1 on the gold in the ${theme} theme`, () => {
    const inkToken = theme === 'light'
      ? varName(decl(rule(":root[data-theme='light'] .new:not(:disabled)"), 'color'))
      : varName(decl(rule('.new'), 'color'))
    assert.equal(inkToken, theme === 'light' ? '--text-primary' : '--on-accent')
    const fill = token(theme, varName(decl(rule('.new'), 'background')))
    const ink = token(theme, inkToken)
    const ratio = contrast(fill, ink)
    console.log(`# ${theme}: ${inkToken} ${ink} on --accent-mark ${fill} = ${ratio.toFixed(2)}:1`)
    assert.ok(ratio >= 4.5, `${ratio.toFixed(2)}:1`)
  })
}

test('hover is a brightness step that leaves the ink alone', () => {
  const hover = rule('.new:hover:not(:disabled)')
  assert.match(decl(hover, 'filter') ?? '', /^brightness\(/)
  assert.notEqual(decl(hover, 'color'), 'var(--text-primary)')
  assert.equal(decl(hover, 'color'), undefined)
})

test('focus-visible draws an outline', () => {
  const focus = rule('.new:focus-visible')
  assert.match(decl(focus, 'outline') ?? '', /^2px solid var\(--text-primary\)$/)
})

test('disabled drops the fill and names --text-disabled, never the gold', () => {
  const off = rule('.new:disabled')
  assert.equal(varName(decl(off, 'color')), '--text-disabled')
  assert.equal(decl(off, 'background'), 'none')
  assert.ok(!off.includes('--accent-mark'), off)
})

test('.who truncates so the button never shrinks first', () => {
  const who = rule('.who')
  assert.equal(decl(who, 'min-width'), '0')
  assert.equal(decl(who, 'overflow'), 'hidden')
  assert.equal(decl(who, 'text-overflow'), 'ellipsis')
  assert.equal(decl(who, 'white-space'), 'nowrap')
  // Without this the grid item grows to its content and the header outgrows
  // the rail: `.who` would never be narrower than its text.
  assert.equal(decl(rule('.brand'), 'min-width'), '0')
})

test('the button keeps CANT-270 wiring: click, disabled, reveal(openNew)', () => {
  assert.ok(
    rail.includes('<button class="new" :disabled="!state.me || pickerState.busy" @click="reveal(openNew)">+ NEW</button>'),
  )
})
