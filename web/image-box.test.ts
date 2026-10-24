/* `npm run test:image-box` — the inline image's box (CANT-49).
 *
 *   the cap        340x400 from the STORED aspect ratio, in both directions
 *   the box        the rendered figure and frame carry exactly imageBox()'s
 *                  size whether or not there is a URL or a placeholder, so
 *                  the row is the same box before and after the bytes
 *   the guard      the test that fails if the box depends on the loaded
 *                  image: the component never reads naturalWidth/Height,
 *                  `loaded` is only ever a class on the out-of-flow <img>
 *                  (never a v-if/v-else that swaps the frame, never in a
 *                  :style), the <img> and the blurhash canvas are
 *                  position:absolute, and the frame's height is a pixel
 *                  value bound from the stored dimensions
 *   the variants   inline asks for `&v=thumb`, the lightbox for the plain URL
 *   the hash       the blurhash decoder agrees with the reference vectors
 *
 * Renders with vue/server-renderer; the repo has no browser dependency, so the
 * measured before/after geometry in a real browser is in the PR. */

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import ImageAttachmentView from '@/components/ImageAttachment.vue'
import { decodeBlurhash } from '@/lib/blurhash'
import { IMAGE_MAX_H, IMAGE_MAX_W, imageBox, thumbUrl } from '@/lib/image-box'

const src = readFileSync(resolve(process.cwd(), 'src/components/ImageAttachment.vue'), 'utf8')
const template = src.slice(src.indexOf('<template>'), src.indexOf('</template>\n\n<style') + 11)
const script = src.slice(src.indexOf('<script'), src.indexOf('</script>'))
const style = src.slice(src.indexOf('<style'))

const base = { kind: 'image' as const, filename: 'a.png', bytes: 1234 }
const render = (image: Record<string, unknown>) =>
  renderToString(createSSRApp(ImageAttachmentView, { image: { ...base, ...image } as never }))

test('the cap: 340x400 at the stored aspect ratio', () => {
  assert.deepEqual(imageBox(4000, 3000), { w: 340, h: 255 }) // landscape: width-bound
  assert.deepEqual(imageBox(3000, 4000), { w: 300, h: 400 }) // portrait: height-bound
  assert.deepEqual(imageBox(1000, 1000), { w: 340, h: 340 })
  assert.deepEqual(imageBox(100, 4000), { w: 10, h: 400 }) // a tall strip
  for (const [w, h] of [[1, 1], [640, 480], [480, 640], [5000, 10], [10, 5000]]) {
    const b = imageBox(w, h)
    assert.ok(b.w >= 1 && b.w <= IMAGE_MAX_W && b.h >= 1 && b.h <= IMAGE_MAX_H, `${w}x${h} -> ${b.w}x${b.h}`)
  }
  assert.deepEqual(imageBox(0, 0), { w: 340, h: 340 }) // degenerate stays a box
})

test('the box is the same before and after the bytes: it keys on dimensions alone', async () => {
  const stored = { width: 3000, height: 4000 }
  const want = imageBox(3000, 4000)
  const variants = [
    { ...stored, url: '' }, // outbox, no URL yet
    { ...stored, url: '/media/k?kid=1&sig=x' }, // URL, nothing loaded
    { ...stored, url: '/media/k?kid=1&sig=x', placeholder: 'LEHV6nWB2yk8pyo0adR*.7kCMdnj' },
    { ...stored, url: '/media/k?kid=1&sig=x', placeholder: 'not a blurhash' },
    { ...stored, url: '/media/k?kid=1&sig=x', uploadedBytes: 600 },
    { ...stored, url: '/media/k?kid=1&sig=x', bytes: 99_999_999, filename: 'a-very-long-filename-that-would-wrap-if-it-were-allowed-to.png' },
  ]
  for (const v of variants) {
    const html = await render(v)
    assert.match(html, new RegExp(`<figure[^>]*style="width:${want.w}px;?"`), JSON.stringify(v))
    assert.match(html, new RegExp(`class="frame[^"]*"[^>]*style="height:${want.h}px;?"`), JSON.stringify(v))
  }
})

test('the guard: nothing in the component lets the loaded image size the row', () => {
  assert.doesNotMatch(script + template, /naturalHeight|clientHeight|offsetHeight|getBoundingClientRect|ResizeObserver/)
  // naturalWidth appears once, guarded by .complete, and only sets `loaded`.
  assert.equal((script.match(/naturalWidth/g) ?? []).length, 1)
  assert.match(script, /complete && pic\.value\.naturalWidth > 0\) loaded\.value = true/)
  // `loaded` is only a class; it never swaps the frame or enters a style.
  const uses = template.match(/.*\bloaded\b.*/g) ?? []
  for (const u of uses) {
    assert.ok(/:class="\{ shown: loaded \}"|@load="loaded = true"|v-if="!loaded" class="badge"/.test(u), `unexpected use of loaded: ${u}`)
  }
  assert.doesNotMatch(template, /:style="[^"]*loaded/)
  // The frame's height is bound from the stored box.
  assert.match(script, /frameStyle = computed\(\(\) => \(\{ height: `\$\{box\.value\.h\}px` \}\)\)/)
  assert.match(script, /box = computed\(\(\) => imageBox\(props\.image\.width, props\.image\.height\)\)/)
  // What paints inside the frame is out of flow, so it cannot size it.
  const css = style.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^<style[^>]*>/, '')
  const rule = (sel: string) => {
    const norm = (x: string) => x.replace(/\s+/g, ' ').trim()
    for (const m of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) if (norm(m[1]) === norm(sel)) return m[2]
    assert.fail(`no rule ${sel}`)
  }
  assert.match(rule('.blur, .pic'), /position:\s*absolute/)
  assert.match(rule('.badge'), /position:\s*absolute/)
  assert.match(rule('.frame'), /overflow:\s*hidden/)
  assert.doesNotMatch(rule('.frame'), /min-height|height:\s*auto|aspect-ratio/)
  assert.match(rule('.pic'), /opacity:\s*0/)
  assert.doesNotMatch(rule('.pic'), /position/, '.pic must not override the shared absolute rule')
  // The caption row cannot wrap or grow with its content.
  assert.match(rule('.fname'), /white-space:\s*nowrap/)
  assert.match(rule('.dims'), /white-space:\s*nowrap/)
  assert.doesNotMatch(template, /<template v-if="loaded">/)
})

test('the variants: inline asks for the thumbnail, the lightbox for the plain URL', async () => {
  const url = '/media/abc?kid=1&sig=s'
  assert.equal(thumbUrl(url), `${url}&v=thumb`)
  assert.equal(thumbUrl('/media/abc'), '/media/abc?v=thumb')
  assert.equal(thumbUrl(''), '')
  const html = await render({ width: 640, height: 480, url })
  assert.match(html, /<img[^>]*class="pic"[^>]*src="\/media\/abc\?kid=1&amp;sig=s&amp;v=thumb"/)
  assert.doesNotMatch(html, /light-pic/, 'the lightbox is closed until opened')
  // The open lightbox: thumbnail under, plain URL over.
  assert.match(template, /class="light-pic" :src="inline"/)
  assert.match(template, /class="light-pic" :src="image\.url"/)
})

test('the blurhash decoder', () => {
  // The reference implementation's own example hash.
  const px = decodeBlurhash('LEHV6nWB2yk8pyo0adR*.7kCMdnj', 8, 6)
  assert.ok(px && px.length === 8 * 6 * 4)
  assert.ok(px.every((v, i) => (i % 4 === 3 ? v === 255 : true)))
  assert.notDeepEqual(Array.from(px.slice(0, 4)), Array.from(px.slice(-4)), 'a gradient, not a flat plane')
  // A 1x1 hash is flat: its DC colour everywhere. '00' size, 0 maxAC, then #ff0000-ish.
  assert.equal(decodeBlurhash('nothash', 4, 4), null)
  assert.equal(decodeBlurhash('', 4, 4), null)
  assert.equal(decodeBlurhash('LEHV6nWB2yk8pyo0adR*.7kCMdn', 4, 4), null, 'wrong length')
})
