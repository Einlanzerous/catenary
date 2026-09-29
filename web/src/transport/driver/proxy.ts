/* The driver's loopback TCP proxy (CANT-35's plan, *Soak harness*).
 *
 * WHY IT EXISTS: a browser `WebSocket` has no drop-without-handshake call.
 * Node 24's global one has no `terminate`, and `close()` runs the close
 * handshake, so Go's `Sever` — `CloseNow`, "the same as a network drop" —
 * cannot be reproduced on the socket itself. The driver's `WebSocketCtor` seam
 * dials this proxy instead of the server, and the proxy forwards every byte.
 * That puts the network, rather than the socket API, in the driver's hands.
 *
 *   sever      destroys both TCP legs of every connection, with no WebSocket
 *              close frame on either: the transport sees an abnormal closure
 *              (1006, counted under -1), the branch the Go storm exercises.
 *   blackhole  stops forwarding in both directions on every connection open
 *              now, and keeps both legs open. Nothing crosses — not a frame,
 *              not a FIN — which is a real half-dead socket, the one way to
 *              make the heartbeat sever against a real server. Connections
 *              made afterwards forward normally, so the redial that follows the
 *              heartbeat's sever gets through.
 *
 * `/sync` never comes through here: it goes direct, as Go's `Sever` leaves it
 * untouched.
 */

import net from 'node:net'

interface Pair {
  down: net.Socket
  up: net.Socket
  holed: boolean
}

export interface TcpProxy {
  /** The loopback port the proxy listens on. */
  readonly port: number
  /** Drops every connection. Returns how many there were. */
  sever(): number
  /** Silences every connection open now. Returns how many there were. */
  blackhole(): number
  close(): void
}

export function startProxy(targetHost: string, targetPort: number): Promise<TcpProxy> {
  const pairs = new Set<Pair>()
  const server = net.createServer((down) => {
    const up = net.connect(targetPort, targetHost)
    const pair: Pair = { down, up, holed: false }
    pairs.add(pair)
    // FORWARDED BY HAND RATHER THAN piped, so a black hole can stop the bytes
    // without closing anything: `unpipe` would leave the stream paused and the
    // data queued, and an end propagated by `pipe` is exactly the FIN a black
    // hole must swallow.
    down.on('data', (d) => {
      if (!pair.holed) up.write(d)
    })
    up.on('data', (d) => {
      if (!pair.holed) down.write(d)
    })
    // A black-holed pair passes on no close either, and stays listed until both
    // legs are gone, so a later `sever` still reaches the one left open.
    const end = (other: net.Socket) => () => {
      if (pair.holed) {
        if (pair.down.destroyed && pair.up.destroyed) pairs.delete(pair)
        return
      }
      pairs.delete(pair)
      other.destroy()
    }
    down.on('close', end(up))
    up.on('close', end(down))
    // An error is followed by 'close', which is where the pair ends.
    down.on('error', () => {})
    up.on('error', () => {})
  })
  return new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address()
      if (addr === null || typeof addr === 'string') {
        reject(new Error('proxy: no TCP address'))
        return
      }
      resolve({
        port: addr.port,
        sever() {
          const n = pairs.size
          for (const p of [...pairs]) {
            pairs.delete(p)
            p.down.destroy()
            p.up.destroy()
          }
          return n
        },
        blackhole() {
          let n = 0
          for (const p of pairs) {
            if (p.holed) continue
            p.holed = true
            n++
          }
          return n
        },
        close() {
          server.close()
          for (const p of [...pairs]) {
            p.down.destroy()
            p.up.destroy()
          }
          pairs.clear()
        },
      })
    })
  })
}
