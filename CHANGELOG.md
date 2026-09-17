# Changelog

## [0.8.0](https://github.com/Einlanzerous/catenary/compare/v0.7.0...v0.8.0) (2026-09-17)


### Features

* CANT-30 — sever a revoked device's live socket, immediately ([#63](https://github.com/Einlanzerous/catenary/issues/63)) ([03f00e4](https://github.com/Einlanzerous/catenary/commit/03f00e49079648da28c8cdabed9e81736e99b5e3))
* CANT-97 — POST /refresh, the rotating exchange ahead of reuse detection ([#62](https://github.com/Einlanzerous/catenary/issues/62)) ([b99eaf3](https://github.com/Einlanzerous/catenary/commit/b99eaf38cb5b9e8abe5a8970499a12c4f981054f))


### Maintenance

* ignore a stray /service-account credential file ([04e1a14](https://github.com/Einlanzerous/catenary/commit/04e1a14ba422816799b86ffb3575a5a0d3e916cf))

## [0.7.0](https://github.com/Einlanzerous/catenary/compare/v0.6.0...v0.7.0) (2026-09-17)


### Features

* CANT-109 — soakrig against a deployed server: Access headers, provision, a stricter idle ([#53](https://github.com/Einlanzerous/catenary/issues/53)) ([c720a4d](https://github.com/Einlanzerous/catenary/commit/c720a4d416d44d18664488e7a0ca58e8c3caec1b))
* CANT-113 — the conversation and user frames on ServerFrame ([#58](https://github.com/Einlanzerous/catenary/issues/58)) ([ab4ac5f](https://github.com/Einlanzerous/catenary/commit/ab4ac5fe970313705f2b9a03db54882adefc6436))
* CANT-114 — the hub introduces a conversation and its users before the first message ([#59](https://github.com/Einlanzerous/catenary/issues/59)) ([6943b2b](https://github.com/Einlanzerous/catenary/commit/6943b2b6b95f54ff1ebdb4c0df85700c47c8d718))
* CANT-92 — receipt re-emission, the wire's live read-state refresh ([#55](https://github.com/Einlanzerous/catenary/issues/55)) ([8541567](https://github.com/Einlanzerous/catenary/commit/8541567989085c0bb8896daf71909963f2d6ca36))


### Bug Fixes

* CANT-111 — correct three stale wire-text clauses and regenerate ([#56](https://github.com/Einlanzerous/catenary/issues/56)) ([93bb636](https://github.com/Einlanzerous/catenary/commit/93bb636dfada813ce21183289bdf2a17a40232eb))
* CANT-115 — the hello deadline is per-server, not a package variable ([#60](https://github.com/Einlanzerous/catenary/issues/60)) ([dc328d5](https://github.com/Einlanzerous/catenary/commit/dc328d5e965f0558f06865e7525ed0c4e41e03ed))

## [0.6.0](https://github.com/Einlanzerous/catenary/compare/v0.5.0...v0.6.0) (2026-09-14)


### Features

* CANT-102 — the kill test over the socket, and a reusable Go client ([#51](https://github.com/Einlanzerous/catenary/issues/51)) ([937e430](https://github.com/Einlanzerous/catenary/commit/937e430bca3c68828101173bbbf57cdc745c3469))
* CANT-23 — heartbeat severance: server-driven dial and server-side enforcement ([#48](https://github.com/Einlanzerous/catenary/issues/48)) ([67440ef](https://github.com/Einlanzerous/catenary/commit/67440efd53bd56320ef87eeda00d626adb17461c))
* CANT-27 — the soak and chaos harness ([#52](https://github.com/Einlanzerous/catenary/issues/52)) ([7ebd246](https://github.com/Einlanzerous/catenary/commit/7ebd246183fc3ff4b2edc4509ba47a1af1035c11))
* CANT-75 — REST send and find-or-create DM, the write path for bots and agents ([#49](https://github.com/Einlanzerous/catenary/issues/49)) ([4bea19e](https://github.com/Einlanzerous/catenary/commit/4bea19e0fe440ce2723073d4ba88cb03b59d55ca))

## [0.5.0](https://github.com/Einlanzerous/catenary/compare/v0.4.0...v0.5.0) (2026-09-14)


### Features

* CANT-101 — the hello outcome, the schema descriptions, and the CANT-24 decision record ([#37](https://github.com/Einlanzerous/catenary/issues/37)) ([011f315](https://github.com/Einlanzerous/catenary/commit/011f31501e3c2f04f7a5562a7bdbe5aa27d91a57))
* CANT-107 — the hub: per-viewer fan-out, gap → resync_required, read/typing, and the drain ([#46](https://github.com/Einlanzerous/catenary/issues/46)) ([d3eb771](https://github.com/Einlanzerous/catenary/commit/d3eb771879944827282151c9a1f30b24a299b08b))
* CANT-108 — the logomark: the 1C "Span" tile in the rail brand, the favicon, and the accent-mark token ([#47](https://github.com/Einlanzerous/catenary/issues/47)) ([ad3e7fa](https://github.com/Einlanzerous/catenary/commit/ad3e7fa3c82a1824e0329e2d21857b828eec132a))
* CANT-22 — the door: WebSocket upgrade, auth handshake, and the codec on the generated types ([#42](https://github.com/Einlanzerous/catenary/issues/42)) ([6cbe11d](https://github.com/Einlanzerous/catenary/commit/6cbe11dabb10a6ba8b956bd13c8dedc54f7d3db1))
* CANT-85 — attachments linked inside the insert's own transaction ([#45](https://github.com/Einlanzerous/catenary/issues/45)) ([c57878d](https://github.com/Einlanzerous/catenary/commit/c57878d3f6bc0a5e01c980b32b9ec5049e06a35a))


### Bug Fixes

* CANT-105 — allow-list toOpenAPISchema's pass-through keywords ([#43](https://github.com/Einlanzerous/catenary/issues/43)) ([244f734](https://github.com/Einlanzerous/catenary/commit/244f734b7fd579b9f92e2aecc561d4190ad5ccc1))

## [0.4.0](https://github.com/Einlanzerous/catenary/compare/v0.3.0...v0.4.0) (2026-09-12)


### Features

* CANT-28 — the token model: four credential shapes, one authenticate seam ([#33](https://github.com/Einlanzerous/catenary/issues/33)) ([5339457](https://github.com/Einlanzerous/catenary/commit/533945713310cd3e5b0af0f58ff64949a18f2d5d))


### Bug Fixes

* CANT-94 — resolve dependencies in verify.sh so CI and a local run make one claim ([#31](https://github.com/Einlanzerous/catenary/issues/31)) ([964ad33](https://github.com/Einlanzerous/catenary/commit/964ad33c415c5a149ad4c73dab8a05dc151fbb87))
* CANT-98 — rename enrol to US enroll on the wire, in migration 0007 and on the route ([#35](https://github.com/Einlanzerous/catenary/issues/35)) ([3e50f16](https://github.com/Einlanzerous/catenary/commit/3e50f1650c41abd233300c3292dd35893e02af94))

## [0.3.0](https://github.com/Einlanzerous/catenary/compare/v0.2.0...v0.3.0) (2026-09-08)

> **Corrected by hand on 2026-09-10 (CANT-93).** The eight entries from #25–#28 below were absent when this release was cut. Those four PRs were squash-merged with a `CANT-NN:` subject, which is not a conventional-commit type, so release-please never saw them — only #23, which landed as a merge commit, reached the changelog. The work shipped in this release either way; only the record of it was missing.

### Features

* GET /sync — paging, an honest has_more, and a cursor that can advance past what you cannot see ([5c0488c](https://github.com/Einlanzerous/catenary/commit/5c0488c9157b1edeab68a302f8f8696e4c07ebbf))
* read state — the receipt write, and one derivation everything unread reads ([ac347f9](https://github.com/Einlanzerous/catenary/commit/ac347f9c6b143519285593e684f34b36ac71d77e))
* LISTEN/NOTIFY fanout — ids only, and a cap that is a test ([96e5998](https://github.com/Einlanzerous/catenary/commit/96e5998584f9a657872df39aca248642a6c65ce3))
* your own message says what OTHERS have read, and read_by gets a screen ([09fa7b6](https://github.com/Einlanzerous/catenary/commit/09fa7b6ac4fc36870fba8bd512e79b72615388bc))
* the Go decoders validate, and no vector is skipped in any runner ([52248b1](https://github.com/Einlanzerous/catenary/commit/52248b1e17c29b6e4796453563789b0f3bfccf2c))


### Bug Fixes

* your own message is `sent`, and the 500 body is a frame a client can read ([5c0488c](https://github.com/Einlanzerous/catenary/commit/5c0488c9157b1edeab68a302f8f8696e4c07ebbf))
* read_by and member_count now count the same population, so READ n/n closes ([ac347f9](https://github.com/Einlanzerous/catenary/commit/ac347f9c6b143519285593e684f34b36ac71d77e))
* raise the gap after LISTEN, and stop claiming a drop that cannot be asserted ([96e5998](https://github.com/Einlanzerous/catenary/commit/96e5998584f9a657872df39aca248642a6c65ce3))
* the live ladder walked to DELIVERED too, and two claims I got backwards ([09fa7b6](https://github.com/Einlanzerous/catenary/commit/09fa7b6ac4fc36870fba8bd512e79b72615388bc))
* the five nits, and a claim I got backwards ([2c47ddf](https://github.com/Einlanzerous/catenary/commit/2c47ddf1cf305396002b41c7acc678757794f519))

## [0.2.0](https://github.com/Einlanzerous/catenary/compare/v0.1.0...v0.2.0) (2026-09-07)


### Features

* derive first_unread_seq, and drop the read_seq advance with its lock ([76cd466](https://github.com/Einlanzerous/catenary/commit/76cd46684761fe52f6ce9661537771d46ddd5818))
* one wire mapping — the Message both transports return ([64e68c6](https://github.com/Einlanzerous/catenary/commit/64e68c676b807440874f607da6939c9b851797d3))
* read_seq, reply_to, the third lock, and what a refusal logs ([811d13e](https://github.com/Einlanzerous/catenary/commit/811d13e54eda02e3e16f3a58b86fb3732858e214))
* the refusals a send can make without touching the transaction ([ccd9c87](https://github.com/Einlanzerous/catenary/commit/ccd9c87c12c7c238e29f6fa84b3baa28f3529a0f))
* the SendError taxonomy and the guard that keeps one file deciding codes ([d2ca70b](https://github.com/Einlanzerous/catenary/commit/d2ca70b86f3372ddb5728d86a4f1e8c5f387bf27))


### Bug Fixes

* a closed pool is transient, and the const scan survives implicit typing ([86de356](https://github.com/Einlanzerous/catenary/commit/86de356192c617e232e98d23c2f3baa76dce31e6))
* check the mktemp, widen the guard, and stop six steps sharing one log ([799a36c](https://github.com/Einlanzerous/catenary/commit/799a36c5c039b278c4ba2cbccb3333137c054c63))
* correct first_unread_seq's mapping note, which this branch falsified ([69a591b](https://github.com/Einlanzerous/catenary/commit/69a591be09a6ca628df55d4ef04825bf6294b366))
* give each verify.sh run its own log directory ([33aeb36](https://github.com/Einlanzerous/catenary/commit/33aeb3620b49cec1e2969e9949541886c6af06cf))
* hold the reply_to source, and say the lock order is four ([e516aac](https://github.com/Einlanzerous/catenary/commit/e516aac57837f2bb61d29133d55530c1e55a93b9))
* make the log-directory allocator stop the run instead of returning empty ([dc6ee2d](https://github.com/Einlanzerous/catenary/commit/dc6ee2d156040eebea4db92456c379adc939bd17))
* scope the reply_to lock to this conversation ([85bfe1c](https://github.com/Einlanzerous/catenary/commit/85bfe1cd5884103a3d33cd92ce67d58e0dad2cac))
* seven shapes, not four — and make the kind assertion assert something ([eac6530](https://github.com/Einlanzerous/catenary/commit/eac6530e47beccdb4042961bd29a2a27986426f4))
* state what is actually true about the /tmp literals, and catch backticks ([48c583d](https://github.com/Einlanzerous/catenary/commit/48c583d78d129135d218d769bfb2d7b91ca65120))
* state why GREATEST is right, and keep the answer position 9 already has ([ec201ea](https://github.com/Einlanzerous/catenary/commit/ec201ea923d0cca671be7faaef7892b3077323ae))
* the guard saw one of four constructions, and column was never checked ([8c30c36](https://github.com/Einlanzerous/catenary/commit/8c30c3660e98fc2e54fab13dcfc6a80023fc0678))
* the nine items from the stage 1+2 review ([adc1a66](https://github.com/Einlanzerous/catenary/commit/adc1a66bca54f73e7508a88b743ff6e3cef96415))
* widen the transient classes, catch code literals, and carry the wire type ([bb50777](https://github.com/Einlanzerous/catenary/commit/bb507774c3fae84c592ad36058b8a76495fb4981))


### Code Refactoring

* move the generated wire package where the service can import it ([3c2d928](https://github.com/Einlanzerous/catenary/commit/3c2d92827eaeb42b245958bc6dbff9ab557def94))

## 0.1.0 (2026-09-05)


### Features

* a Dockerfile and the release pipeline that publishes the image ([64b19be](https://github.com/Einlanzerous/catenary/commit/64b19beb6105b852e893d952340a73b2841e9cc6))
* both ordinals, assigned inside the insert's own transaction ([30d1880](https://github.com/Einlanzerous/catenary/commit/30d1880a4068a6d2ba62fc11a9d4ce1ea8d43057))
* config, structured logging, /healthz and /readyz ([bed2cbe](https://github.com/Einlanzerous/catenary/commit/bed2cbec0c6e9fc0b18232d39003ca2965b3522d))
* graduate IDEA-23 — Catenary becomes the CANT project ([2f9ec63](https://github.com/Einlanzerous/catenary/commit/2f9ec63c1ad4a08a4c3ae13f93f1838ecbd04a67))
* initial schema and the in-process migration harness ([fb7c7f3](https://github.com/Einlanzerous/catenary/commit/fb7c7f344e5d1afb6c553038d6c3fd8d3a725b56))
* the deploy fragments — compose, and the Traefik split entrypoint ([34a8b82](https://github.com/Einlanzerous/catenary/commit/34a8b8244b3d67f221fd81a477ec7b753e35939b))
* the wire schema emits openapi.yaml ([20d5de8](https://github.com/Einlanzerous/catenary/commit/20d5de8710b1a836136273d716329165f2fe3415))


### Bug Fixes

* address the five nits from the PR review ([18bcf39](https://github.com/Einlanzerous/catenary/commit/18bcf39766722232a60871d8bb62db7e49b84100))
* four findings from the first review of CANT-17 ([944beb7](https://github.com/Einlanzerous/catenary/commit/944beb72ae30768705a1148c54708031c774eef0))
* the generator emits gofmt-clean Go ([ffb8e2a](https://github.com/Einlanzerous/catenary/commit/ffb8e2a364a48cc264c2af8138b5c110355deea1))
* the idempotency key is required, and a missing one is loud ([5d87a10](https://github.com/Einlanzerous/catenary/commit/5d87a10dd84bc0550faff492b854efbea4650bcd))
* the reversed client_id claim survived in two more places ([2276f8d](https://github.com/Einlanzerous/catenary/commit/2276f8de3900ccad2ecb06c2ac5d686c5a29a425))
* the staleness proof could corrupt a generated file and still pass ([41dc579](https://github.com/Einlanzerous/catenary/commit/41dc579a552cc877ea2e0cda3c08acaf203eb6c1))
* the wire schema still named the write target CANT-13 reversed ([f5cf3a7](https://github.com/Einlanzerous/catenary/commit/f5cf3a7c6d5fb9a5844ddd9206d29d2054a93242))
* three important findings and four nits from the CANT-13 review ([73c65b0](https://github.com/Einlanzerous/catenary/commit/73c65b0ba229f595d604c2858d50b5a1094eb7ef))
