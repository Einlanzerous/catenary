/* The inline image's box (CANT-49), derived from the STORED dimensions and
 * nothing else. Nothing here may read the loaded image: the row's height is
 * final before the first byte arrives, so the thread never reflows under the
 * reader. `web/image-box.test.ts` holds that. */

export const IMAGE_MAX_W = 340
export const IMAGE_MAX_H = 400

/** Width then height in CSS pixels: fit inside 340x400 at the stored aspect
 *  ratio. Never upscales past the cap and never returns a zero side, so a
 *  degenerate stored size (the outbox's 1x1 stand-in) still has a box. */
export function imageBox(width: number, height: number): { w: number; h: number } {
  const aspect = width > 0 && height > 0 ? width / height : 1
  let w = IMAGE_MAX_W
  let h = Math.round(w / aspect)
  if (h > IMAGE_MAX_H) {
    h = IMAGE_MAX_H
    w = Math.round(h * aspect)
  }
  return { w: Math.max(1, w), h: Math.max(1, h) }
}

/** CANT-47: the thumbnail is the same signed URL with `&v=thumb`; the lightbox
 *  and everything else use the URL as the wire carries it. Empty stays empty
 *  (an outbox entry has no media URL yet). */
export function thumbUrl(url: string): string {
  if (!url) return url
  return `${url}${url.includes('?') ? '&' : '?'}v=thumb`
}

/** CANT-330 (canvas 08, "image box"): a link card's image box. The card's width
 *  is the layout's; only the ratio is ours, and it is the STORED one clamped
 *  between 2:1 (wide) and 1:1 (square), so a tall page image cannot make a
 *  tall card and a sliver cannot make a thin one. Returned as the pair for a
 *  CSS `aspect-ratio`, never a measurement of the loaded image. */
export function linkImageAspect(width: number, height: number): { w: number; h: number } {
  if (!(width > 0) || !(height > 0)) return { w: 1, h: 1 }
  if (height * 2 < width) return { w: 2, h: 1 }
  if (height > width) return { w: 1, h: 1 }
  return { w: width, h: height }
}
