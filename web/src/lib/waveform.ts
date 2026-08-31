/* Waveform peaks.
 *
 * Deliberate call 13: "amplitude peaks precomputed server-side and stored with
 * the message so the bar pattern is identical in web and Flutter". The client
 * therefore *renders* a stored array and never derives one — see the Waveform
 * component, which takes `peaks` as data.
 *
 * `peaksFromSeed` below reproduces the design canvas's generator so the mock
 * corpus draws the same bars the canvas does. It is fixture code and belongs
 * to the mock layer only.
 *
 * A note for whoever ports this: the generator is NOT portable as written.
 * `s * 1103515245` with s near 2^31 lands around 2^61, past the 2^53 where a
 * JS double stops being an exact integer — so JS silently rounds where Dart's
 * 64-bit int would not, and the two produce different bars from the same seed.
 * That does not matter here (fixtures only) and would matter enormously if
 * anyone "helpfully" moved generation into the clients. Peaks come from the
 * server.
 */

/** Reproduces the canvas generator. Returns bar heights as percentages. */
export function peaksFromSeed(seed: number, n: number): number[] {
  let s = seed
  const out: number[] = []
  for (let i = 0; i < n; i++) {
    s = (s * 1103515245 + 12345) % 2147483648
    const r = s / 2147483648
    // Envelope: quiet at both ends, fullest in the middle — a spoken clip.
    const env = 0.35 + 0.65 * Math.sin((i / (n - 1)) * Math.PI)
    out.push(Math.max(9, Math.round((0.22 + 0.78 * r) * env * 100)))
  }
  return out
}

/** Resample a stored peak array to the bar count a given width affords. */
export function resample(peaks: number[], n: number): number[] {
  if (peaks.length === 0 || n <= 0) return []
  if (peaks.length === n) return peaks
  const out: number[] = []
  for (let i = 0; i < n; i++) {
    const start = Math.floor((i * peaks.length) / n)
    const end = Math.max(start + 1, Math.floor(((i + 1) * peaks.length) / n))
    let peak = 0
    for (let j = start; j < end && j < peaks.length; j++) {
      if (peaks[j] > peak) peak = peaks[j]
    }
    out.push(peak)
  }
  return out
}
