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

**The image carries the web client, and nothing in `construct-server` routes it separately.** `deploy/Dockerfile` builds `web/` in a Node stage and the Go stage embeds the result, so `GET /` on 4012 is the client and the one Host-only router already reaches it. It has to be this binary: the WebSocket door refuses a browser whose `Origin` host is not the request's `Host` (CANT-22), so a client served from any other host cannot open a socket. `/` revalidates on every load and `/assets/*` is immutable, so a promote is visible on the next reload with no cache to purge. The provisioning port serves none of it.

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

## Media

**Uploaded media lives on local disk, on a volume the deployment mounts.** The decision is `docs/decisions/cant-47-media-storage.md`; this is what a deployment needs from it.

| variable | meaning |
|---|---|
| `CATENARY_MEDIA_DIR` | the directory media lives in, as `blobs/<hh>/<sha256>` with a `tmp/` beside it. **Optional**: unset, there is no `POST /uploads` and every attachment send is refused `upload_not_found`, so a deployment that predates the volume keeps booting |
| `CATENARY_MEDIA_URL_KEYS` | `1=<hex>,2=<hex>`: the secrets media URLs are signed with, at least 32 bytes (64 hex characters) each. The **last** entry signs and all verify, so a key is rotated by appending one. **Required with `CATENARY_MEDIA_DIR`** (boot refuses naming the variable). Generated in Signet, never in a file, a compose fragment or a ticket |
| `CATENARY_MAX_IMAGE_BYTES` | byte cap on an image upload. Default 16 MiB. Images are decoded and re-encoded on upload (JPEG, PNG, GIF first frame and WebP; HEIC and AVIF are `415`), so the process holds the decoded image in memory per concurrent upload: up to about **560 MiB** for one image at the 48-megapixel limit (a progressive JPEG with an orientation tag is the worst case), far more than the 16 MiB file cap suggests |
| `CATENARY_MAX_VOICE_BYTES` | byte cap on a voice upload. Default 8 MiB |

The directory must be **writable by `nobody`**, the image's user, and a directory that is not refuses to boot naming the variable rather than failing at the first upload. `tmp/` is emptied at every boot. **Media is not in Postgres, so it is not in a database backup**: the volume is a `construct-backup` source, taken **after** the database so a restored message never points at a blob the media snapshot lacks (CANT-47 section 6). The `construct-server` change is **SERV-244**; this file holds the decision, not a compose fragment.

## Link previews

**Off by default, and enabling it is a decision, not a toggle.** `POST /link-previews` (CANT-308) makes the server fetch the page behind a URL a member is composing and return a card for it. That is the one place this process opens a socket to an address somebody else chose, from inside the estate's network, so the route does not exist unless `CATENARY_LINK_PREVIEWS` is set: with it unset the router answers its own `404`, no fetcher is built and no outbound traffic is possible. A client treats that `404` as "previews unavailable".

| variable | meaning |
|---|---|
| `CATENARY_LINK_PREVIEWS` | `true` serves the route. Default off. **Requires `CATENARY_MEDIA_DIR`** (boot refuses naming the variable): a card's image is a stored blob and its handle is resolved by the uploads resolver |
| `CATENARY_LINKPREVIEW_CONCURRENCY` | fetches in flight server-wide. Default 4 |
| `CATENARY_LINKPREVIEW_RATE_PER_MINUTE`, `CATENARY_LINKPREVIEW_BURST` | per-account token bucket. Default 10 a minute, burst 5. Over either bound is `429 rate_limited` with `retry_after_sec` |
| `CATENARY_LINKPREVIEW_CACHE_TTL`, `CATENARY_LINKPREVIEW_NEGATIVE_CACHE_TTL` | in-process cache by canonical URL: a card 15m, a failure 5m. A hit opens no socket but still mints a fresh single-use handle |
| `CATENARY_LINKPREVIEW_CONNECT_TIMEOUT`, `_PAGE_TIMEOUT`, `_IMAGE_TIMEOUT`, `_TOTAL_TIMEOUT` | Go durations. Default 3s, 6s, 4s, 10s |
| `CATENARY_LINKPREVIEW_MAX_PAGE_BYTES`, `_MAX_IMAGE_BYTES`, `_MAX_REDIRECTS` | page bytes counted after decompression (2 MiB), image bytes (5 MiB), redirects (3) |

**The requirement, and the reason enabling waits.** The address check lives inside the process, at connect time: every socket the fetcher opens is refused unless its actual remote address is a global unicast one, so loopback, RFC 1918 (which is `construct_net` and every Docker bridge), link-local including the `169.254.169.254` metadata address, carrier-grade NAT and the other special ranges die at the dialer, and so do a redirect or a rebinding name that lands on one. That is one layer. The container also needs **outbound DNS and HTTPS to the public internet, and ideally nothing more than that**: an egress restriction at the network layer (no route from this container to anything but a resolver and the internet) is the second layer, and it is what makes a hole in the first one not matter. Whether the estate's container network can express that restriction is not settled, and **nothing should enable `CATENARY_LINK_PREVIEWS` in `construct-server` until it is.** Until then the variable stays unset, the route stays a `404`, and this repository carries the code switched off. The end-to-end proof (a fake origin and a rebinding resolver) is CANT-326 and has not run against a real container network; nothing in this repository has made a real outbound request.

**What an operator sees.** One structured log line per request: requester id, result class (`ok`, `blocked`, `timeout`, `too_large`, `not_html`, `empty`, `dns`, `unavailable`, `bad_url`, `limited`), duration, and whether the answer came from the cache. A `blocked` result also logs the address class at warn (a member pasting an internal address is worth noticing). **The URL's path and query are never logged**, because a pasted link can carry a token. To a client every fetch failure is the same empty `204`; the class exists only in this log.

**Two rules turn a fetched page into no card, and both log as `result=empty` with a `detail`.** Neither opens a socket or widens what the fetcher may connect to, and both run before the image fetch, so they cost no image request. Each is a `204` to the client, remembered as a failure for the negative-cache TTL, and writes nothing to uploads.

- **The wall rule** (`internal/linkpreview/wall.go`, CANT-336). A login wall or bot check answers `200` with a page whose title names only the site or the wall, and a card built from it would freeze that title and the site's logo into a message. The detail is `wall_login_redirect` (the fetch was redirected and landed on a login path such as `/login`, `/accounts/login` or `/authwall`), `wall_title` (every segment of the title is a brand token or a wall phrase, and at least one is a phrase: `Login • Instagram`, `Just a moment...`) or `wall_brand` (every segment is a brand token: `Instagram`). The rule is narrow on purpose: a title with any segment that says something the host does not is a card, and one stated false positive is accepted, a page whose whole title is its own site's name or a sign-in phrase. Brand tokens are derived from the pasted and final hosts and `og:site_name`, with no per-site list.
- **The final-host check** (`internal/linkpreview/finalhost.go`, CANT-337). The detail is `final_host`: the redirects left the pasted site and the host that served the page does not match the wire pattern for `final_host`, so a card that cannot name who served it is not shipped.

**Adding a wall entry, or fixing a false positive, is fixture first.** The phrase, brand and path lists live in `wall.go`, owned by this project, with no configuration override. An entry is added only with a page in `internal/linkpreview/testdata/walls/` and a row in `wallFixtures` in `internal/linkpreview/wall_test.go` naming the host, path, optional redirect and the expected `wall_*` detail. A reported false positive is answered the same way: add the page as a fixture whose row expects a card (an empty `want`), watch it fail, then change the rule. All fixtures there are hand-written and served from a loopback test server, so the suite makes no outbound request.

**A card may show a second host.** When the fetch's redirects left the pasted site, the card carries `final_host`, the host that actually served the page, and a client shows it after the host of the pasted URL (a shortener's card is titled and pictured by its target). When every hop stayed on the pasted site, or nothing redirected, the field is absent. Same site is decided once, at fetch time, and frozen with the card: two hosts are one site when they share a registrable domain (eTLD+1) from the public suffix list compiled into `golang.org/x/net/publicsuffix`. Where either host has none (an address literal, or a host that is itself a public suffix) they are compared whole after dropping a leading `www.`. No client carries a list or a rule.

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
