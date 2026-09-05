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
is still draining in-flight sends.

**No `ports:` mapping.** Catenary is reached through Traefik. Publishing a port
would put a listener on the host that bypasses the edge — and until CANT-22 and
CANT-28 land there is no in-process authentication behind that listener at all.

## The routing split, and why it is not symmetric

**`/admin` behind Access; the socket and `/sync` never.** Cloudflare Access is a
browser SSO wall, and a Flutter client holding a bearer token cannot open one —
that is D2, and it is why the socket cannot simply live on the gated host.

**`/admin` needs its own higher-priority router pointed at the deny pair, not a
Host match.** A public router matching on Host alone serves `/admin` from the
public host too, which is precisely what CANT-16's `Done when` forbids.
Chronicle's routers file records hitting exactly this.

**There should be no public router at all until CANT-22 and CANT-28 land.** The
bar for the `public` entrypoint on this estate is that the endpoint
authenticates with something Access cannot express. Catenary does not
authenticate at all yet, so a public router today would put a service that will
hold everyone's messages, unauthenticated, on the open 443.

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
re-test R1 asked for has not been run**, and is carried on CANT-22, which is the
ticket that first has a socket to run it with.

## Blocked on

| clause of CANT-16's `Done when` | blocker |
|---|---|
| reachable through the tunnel | **CANT-78** — nothing publishes `ghcr.io/einlanzerous/catenary`; no Dockerfile, no release workflow |
| the WebSocket upgrade survives it | **CANT-22** — there is no socket. The service serves `/healthz` and `/readyz` |
| `/admin` behind Access, `/sync` and the socket not | **CANT-69** (`/admin`) and **CANT-18** (`/sync`) — neither route exists |

Catenary's block should not be added to `construct-server` before CANT-78
publishes a tag: a service that cannot pull its image is a guaranteed red
deploy.
