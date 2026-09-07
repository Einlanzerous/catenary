# Changelog

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
