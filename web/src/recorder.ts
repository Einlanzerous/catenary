/**
 * The recorder behind RECORD (CANT-315, under CANT-247).
 *
 * WHAT IT HANDS BACK IS WHAT THE BROWSER RECORDED. `MediaRecorder` output
 * (webm/opus on Chromium and Firefox, mp4 on Safari) goes to the uploader as
 * bytes, untouched: the server decides what a blob is from its bytes
 * (`media.ContentType` already knows `audio/webm`, `audio/mp4`, `audio/ogg`),
 * and the transcription service decodes, because doing that once beats doing it
 * in every client.
 *
 * `durationMs` IS THE RECORDER'S OWN CLOCK, for local rendering only, and THERE
 * IS NO `peaks` ANYWHERE HERE, on purpose (Invariant 3): peaks are computed
 * once, server-side, by CANT-56, because the seeded generator overflows 2^53 and
 * draws different bars in JavaScript than in Dart.
 *
 * THE GATE. A voice upload succeeds today and then cannot be SENT: a `voice`
 * row resolves only once the server has filled its peaks and duration, which
 * CANT-56 builds. Until then a voice send is refused `upload_not_found`, the
 * outbox spends its one automatic re-upload, and the entry fails: a person
 * would record a note and watch it fail. So `VOICE_SEND_ENABLED` is off, REC is
 * drawn disabled, and no entry is composed (CANT-247 ruling 0 → B). The
 * capture, the upload path and the entry are all built and exercised against a
 * live server by a recorder that is switched on in the test; CANT-56 flips this
 * one constant and adds the end-to-end send proof.
 */

/** THE ONE GATE CANT-56 FLIPS. */
export const VOICE_SEND_ENABLED = false

/** One take. `stop()` ends it and returns what was recorded; `cancel()` throws
 *  it away. Exactly one of them is called. */
export interface Capture {
  stop(): Promise<{ blob: Blob; durationMs: number }>
  cancel(): void
}

export interface Recorder {
  /** Whether RECORD is offered at all. Off until CANT-56. */
  readonly enabled: boolean
  /** Ask for the microphone and start. Rejects with `RecorderRefused`, whose
   *  message is for the person. */
  start(): Promise<Capture>
}

/** A refusal whose text is user copy, shown inline. */
export class RecorderRefused extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'RecorderRefused'
  }
}

export const RECORDER_MESSAGES = {
  denied: 'Microphone access was blocked. Allow it for this site in the browser, then try again.',
  noMicrophone: 'No microphone was found on this device.',
  unavailable: "This browser can't record audio.",
  failed: 'Recording could not start. Try again.',
} as const

/** In preference order. What the browser supports first wins; the browser's own
 *  choice (no `mimeType`) is the last resort. */
export const RECORDER_MIME_TYPES = ['audio/webm;codecs=opus', 'audio/ogg;codecs=opus', 'audio/mp4'] as const

export interface BrowserRecorderSeams {
  mediaDevices?: Pick<MediaDevices, 'getUserMedia'> | undefined
  MediaRecorder?: typeof MediaRecorder | undefined
  /** The recorder's clock, ms. Default `performance.now`. */
  now?: () => number
}

export function browserRecorder(enabled: boolean, seams: BrowserRecorderSeams = {}): Recorder {
  return {
    enabled,
    async start(): Promise<Capture> {
      const devices = 'mediaDevices' in seams ? seams.mediaDevices : globalThis.navigator?.mediaDevices
      const Ctor = 'MediaRecorder' in seams ? seams.MediaRecorder : globalThis.MediaRecorder
      const now = seams.now ?? (() => globalThis.performance.now())
      if (!devices || typeof devices.getUserMedia !== 'function' || !Ctor) throw new RecorderRefused(RECORDER_MESSAGES.unavailable)

      let stream: MediaStream
      try {
        stream = await devices.getUserMedia({ audio: true })
      } catch (e) {
        const name = e instanceof Error ? e.name : ''
        if (name === 'NotAllowedError' || name === 'SecurityError' || name === 'PermissionDeniedError') {
          throw new RecorderRefused(RECORDER_MESSAGES.denied)
        }
        if (name === 'NotFoundError' || name === 'DevicesNotFoundError' || name === 'OverconstrainedError') {
          throw new RecorderRefused(RECORDER_MESSAGES.noMicrophone)
        }
        throw new RecorderRefused(RECORDER_MESSAGES.failed)
      }
      const release = () => stream.getTracks().forEach((t) => t.stop())

      const mimeType = RECORDER_MIME_TYPES.find((t) => typeof Ctor.isTypeSupported === 'function' && Ctor.isTypeSupported(t))
      let recorder: MediaRecorder
      try {
        recorder = new Ctor(stream, mimeType ? { mimeType } : undefined)
      } catch {
        release()
        throw new RecorderRefused(RECORDER_MESSAGES.failed)
      }
      const chunks: Blob[] = []
      recorder.ondataavailable = (e) => {
        if (e.data && e.data.size > 0) chunks.push(e.data)
      }
      const startedAt = now()
      try {
        recorder.start()
      } catch {
        release()
        throw new RecorderRefused(RECORDER_MESSAGES.failed)
      }

      return {
        stop: () =>
          new Promise((resolve, reject) => {
            const stoppedAt = now()
            recorder.onstop = () => {
              release()
              resolve({
                blob: new Blob(chunks, { type: recorder.mimeType || mimeType || '' }),
                durationMs: Math.max(0, Math.round(stoppedAt - startedAt)),
              })
            }
            recorder.onerror = () => {
              release()
              reject(new RecorderRefused(RECORDER_MESSAGES.failed))
            }
            try {
              recorder.stop()
            } catch {
              release()
              reject(new RecorderRefused(RECORDER_MESSAGES.failed))
            }
          }),
        cancel() {
          recorder.ondataavailable = null
          recorder.onstop = null
          try {
            if (recorder.state !== 'inactive') recorder.stop()
          } catch {
            // already stopped
          }
          release()
        },
      }
    },
  }
}

/** The shipped recorder: the browser's, behind the gate. */
export const shippedRecorder = (): Recorder => browserRecorder(VOICE_SEND_ENABLED)
