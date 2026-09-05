# Changelog

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
