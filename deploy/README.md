# deploy — decisions, not configuration

**The configuration lives in `construct-server`, not here.** That repo's
`docker-compose.yml` declares every service on the box inline, and every router
lives in its `config/traefik/dynamic/routers.yml`. Nothing is assembled from
fragments in service repos at deploy time.

This directory holds the two things that are genuinely Catenary's: the
`provision.sql` that must run once by hand, and the decisions below that
whoever writes Catenary's block in `construct-server` needs and cannot infer
from the code.

**It deliberately does not hold a copy of that block.** An earlier revision of
this ticket did, on Chronicle's pattern — and Chronicle's copy has already
diverged from what is deployed: it carries `CHRONICLE_ASR_MODEL` and
`CHRONICLE_TRANSCRIBE_INTERVAL`, the live block does not, and nothing checks
that the two agree. Two documents making the same claim is the CHRN-79 shape,
and the second copy is the one that goes stale.

## Before anything is deployed

1. **`deploy/provision.sql`**, once, by hand, as a superuser. CANT-13 Ruling 3.
   Deliberately not a migration: migrations run as the `catenary` role, and a
   role cannot create itself.
2. **`CATENARY_DB_PASSWORD` reaches `.env` via Signet.** The password never
   appears in this repo.
3. **An image exists.** It does not — see *Blocked on*, below.

## Decisions

**Port 4012.** The next free slot in the estate's 40xx block; `DefaultPort` in
`internal/config`. Overridable with `CATENARY_PORT`.

**Use the keyword/value DSN form, not the URL form.** pgx accepts
`host=postgres user=catenary password=… dbname=catenary sslmode=disable`
through `CATENARY_DATABASE_URL` just as it accepts a URL, and the URL form
misparses a Signet-generated password containing `@`, `/`, `#`, `?` or `%` —
`@` in particular splits the authority in the wrong place, so the failure names
the wrong host rather than the password. `config.Load` hands the string to pgx
untouched, so nothing upstream would catch it.

**Health-check `/healthz`, never `/readyz`.** `/healthz` takes no dependencies;
`/readyz` pings Postgres. Restarting the container is the wrong remedy for a
database blip, and it is worse here than elsewhere: every restart severs every
open WebSocket at once and the clients all reconnect together.

**The health-check and the image must be chosen together.** A `wget`-based
check requires `wget` in the final image. If CANT-78 builds the usual
`scratch`/distroless single-binary image, such a check can never pass, the
container stays `unhealthy` forever, and anything gating on `service_healthy`
stalls behind it. Either the image carries a shell and `wget`, or the binary
grows a `healthcheck` subcommand — the choice belongs in CANT-78, in one place.

**`stop_grace_period` must exceed `CATENARY_SHUTDOWN_GRACE`** (default 20s, and
Docker's default stop timeout is 10s), or a redeploy kills the process while it
is still draining in-flight sends. Since CANT-107 the grace also covers the
socket drain: on SIGTERM every open session is closed with `1001 going away`,
concurrently with the REST shutdown, so clients reconnect deliberately rather
than on a TCP reset.

**One Postgres connection outside the pool, for the life of the process.** The
NOTIFY listener (CANT-21) holds its own connection, because `LISTEN` registers
against a session and a pooled connection handed back is a registration lost.
CANT-107 is the first process that actually opens it, so the role's connection
budget is the pool's maximum plus one — and the listener reconnects through a
Postgres restart on its own backoff, raising a gap to every attached session
when it does.

**No `ports:` mapping.** Catenary is reached through Traefik. Publishing a port
would put a listener on the host that bypasses the edge — and until the three
tickets named below land, what is behind that listener is not something to put
on the open internet.

**The provisioning port is `expose`d on `construct_net` and NEVER under `ports:`, and no router, service URL or label points at it.** CANT-131 adds a SECOND listener to this process — its own `http.Server`, its own mux, serving `POST /accounts`, `GET /accounts` and `POST /accounts/{id}/deactivate` and nothing else — which Purser's `catenary` connector calls to create, look up and deactivate accounts. The rule above about the host applies to it with more force, not less: `expose` makes it reachable from `construct_net`, which is all Purser needs, and a `ports:` entry would put account creation on the host. There is also no Traefik router for it, no `traefik.http.services.*.loadbalancer.server.port` naming it, and no entry in `config/traefik/dynamic/routers.yml` — publishing it has to be a deliberate act rather than the absence of a deny rule, which is the honest version of ruling 1: the port is not structurally unreachable, and what keeps the door safe is the credential plus a listener that serves nothing else.

**Why that matters more than it does for 4012.** The credential on that port can obtain an enrollment token for any person in the service, and an enrollment token redeems into a device enrolled as them — every conversation they are in, every message in it. Re-invite is the only way anyone ever gets a second device, so the surface cannot refuse it. It is the most powerful credential Catenary has, and the only one that grants nothing in the database: rotating it is a Signet change and a restart of both services.

**Catenary's two variables, and it is both or neither.**

| variable | what it is |
|---|---|
| `CATENARY_PROVISION_ADDR` | the provisioning listener's address, e.g. `:4013`. Unset means the surface does not exist — no listener, no route |
| `CATENARY_PROVISION_TOKEN` | the service credential, at least 32 bytes, presented as `Authorization: Bearer`. Compared constant-time over SHA-256 digests; never logged |

One without the other **refuses to boot**, with a message naming the missing variable, and so does a token under 32 bytes — a listener with no credential would serve account creation to anything on `construct_net`, and a credential with no listener is a deployment that believes it has a provisioning surface and does not. There is deliberately **no default port** for this one, unlike 4012: a default would mean a deployment could open this door by forgetting something rather than by choosing it. `4013` is the next free slot in the estate's 40xx block at the time of writing, and `construct-server`'s own `.env` is the authority on that.

**Purser's two variables**, set on the `purser` service in the same compose file: `PURSER_CATENARY_BASE_URL` (`http://catenary:4013`, the `expose`d port on `construct_net`) and `PURSER_CATENARY_PROVISION_TOKEN` (the same secret). With them unset the connector registers itself `Unavailable`, as Purser's other connectors do, so a Catenary that is not deployed yet is not an error there.

**A person generates the secret in Signet, and nobody else.** It never appears in this repository, in a compose file, or in a ticket — the same rule `CATENARY_DB_PASSWORD` already follows. Both services read the identical value, so rotating it is one Signet change and a restart of both; there is no rotation window in which one of them holds the old one.

**The `construct-server` change is SERV-202.** SERV-168, which onboarded Catenary, is closed, so the second `expose`d port, Catenary's two variables and Purser's two have their own ticket rather than nobody's. **And it is a change over there, not a fragment here**: `deploy/` holds no copy of Catenary's compose block and must not gain one, for the reason at the top of this file — Chronicle's copy has already diverged from what is deployed, and the second copy is the one that goes stale. What lives here is this decision.

**The contract is `provision/openapi.yaml`, versioned by the tag `provision-v1`.** It is not part of `schema/`, no generator reads it, and a test in `server/spec` drives every operation against the real handlers and fails when they disagree with it. PRSR-50 pins the tag, which is invariant 4's arrangement for the shared ASR service pointed the other way round.

## The routing split, and why it is not symmetric

**`/admin` behind Access; the socket and `/sync` never.** Cloudflare Access is a
browser SSO wall, and a Flutter client holding a bearer token cannot open one —
that is D2, and it is why the socket cannot simply live on the gated host.

**`/admin` needs its own higher-priority router pointed at the deny pair, not a
Host match.** A public router matching on Host alone serves `/admin` from the
public host too, which is precisely what CANT-16's `Done when` forbids.
Chronicle's routers file records hitting exactly this.

**There should be no public router at all until CANT-22, CANT-28 and CANT-29 land.** The bar for the `public` entrypoint on this estate is that the endpoint authenticates with something Access cannot express.

**This used to name two tickets, and CANT-28 found that it is short by one.** CANT-28 lands the credential model and turns `GET /sync` on, so after it the service does authenticate — but a rotating refresh token whose replay is merely refused, rather than invalidating the whole family, leaves an attacker who rotated first holding live credentials while the real device's refusal looks like an ordinary bug. That is the case CANT-29 exists for and the one it calls *"the one that hands over the whole archive"*, and no ruling on CANT-28 could close it: the plan worked the bar through every branch of where the refresh handler lands and the answer came out the same each time. So the third name is not a nicety.

Until all three, a public router would put a service that holds everyone's messages on the open 443 either unauthenticated or with an undetectable credential replay.

**All three have landed.** CANT-22 (the socket door and its handshake), CANT-28 (the credential model, and `GET /sync` turned on behind it) and CANT-29 (reuse detection: a replayed refresh token invalidates the whole family, revokes what it buys, severs the live socket and records the event). The condition this section states — *"the endpoint authenticates with something Access cannot express"* — is met, and a credential replay is no longer undetectable.

**That unblocks the decision; it does not make it.** Standing a public router up is an operator's call and wants its own read of what is exposed, including the one thing reuse detection deliberately does not catch: a replay landing within CANT-29's ten-second grace window escapes invalidation, which is the trade that keeps a waking phone's two in-flight refreshes from logging a real person out. `docs/decisions/cant-29-reuse-detection.md` carries the reasoning and the exposure.

**Every router on the `internal` entrypoint carries `cf-access-jwt`** (SERV-106),
and adding one requires a matching `CF_ACCESS_AUD_MAP` entry on the guard —
`check-edge-auth.sh` fails a gated host with no AUD entry rather than serving it
unmapped.

## What R1 did and did not measure

R1 held one socket for 1h20m at a flat 9 ms RTT and the WebSocket architecture
stands on it. **It did not measure the deployed path.** Its own findings say so:

> a **quick tunnel is not the production named tunnel**. Same Cloudflare edge
> and the same idle-timeout behaviour — which is the risk under test — but the
> deployed path adds Traefik and the split-entrypoint setup. Re-running the idle
> test once through the real path is cheap and worth doing before P1 depends on
> it.

Traefik and the split entrypoint are exactly what this deployment adds, and the
internal router runs the upgrade through `cf-access-jwt` besides. **The
re-test R1 asked for has not been run.** CANT-22 landed the socket it needs
(`GET /ws`, authenticated on `Sec-WebSocket-Protocol`, R1's 35 s / 2 dial
announced in `ready`), so there is now something to run it with; the run itself
needs the deployed path, which does not exist until CANT-78 publishes an image.
It stays carried on the CANT-22 thread until a ticket that has that path takes
it.

## Blocked on

| clause of CANT-16's `Done when` | blocker |
|---|---|
| reachable through the tunnel | **CANT-78** — nothing publishes `ghcr.io/einlanzerous/catenary`; no Dockerfile, no release workflow |
| the WebSocket upgrade survives it | **CANT-78** — the socket exists (`GET /ws`, CANT-22) and holds in-process; whether it survives the tunnel and Traefik is the R1 re-test above, which needs a deployed image to run against |
| `/admin` behind Access, `/sync` and the socket not | **CANT-69** (`/admin`) and **CANT-18** (`/sync`) — neither route exists |

Catenary's block should not be added to `construct-server` before CANT-78
publishes a tag: a service that cannot pull its image is a guaranteed red
deploy.
