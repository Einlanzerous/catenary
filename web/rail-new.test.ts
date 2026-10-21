/* `npm run test:rail-new` — the rail's + NEW control (CANT-302, from the
 * CANT-301 plan).
 *
 *   the box        18px, the logomark's height, from one padding on all
 *                  four sides over --type-label's 12px line and a 1px border
 *   the hit area   the box plus `::before` insets above and below reaches
 *                  44px or more; the control never shrinks (`flex: none`)
 *   the color      `--accent-mark` fill and the ink tokens named in the
 *                  `.new` rules, read from tokens.css per theme: at least
 *                  4.5:1 in both (the numbers are printed)
 *   the states     hover is a brightness step, focus-visible an outline,
 *                  disabled drops the fill; no raw hex anywhere in the rule
 *   `.who`         truncates with an ellipsis so the button never yields
 *   the footer     (CANT-304) the rail's one column is `minmax(0, 1fr)`, not
 *                  `auto`; `.me .name` and a row's `.name` truncate with an
 *                  ellipsis; the footer's other items do not shrink
 *   the bindings   @click, :disabled and reveal(openNew) as CANT-270 wrote
 *
 * It parses the rules of Rail.vue and tokens.css into property maps and
 * asserts on values and computed numbers, not on spelling. It renders
 * nothing: the repo has no browser dependency (the measured hit box and
 * overflow are in the PR); node:test over a vite-ssr bundle, chained into
 * `npm run test:transport` so `./verify.sh` runs it.
 */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const rail = readFileSync(resolve(process.cwd(), 'src/components/Rail.vue'), 'utf8')
const tokens = readFileSync(resolve(process.cwd(), 'src/styles/tokens.css'), 'utf8')
const style = rail.slice(rail.indexOf('>', rail.indexOf('<style')) + 1)
const rowSrc = readFileSync(resolve(process.cwd(), 'src/components/ConversationRow.vue'), 'utf8')
const rowStyle = rowSrc.slice(rowSrc.indexOf('>', rowSrc.indexOf('<style')) + 1)

/** The declarations of the one rule whose selector is `selector`, as a
 *  property map: comments, whitespace, ordering and the spelling of values
 *  (`-8px 0` against `-8px  0`) do not matter to what is asserted. */
const rule = (selector: string, source: string = style): Record<string, string> => {
  const css = source.replace(/\/\*[\s\S]*?\*\//g, '')
  const norm = (x: string) => x.replace(/\s+/g, ' ').trim()
  const re = /([^{}]+)\{([^{}]*)\}/g
  for (let m = re.exec(css); m; m = re.exec(css)) {
    if (norm(m[1]) !== norm(selector)) continue
    const out: Record<string, string> = {}
    for (const d of m[2].split(';')) {
      const i = d.indexOf(':')
      if (i > 0) out[d.slice(0, i).trim()] = norm(d.slice(i + 1))
    }
    return out
  }
  assert.fail(`no rule for ${selector}`)
}

/** CSS box shorthand (1 to 4 values) as [top, right, bottom, left] in px. */
const sides = (value: string | undefined): number[] => {
  const v = (value ?? '').split(' ').map(parseFloat)
  assert.ok(v.length >= 1 && v.length <= 4 && v.every((n) => !Number.isNaN(n)), `a px box value, got ${value}`)
  return [v[0], v[1] ?? v[0], v[2] ?? v[0], v[3] ?? v[1] ?? v[0]]
}
/** Every color value in a rule, to refuse a raw hex. */
const hasRawHex = (body: Record<string, string>) => Object.values(body).some((v) => /#[0-9a-fA-F]{3,8}\b/.test(v))

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

test('the box is as tall as the logomark, evenly padded, with a hit area of at least 44px', () => {
  const box = rule('.new')
  const [pt, pr, pb, pl] = sides(box.padding ?? [box['padding-top'], box['padding-right'], box['padding-bottom'], box['padding-left']].join(' '))
  assert.ok(pt === pr && pr === pb && pb === pl, `the same padding on all four sides: ${[pt, pr, pb, pl]}`)
  assert.equal(box.height, undefined, 'the height comes from the padding, not a literal')
  const lineHeight = parseFloat(/--type-label:\s*[^;]*?\/\s*([\d.]+)px/.exec(tokens)?.[1] ?? 'NaN')
  assert.ok(lineHeight > 0, '--type-label carries a line height in px')
  const border = parseFloat(box.border ?? box['border-width'] ?? '0')
  const height = lineHeight + pt + pb + 2 * border
  const mark = Number(/<Logomark :size="(\d+)"/.exec(rail)?.[1])
  assert.equal(height, mark, `the visible height ${height} is the logomark's ${mark}`)
  const [top, , bottom] = sides(rule('.new::before').inset)
  assert.ok(height - top - bottom >= 44, `${height} + ${-top} + ${-bottom} >= 44`)
  assert.equal(box.position, 'relative')
  assert.equal(box.flex, 'none')
  assert.ok(parseFloat(box['font-size']) >= 11 && parseFloat(box['font-size']) <= 13, box['font-size'])
  assert.equal(box['font-weight'], '600')
})

test('the fill is --accent-mark and every color is a token, never a raw hex', () => {
  const rules = [rule('.new'), rule(".new::before"), rule('.new:hover:not(:disabled)'), rule('.new:focus-visible'),
    rule('.new:disabled'), rule(":root[data-theme='light'] .new:not(:disabled)")]
  assert.equal(varName(rule('.new').background), '--accent-mark')
  for (const body of rules) assert.ok(!hasRawHex(body), JSON.stringify(body))
})

for (const theme of ['dark', 'light'] as const) {
  test(`the label clears 4.5:1 on the gold in the ${theme} theme`, () => {
    const inkToken = theme === 'light'
      ? varName(rule(":root[data-theme='light'] .new:not(:disabled)").color)
      : varName(rule('.new').color)
    assert.equal(inkToken, theme === 'light' ? '--text-primary' : '--on-accent')
    const fill = token(theme, varName(rule('.new').background))
    const ink = token(theme, inkToken)
    const ratio = contrast(fill, ink)
    console.log(`# ${theme}: ${inkToken} ${ink} on --accent-mark ${fill} = ${ratio.toFixed(2)}:1`)
    assert.ok(ratio >= 4.5, `${ratio.toFixed(2)}:1`)
  })
}

test('hover is a brightness step that leaves the ink alone', () => {
  const hover = rule('.new:hover:not(:disabled)')
  assert.match(hover.filter ?? '', /^brightness\(/)
  assert.equal(hover.color, undefined, 'hover leaves the ink alone')
})

test('focus-visible draws an outline', () => {
  const focus = rule('.new:focus-visible')
  assert.ok(parseFloat(focus['outline-width'] ?? focus.outline ?? '0') >= 2 || /^\d+px/.test(focus.outline ?? ''), 'a visible outline')
  assert.ok((focus.outline ?? '').includes('var(--text-primary)'), 'in --text-primary')
})

test('disabled drops the fill and names --text-disabled, never the gold', () => {
  const off = rule('.new:disabled')
  assert.equal(varName(off.color), '--text-disabled')
  assert.ok(['none', 'transparent'].includes(off.background), 'no fill')
  assert.ok(!Object.values(off).some((v) => v.includes('--accent-mark')), JSON.stringify(off))
})

test('.who truncates so the button never shrinks first', () => {
  const who = rule('.who')
  assert.equal(parseFloat(who['min-width']), 0)
  assert.equal(who.overflow, 'hidden')
  assert.equal(who['text-overflow'], 'ellipsis')
  assert.equal(who['white-space'], 'nowrap')
  // Without this the grid item grows to its content and the header outgrows
  // the rail: `.who` would never be narrower than its text.
  assert.equal(parseFloat(rule('.brand')['min-width']), 0)
})

test('the button keeps CANT-270 wiring: click, disabled, reveal(openNew)', () => {
  const m = /<button\b([^>]*\bclass="new"[^>]*)>/.exec(rail.slice(0, rail.indexOf('<style')))
  assert.ok(m, 'the + NEW button')
  const squash = (x: string) => x.replace(/\s+/g, '')
  const attrs = squash(m[1])
  assert.ok(attrs.includes(':disabled="!state.me||pickerState.busy"'), attrs)
  assert.ok(attrs.includes('@click="reveal(openNew)"'), attrs)
})

test('the rail column cannot grow past the rail, and long names truncate (CANT-304)', () => {
  // An unset column is `auto`, which grows to the widest min-content inside
  // it: a 40-character footer name made the rail's content 353px in a 304px
  // rail. minmax(0, 1fr) has a zero floor, so the column is the rail's width.
  assert.equal(rule('.rail')['grid-template-columns'], 'minmax(0, 1fr)')
  const name = rule('.me .name')
  assert.equal(parseFloat(name['min-width']), 0)
  assert.equal(name.overflow, 'hidden')
  assert.equal(name['text-overflow'], 'ellipsis')
  assert.equal(name['white-space'], 'nowrap')
  assert.equal(rule('.me > :not(.name)').flex, 'none', 'the controls and the lamp keep their size')
  const title = rule('.name', rowStyle)
  assert.equal(parseFloat(title['min-width']), 0)
  assert.equal(title.overflow, 'hidden')
  assert.equal(title['text-overflow'], 'ellipsis')
  assert.equal(title['white-space'], 'nowrap')
  assert.equal(rule('.preview', rowStyle)['text-overflow'], 'ellipsis')
})
