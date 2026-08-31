# Missed-pong severance, exercised through the tunnel

The dangerous failure R1 names is the SILENT one: a mobile network drops TCP without a FIN, both ends still believe the socket is open, and the client shows no error while receiving nothing. You cannot reproduce that by killing a process — that is a clean close the client notices at once. The server has to go deaf while staying connected, which is what `?deaf_after=<sec>` does.

Run: server heartbeat 10 s, missed_pong_limit 2, server goes deaf at t=15 s.

```
01:21:04 ready · heartbeat=10s · missed_limit=2
01:21:14 alive 10s · outstanding_pings=0 · last_rtt=0s
01:21:24 alive 20s · outstanding_pings=0 · last_rtt=16ms     <- deaf at t=15s
01:21:34 alive 30s · outstanding_pings=1 · last_rtt=16ms
01:21:44 alive 40s · outstanding_pings=2 · last_rtt=16ms
01:21:44 socket closed after 40s: severed: 2 unanswered pings (limit 2)
01:21:44 reconnect #1 in 2s
01:21:46 ready · resumed=true                                <- healthy again
01:21:56 alive 10s · outstanding_pings=0 · last_rtt=8ms
```

The client severed itself deliberately rather than waiting for the OS, and resumed from its cursor 2 s later. (The second connection also goes deaf, because `deaf_after` is per-connection — hence outstanding_pings climbing again at 01:22:16.)

## The number this puts a name to

Detection latency = heartbeat x (missed_pong_limit + 1), roughly. Here 10 s x 2.5 = 25 s from going deaf to severing.

**At the production 35 s heartbeat that is 70-105 s** — the window in which a client still believes it is connected and is not. That is under Cloudflare's ~100 s idle timeout, which is what the interval was chosen for, but it is worth stating plainly because it is the real cost of the heartbeat interval: a longer interval is cheaper on battery and radio, and proportionally slower to notice a dead socket. If 105 s of false confidence is too long, the interval comes down and the battery cost goes up. It is a dial, not a constant — and per R4 it is server-driven, so it can be turned without shipping a client.
