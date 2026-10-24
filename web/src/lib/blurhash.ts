/* Blurhash decoder (https://github.com/woltapp/blurhash), the server encodes
 * in internal/media/blurhash.go. Pure, no DOM: returns RGBA. A malformed hash
 * returns null and the caller paints the plane flat, which is what the wire
 * says an absent placeholder means. */

const B83 = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz#$%*+,-.:;=?@[]^_{|}~'

function decode83(s: string): number {
  let v = 0
  for (const ch of s) {
    const i = B83.indexOf(ch)
    if (i < 0) return NaN
    v = v * 83 + i
  }
  return v
}

const toLinear = (v: number) => {
  const x = v / 255
  return x <= 0.04045 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4)
}
const toSRGB = (v: number) => {
  const x = Math.max(0, Math.min(1, v))
  return Math.round((x <= 0.0031308 ? x * 12.92 : 1.055 * Math.pow(x, 1 / 2.4) - 0.055) * 255)
}
const signPow = (v: number, e: number) => Math.sign(v) * Math.pow(Math.abs(v), e)

export function decodeBlurhash(hash: string, width: number, height: number): Uint8ClampedArray | null {
  if (hash.length < 6) return null
  const size = decode83(hash[0])
  const nx = (size % 9) + 1
  const ny = Math.floor(size / 9) + 1
  if (!Number.isFinite(size) || hash.length !== 4 + 2 * nx * ny) return null

  const maxAC = (decode83(hash[1]) + 1) / 166
  const colors: [number, number, number][] = []
  for (let i = 0; i < nx * ny; i++) {
    if (i === 0) {
      const v = decode83(hash.slice(2, 6))
      if (!Number.isFinite(v)) return null
      colors.push([toLinear(v >> 16), toLinear((v >> 8) & 255), toLinear(v & 255)])
    } else {
      const v = decode83(hash.slice(4 + i * 2, 6 + i * 2))
      colors.push([
        signPow((Math.floor(v / 361) - 9) / 9, 2) * maxAC,
        signPow(((Math.floor(v / 19) % 19) - 9) / 9, 2) * maxAC,
        signPow(((v % 19) - 9) / 9, 2) * maxAC,
      ])
    }
  }
  if (colors.some((c) => c.some((n) => !Number.isFinite(n)))) return null

  const px = new Uint8ClampedArray(width * height * 4)
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      let r = 0
      let g = 0
      let b = 0
      for (let j = 0; j < ny; j++) {
        for (let i = 0; i < nx; i++) {
          const basis = Math.cos((Math.PI * x * i) / width) * Math.cos((Math.PI * y * j) / height)
          const c = colors[i + j * nx]
          r += c[0] * basis
          g += c[1] * basis
          b += c[2] * basis
        }
      }
      const o = 4 * (x + y * width)
      px[o] = toSRGB(r)
      px[o + 1] = toSRGB(g)
      px[o + 2] = toSRGB(b)
      px[o + 3] = 255
    }
  }
  return px
}
