/* The Node transport driver (CANT-35 ruling 0 → A): the TypeScript transport,
 * run as a child process and controlled over stdio, so soakrig and
 * cmd/catenary's kill-test and restore-test rigs can run it as a client beside
 * the Go one and judge its journal with the same, unchanged `client.Compare`.
 * The Go half is `tsDriver` in server/cmd/soakrig/tsdriver.go.
 *
 * THE PROTOCOL is line-delimited JSON. Each request is one line,
 *
 *     {"id": 7, "cmd": "send", "args": {...}}
 *
 * and each is answered by exactly one line carrying the same id, either
 * `{"id": 7, "ok": true, "result": {...}}` or `{"id": 7, "ok": false, "error":
 * {"kind": "...", "message": "...", "frame": {...}}}`. Requests are handled
 * concurrently, so a `send` awaiting its ack never holds up a `status`. Nothing
 * else is ever written to stdout; the transport's own log, when asked for,
 * goes to stderr.
 *
 * NINE COMMANDS, and no more:
 *
 *   start      {baseUrl, credential: {userId, deviceId, accessToken}, faults,
 *              backoffMinMs, backoffMaxMs, clientVersion, log} — builds a
 *              transport over this process's one journal and starts it. A
 *              second `start` after a `stop` is a NEW transport over the SAME
 *              journal, which is Go's "restart with a new Client over
 *              Journal()"; its stats begin at zero, as a new Go client's do.
 *   send       {frame: ClientSend, wire JSON} → {ack: ServerAck, wire JSON};
 *              refused with the kind NotConnected, SessionEnded, SendInFlight
 *              or SendRefused (which carries the `error` frame, wire JSON).
 *   read       {frame: ClientRead, wire JSON} → {}; written on the session when
 *              the transport's `status.ready` is true and refused with the
 *              kind NotConnected otherwise, before `start` included. THE REFUSAL IS THE DRIVER'S:
 *              `Transport.read` drops a frame silently when there is no
 *              connection, and a frame written between the upgrade and
 *              `ready` is not one the server has agreed to take (CANT-46).
 *   sever      drops the socket through the proxy (proxy.ts): no close frame.
 *   blackhole  silences the socket through the proxy, and leaves it open.
 *   catchup    `Transport.catchUp()`: a trigger.
 *   status     {status: TransportStatus, journalWipes}.
 *   snapshot   the journal, wire JSON: {cursor, messages, conversations, users,
 *              counted, wipes}. `counted` and `wipes` are what `client.Compare`
 *              reads beyond the store (`MemoryJournal.counted()`, `.wipes()`).
 *   stop       `Transport.stop()`: a clean close; the journal is kept.
 *
 * `Kill` IS NOT A COMMAND. It is SIGKILL on this process, sent from Go, and
 * nothing here can observe it — which is the point. Closing stdin ends the
 * process too, so a rig that dies without cleaning up leaves no driver behind.
 *
 * THE JOURNAL is in memory (`MemoryJournal`) and dies with the process, unless
 * the driver is launched with `--journal=<file>` (CANT-169): then it is the
 * durable `IdbJournal` over fake-indexeddb, persisted to that file after every
 * completed transaction (persist.ts), and a driver launched again over the same
 * file — after a SIGKILL — resumes from what the dead one had committed. That
 * is the TypeScript `clientDies` lane's relaunch.
 *
 * THE CREDENTIAL IS HELD (`heldCredential`, Go's `Refresh: false`), which is
 * what the rigs run (CANT-31 criterion 39). CANT-152's credential layer is not
 * needed by any lane here: no run outlives the access token it enrolled with.
 */

import readline from 'node:readline'
import {
  decodeClientRead,
  decodeClientSend,
  encodeConversation,
  encodeMessage,
  encodeServerAck,
  encodeServerError,
  encodeUser,
} from '@/wire/generated'
import { heldCredential, type Credential } from '../credential'
import { type Faults, NO_FAULTS } from '../faults'
import { MemoryJournal, type StagedJournal } from '../journal'
import { openFileJournal } from './persist'
import { type Logger, type WebSocketCtor, manualLifecycle, silentLogger } from '../seams'
import type { TransportStatus } from '../status'
import { type Transport, SendRefused, createTransport } from '../transport'
import { type TcpProxy, startProxy } from './proxy'

interface Request {
  id: number
  cmd: string
  args?: Record<string, unknown>
}

interface StartArgs {
  baseUrl: string
  credential: Credential
  faults?: Partial<Faults>
  backoffMinMs?: number
  backoffMaxMs?: number
  clientVersion?: string
  log?: boolean
}

/** A refusal the Go half maps back onto its own error values. */
class DriverError extends Error {
  constructor(
    readonly kind: string,
    message: string,
    readonly frame?: Record<string, unknown>,
  ) {
    super(message)
  }
}

export interface DriverIO {
  input: NodeJS.ReadableStream
  write(line: string): void
  log(line: string): void
  /** Called once stdin has ended and the driver has shut its proxy. */
  exit(): void
}

/** Resolves once `pred` holds over the transport's status, or after `ms`. */
function until(t: Transport, pred: (s: TransportStatus) => boolean, ms: number): Promise<void> {
  return new Promise((resolve) => {
    if (pred(t.status())) return resolve()
    const done = () => {
      clearTimeout(timer)
      off()
      resolve()
    }
    const timer = setTimeout(done, ms)
    const off = t.subscribe((s) => {
      if (pred(s)) done()
    })
  })
}

export interface ServeOptions {
  /** `--journal=<file>`: the durable journal persisted there. Absent is a
   *  journal in memory. */
  journalFile?: string
}

/** Runs the driver until its input ends. */
export function serve(io: DriverIO, opts: ServeOptions = {}): void {
  // Every request waits for the journal: reading a file back is not instant,
  // and nothing may run over a journal that is not open yet.
  const opened: Promise<StagedJournal> = opts.journalFile ? openFileJournal(opts.journalFile) : Promise.resolve(new MemoryJournal())
  let journal: StagedJournal = new MemoryJournal()
  let transport: Transport | null = null
  let proxy: TcpProxy | null = null
  let proxyTarget = ''

  const stderrLogger: Logger = {
    info: (msg, fields) => io.log(JSON.stringify({ level: 'info', msg, ...fields })),
    warn: (msg, fields) => io.log(JSON.stringify({ level: 'warn', msg, ...fields })),
  }

  const need = (): Transport => {
    if (transport === null) throw new DriverError('NotStarted', 'driver: no transport is running; send start first')
    return transport
  }

  const handlers: Record<string, (args: Record<string, unknown>) => Promise<unknown>> = {
    async start(raw) {
      if (transport !== null) throw new DriverError('AlreadyStarted', 'driver: a transport is already running')
      const args = raw as unknown as StartArgs
      const base = new URL(args.baseUrl)
      const targetPort = Number(base.port || (base.protocol === 'https:' ? 443 : 80))
      const target = `${base.hostname}:${targetPort}`
      if (proxy === null) {
        proxy = await startProxy(base.hostname, targetPort)
        proxyTarget = target
      } else if (proxyTarget !== target) {
        throw new DriverError('BadArgs', `driver: this driver proxies ${proxyTarget}, not ${target}`)
      }
      const port = proxy.port
      // THE SEAM THE PROXY SITS BEHIND. The transport builds its URL from
      // `baseUrl` as it would in a browser; only the host it connects to moves.
      const Proxied = function (url: string, protocols: string[]) {
        const u = new URL(url)
        u.hostname = '127.0.0.1'
        u.port = String(port)
        return new globalThis.WebSocket(u.toString(), protocols)
      } as unknown as WebSocketCtor
      const t = createTransport({
        baseUrl: args.baseUrl,
        credential: heldCredential(args.credential),
        journal,
        clientVersion: args.clientVersion ?? 'soakrig-driver',
        WebSocket: Proxied,
        lifecycle: manualLifecycle(),
        logger: args.log ? stderrLogger : silentLogger,
        backoffMinMs: args.backoffMinMs,
        backoffMaxMs: args.backoffMaxMs,
        faults: { ...NO_FAULTS, ...args.faults },
      })
      transport = t
      t.start()
      return {}
    },

    async send(args) {
      const t = need()
      const frame = decodeClientSend(args.frame)
      try {
        return { ack: encodeServerAck(await t.send(frame)) }
      } catch (e) {
        if (e instanceof SendRefused) throw new DriverError('SendRefused', e.message, encodeServerError(e.frame))
        if (e instanceof Error) throw new DriverError(e.name, e.message)
        throw e
      }
    },

    async read(args) {
      const frame = decodeClientRead(args.frame)
      // Not `need()`: before `start` there is no session either, and the Go
      // half reads one refusal for both, as `(*Client).Read` gives one.
      const t = transport
      if (t === null || !t.status().ready) throw new DriverError('NotConnected', 'driver: read without a ready session')
      t.read(frame)
      return {}
    },

    async sever() {
      const severed = proxy?.sever() ?? 0
      // ANSWERED ONCE THE TRANSPORT HAS SEEN IT, so a rig that severs and then
      // waits for `ready` cannot read the old session's `ready` and move on
      // before the drop has even arrived. Bounded: a socket the proxy did not
      // carry (none open) has nothing to wait for.
      const t = transport
      if (severed > 0 && t !== null) await until(t, (s) => !s.connected, 5_000)
      return { severed }
    },

    async blackhole() {
      return { holed: proxy?.blackhole() ?? 0 }
    },

    async catchup() {
      need().catchUp()
      return {}
    },

    async status() {
      return { status: need().status(), journalWipes: journal.wipes() }
    },

    async snapshot() {
      const s = journal.snapshot()
      return {
        cursor: s.cursor,
        messages: s.messages.map(encodeMessage),
        conversations: s.conversations.map(encodeConversation),
        users: s.users.map(encodeUser),
        counted: journal.counted(),
        wipes: journal.wipes(),
      }
    },

    async stop() {
      transport?.stop()
      transport = null
      return {}
    },
  }

  const answer = (id: number, body: Record<string, unknown>) => io.write(JSON.stringify({ id, ...body }))

  const rl = readline.createInterface({ input: io.input, crlfDelay: Infinity })
  rl.on('line', (line) => {
    if (line.trim() === '') return
    let req: Request
    try {
      req = JSON.parse(line) as Request
    } catch {
      io.log(JSON.stringify({ level: 'warn', msg: 'driver: a request that is not JSON; ignored' }))
      return
    }
    const h = handlers[req.cmd]
    if (h === undefined) {
      answer(req.id, { ok: false, error: { kind: 'UnknownCommand', message: `driver: unknown command ${req.cmd}` } })
      return
    }
    opened.then((j) => {
      journal = j
      return h(req.args ?? {})
    }).then(
      (result) => answer(req.id, { ok: true, result }),
      (e: unknown) => {
        const err =
          e instanceof DriverError
            ? { kind: e.kind, message: e.message, ...(e.frame ? { frame: e.frame } : {}) }
            : { kind: 'Error', message: e instanceof Error ? e.message : String(e) }
        answer(req.id, { ok: false, error: err })
      },
    )
  })
  rl.on('close', () => {
    transport?.stop()
    proxy?.close()
    io.exit()
  })
}
