# Changelog

## [0.17.0](https://github.com/Einlanzerous/catenary/compare/v0.16.0...v0.17.0) (2026-10-07)


### Features

* CANT-162 — attachments in the web outbox: the Uploader seam, the refusing default, holder-only uploads, and the one-shot stale-handle re-upload ([#172](https://github.com/Einlanzerous/catenary/issues/172)) ([56d29fc](https://github.com/Einlanzerous/catenary/commit/56d29fc5363a0ca657c8656d9f302bc42edca557))
* CANT-229 — a pending transcript draws the canvas's pulse, estimate and skeleton lines ([#160](https://github.com/Einlanzerous/catenary/issues/160)) ([deac360](https://github.com/Einlanzerous/catenary/commit/deac3607307b2b998c54a6a0f07e1348a8525c2d))
* CANT-241 — serve the web client from the binary ([#170](https://github.com/Einlanzerous/catenary/issues/170)) ([08a16a9](https://github.com/Einlanzerous/catenary/commit/08a16a9311dcc92673f3126f2c7dd6b56b06aceb))
* CANT-244 — the Dart journal records whose it is, and a claim wipes another account's (CANT-230a) ([#167](https://github.com/Einlanzerous/catenary/issues/167)) ([f019b37](https://github.com/Einlanzerous/catenary/commit/f019b37cfdd42fe33f7feab7751b612be84863b3))


### Bug Fixes

* CANT-221 — the web thread header derives TLS or CLEARTEXT from the origin the session talks to ([#162](https://github.com/Einlanzerous/catenary/issues/162)) ([be323a1](https://github.com/Einlanzerous/catenary/commit/be323a15125751ce0a3ded9aa5e4cfc48e40122c))
* CANT-227 — a failed transcript says NO TRANSCRIPT, and a state this build does not know draws no strip ([#164](https://github.com/Einlanzerous/catenary/issues/164)) ([30e4458](https://github.com/Einlanzerous/catenary/commit/30e44587fe35e84dfd9423d872a842c1f726e08d))
* CANT-228 — a credential the web could not store is not "could not reach the server" ([#163](https://github.com/Einlanzerous/catenary/issues/163)) ([c05edd6](https://github.com/Einlanzerous/catenary/commit/c05edd6a31e2622ca046ba2f7404e8fca77f6cdf))
* CANT-235 — the rail previews an unsent voice note as "voice note", never "transcript pending" ([#175](https://github.com/Einlanzerous/catenary/issues/175)) ([8162d64](https://github.com/Einlanzerous/catenary/commit/8162d641e2248f8cbac39472d9391aba703c2068))
* CANT-237 — a start that fails is a screen, and never the enrollment form (CANT-222a) ([#165](https://github.com/Einlanzerous/catenary/issues/165)) ([9477c5e](https://github.com/Einlanzerous/catenary/commit/9477c5e2b09f381eacea7a7404415f8e701f6142))
* CANT-238 — enrollment always ends in a text, and a failed journal wipe is owed (CANT-222b) ([#166](https://github.com/Einlanzerous/catenary/issues/166)) ([9aff271](https://github.com/Einlanzerous/catenary/commit/9aff271a53d1a80cfd9f37eecab170f735b1d5ed))
* CANT-240 — a wipe or start that rejects after enrollment is caught and told on the account view ([#171](https://github.com/Einlanzerous/catenary/issues/171)) ([32259bc](https://github.com/Einlanzerous/catenary/commit/32259bcd246ed3560d59c4b7315bad6d16091585))
* CANT-245 — the app never builds a transport over another account's journal (CANT-230b) ([#173](https://github.com/Einlanzerous/catenary/issues/173)) ([8ce80c6](https://github.com/Einlanzerous/catenary/commit/8ce80c659fd9550e430a1f7add6f764f6fc15ee8))
* CANT-246 — the web never builds a transport over another account's journal (CANT-230c) ([#169](https://github.com/Einlanzerous/catenary/issues/169)) ([3e4495e](https://github.com/Einlanzerous/catenary/commit/3e4495e0778819386b1ec5066d0e21f11476be8c))
* CANT-248 — a direct conversation's web header reads DIRECT · &lt;transport&gt;, not a member count (CANT-233a) ([#174](https://github.com/Einlanzerous/catenary/issues/174)) ([68da7ce](https://github.com/Einlanzerous/catenary/commit/68da7ceaeb4f593ffb4da572e5da68101f9778b0))

## [0.16.0](https://github.com/Einlanzerous/catenary/compare/v0.15.0...v0.16.0) (2026-10-06)


### Features

* CANT-223 — the outbox asks the transport whether it is terminal, and the upload rules are pinned (CANT-220a) ([#155](https://github.com/Einlanzerous/catenary/issues/155)) ([3dc0dfe](https://github.com/Einlanzerous/catenary/commit/3dc0dfed348d9d7538032e8d6b7272e7924d4689))
* CANT-225 — a failed transcript says so, an unknown one claims nothing, RETRYING reaches the thread, and a credential that could not be stored is not "unreachable" (CANT-220c) ([#157](https://github.com/Einlanzerous/catenary/issues/157)) ([4320ed1](https://github.com/Einlanzerous/catenary/commit/4320ed1fe1493c534d81cb68880a69fa24c62759))


### Bug Fixes

* CANT-226 — QUEUE and the recording row's SEND take the canvas's letter spacing (CANT-220d) ([#159](https://github.com/Einlanzerous/catenary/issues/159)) ([257d37a](https://github.com/Einlanzerous/catenary/commit/257d37abd9f98c53a60b242faba4142facb460e5))

## [0.15.0](https://github.com/Einlanzerous/catenary/compare/v0.14.0...v0.15.0) (2026-10-05)


### Features

* CANT-205 — enrollDevice and the journal projection in dart-client (CANT-200a) ([#142](https://github.com/Einlanzerous/catenary/issues/142)) ([78214b7](https://github.com/Einlanzerous/catenary/commit/78214b7e60cddde805573fd262ac4f37e78d705e))
* CANT-206 — the app takes catenary_client: platform seams, the address functions, the session (CANT-200b) ([#146](https://github.com/Einlanzerous/catenary/issues/146)) ([c26a70c](https://github.com/Einlanzerous/catenary/commit/c26a70c889b3e3da8d00843635e2fa20811ae6e7))
* CANT-207 — the store: journal and outbox projected into the rail, the thread and the banner (CANT-200c) ([#151](https://github.com/Einlanzerous/catenary/issues/151)) ([6efef75](https://github.com/Einlanzerous/catenary/commit/6efef7503990752aca422cf532b04558989eef14))
* CANT-208 — the enrollment screen and RE-ENROLL (CANT-200d) ([#153](https://github.com/Einlanzerous/catenary/issues/153)) ([bea09c3](https://github.com/Einlanzerous/catenary/commit/bea09c36601f87e29f6869367d3c633efc69ad65))
* CANT-209 — the controls: send, retry, delete, reconnect, and the ones drawn disabled (CANT-200e) ([#152](https://github.com/Einlanzerous/catenary/issues/152)) ([6ca328a](https://github.com/Einlanzerous/catenary/commit/6ca328a79dd6d3737a2bd07931a5bae3971b014b))
* CANT-210 — a server a phone can reach, and cleartext in the debug and profile manifests (CANT-200f) ([#154](https://github.com/Einlanzerous/catenary/issues/154)) ([572f697](https://github.com/Einlanzerous/catenary/commit/572f6972652d02c4652c679702b6de34c33d8072))
* CANT-211 — media held with an outbox entry, and the offline rule (CANT-201a) ([#148](https://github.com/Einlanzerous/catenary/issues/148)) ([db40067](https://github.com/Einlanzerous/catenary/commit/db4006721f6c44ec27ecc481c4caedd657c3d678))


### Bug Fixes

* CANT-204 — a page in flight across another context's wipe is dropped (CANT-199a) ([#143](https://github.com/Einlanzerous/catenary/issues/143)) ([fd424c6](https://github.com/Einlanzerous/catenary/commit/fd424c6fef1792e450ac7731ad16805ca8f4ed54))

## [0.14.0](https://github.com/Einlanzerous/catenary/compare/v0.13.0...v0.14.0) (2026-10-04)


### Features

* CANT-185 — read on the driver protocol, Client.Read, and the KeepHeldConversation fault (CANT-46a) ([#126](https://github.com/Einlanzerous/catenary/issues/126)) ([50ee4cc](https://github.com/Einlanzerous/catenary/commit/50ee4cc413e3694eef834d0fb7f910314a3d0efd))
* CANT-186 — client.SameState, the partition proxy and the convergence rig (CANT-46b) ([#129](https://github.com/Einlanzerous/catenary/issues/129)) ([e9eb8a3](https://github.com/Einlanzerous/catenary/commit/e9eb8a3804c1ff48b32ace5f3d6afd35b0cfc646))
* CANT-187 — the outbox through the driver: compose, outbox, and schedule S5 (CANT-46c) ([#130](https://github.com/Einlanzerous/catenary/issues/130)) ([5c9bdfa](https://github.com/Einlanzerous/catenary/commit/5c9bdfa86ef89801e85942dd28bb0e1fe95a3e38))
* CANT-188 — the Dart convergence lane: TypeScript against the headless Dart driver (CANT-46d) ([#138](https://github.com/Einlanzerous/catenary/issues/138)) ([0a6ebe3](https://github.com/Einlanzerous/catenary/commit/0a6ebe35f1375d609a597f91eef764976c11051c))
* CANT-191 — the catenary_client package, its SQLite binding and migrator, and the journal (CANT-42a) ([#127](https://github.com/Einlanzerous/catenary/issues/127)) ([083b895](https://github.com/Einlanzerous/catenary/commit/083b89519817c7dbaf671b2128454ab0fac467a2))
* CANT-192 — the Dart transport core and the decision-vector runner (CANT-42b) ([#132](https://github.com/Einlanzerous/catenary/issues/132)) ([7eac3eb](https://github.com/Einlanzerous/catenary/commit/7eac3eb27508523791942322d6cdcbf003a465f0))
* CANT-193 — the Dart credential layer, the SQLite-held lock, and the full decision-vector runner (CANT-42c) ([#134](https://github.com/Einlanzerous/catenary/issues/134)) ([2ab75e0](https://github.com/Einlanzerous/catenary/commit/2ab75e000620e7e4d1e41579f5a9649346ef2d5b))
* CANT-194 — the Dart driver and the Dart cohort: soak, kill-test and restore-test lanes (CANT-42d) ([#133](https://github.com/Einlanzerous/catenary/issues/133)) ([fe9913c](https://github.com/Einlanzerous/catenary/commit/fe9913c1fcf3f8f9a572ee7b3c68173ccb08c765))
* CANT-196 — the Dart outbox: store, state machine, drain under the lock, and its criteria (CANT-42e) ([#136](https://github.com/Einlanzerous/catenary/issues/136)) ([7007201](https://github.com/Einlanzerous/catenary/commit/7007201b454a8e31ae3c504a464eab8127ba6f81))
* CANT-197 — compose and outbox on the Dart driver (CANT-42f) ([#137](https://github.com/Einlanzerous/catenary/issues/137)) ([27cf24c](https://github.com/Einlanzerous/catenary/commit/27cf24cd639bf6f675f1fa66ac5a31c49c76920d))
* CANT-40 — the Flutter app skeleton and the shared tokens, lifted from tokens.css ([#131](https://github.com/Einlanzerous/catenary/issues/131)) ([17774aa](https://github.com/Einlanzerous/catenary/commit/17774aa2ce42c2fccf5d76b175ad723722d80c1a))
* CANT-43 — the rail and the thread against Catenary Mobile.dc.html ([#139](https://github.com/Einlanzerous/catenary/issues/139)) ([ec5de54](https://github.com/Einlanzerous/catenary/commit/ec5de544c58f3d66456af56ef488c96c915dc076))
* CANT-44 — the composer, the connection states and the typing rule in the Flutter client ([#135](https://github.com/Einlanzerous/catenary/issues/135)) ([c836677](https://github.com/Einlanzerous/catenary/commit/c836677840f6da0d77bb032f91f2f8611547de58))

## [0.13.0](https://github.com/Einlanzerous/catenary/compare/v0.12.0...v0.13.0) (2026-10-01)


### Features

* CANT-180 — generated client-side decode for Go (CANT-177a) ([#124](https://github.com/Einlanzerous/catenary/issues/124)) ([f31abea](https://github.com/Einlanzerous/catenary/commit/f31abea972c0fff62e34a6e2b7323126ee57af3f))
* CANT-181 — internal/client decodes as a client (CANT-177b) ([#125](https://github.com/Einlanzerous/catenary/issues/125)) ([d70cc87](https://github.com/Einlanzerous/catenary/commit/d70cc8742ad14772669670e8c88692734edc9d36))


### Bug Fixes

* CANT-184 — keep test files out of the image build context ([#121](https://github.com/Einlanzerous/catenary/issues/121)) ([3c95c96](https://github.com/Einlanzerous/catenary/commit/3c95c960d85895ce06b96043dad73625c41cd022))

## [0.12.0](https://github.com/Einlanzerous/catenary/compare/v0.11.1...v0.12.0) (2026-09-30)


### Features

* CANT-141a — Conversation.other_member_id, wire and store ([#94](https://github.com/Einlanzerous/catenary/issues/94)) ([db9ab64](https://github.com/Einlanzerous/catenary/commit/db9ab64baf202a06d0cce573ce899fe4339bfb93))
* CANT-141b — web reads other_member_id, retires the guess ([#96](https://github.com/Einlanzerous/catenary/issues/96)) ([5c85efd](https://github.com/Einlanzerous/catenary/commit/5c85efd3b9e163f3e72d6c512a4bf2c15543cc6e))
* CANT-151 — the TypeScript transport core (CANT-35a) ([#102](https://github.com/Einlanzerous/catenary/issues/102)) ([0c9159d](https://github.com/Einlanzerous/catenary/commit/0c9159d91a074bdffb05f771fdabc16be2bfdcdf))
* CANT-152 — the TypeScript credential layer (CANT-35b) ([#105](https://github.com/Einlanzerous/catenary/issues/105)) ([08875e3](https://github.com/Einlanzerous/catenary/commit/08875e390e38d08654e8b7b37315f4c6e1b4518e))
* CANT-153 — soakrig TypeScript cohort, TS kill-test and restore-test lanes (CANT-35c) ([#106](https://github.com/Einlanzerous/catenary/issues/106)) ([a467896](https://github.com/Einlanzerous/catenary/commit/a4678962eec42996dac4929317d17cbc0212aff7))
* CANT-156 — shared decision vectors for CANT-31's client rules ([#115](https://github.com/Einlanzerous/catenary/issues/115)) ([ec7242b](https://github.com/Einlanzerous/catenary/commit/ec7242b449d5401bbb703c855f2af053e1d5a456))
* CANT-161 — the outbox core: catenary-outbox store, state machine, rail merge, NullTransport ([#103](https://github.com/Einlanzerous/catenary/issues/103)) ([7a57cf8](https://github.com/Einlanzerous/catenary/commit/7a57cf824fe4cd81988b49b05594aa4e3cb4e384))
* CANT-163 — the outbox's OutboxTransport adapter over CANT-35's transport ([#107](https://github.com/Einlanzerous/catenary/issues/107)) ([63ac9a2](https://github.com/Einlanzerous/catenary/commit/63ac9a23c253925e5b68cac794fc7bced9155254))
* CANT-165 — soakrig restoreprobe, drives a client and one send against a served, restored instance ([#101](https://github.com/Einlanzerous/catenary/issues/101)) ([c9e3dda](https://github.com/Einlanzerous/catenary/commit/c9e3dda3ed808c5774da7a4594dd9ff9c07ac7ac))
* CANT-169 — durable IndexedDB journal for the web transport, and the TS clientDies kill-test lane ([#110](https://github.com/Einlanzerous/catenary/issues/110)) ([3f7bf22](https://github.com/Einlanzerous/catenary/commit/3f7bf2262d310a35b8d48f292b5f4444a81db827))
* CANT-170 — CANT-35 ruling 3B's backoff in the Go reference client ([#116](https://github.com/Einlanzerous/catenary/issues/116)) ([b59081b](https://github.com/Einlanzerous/catenary/commit/b59081bda4030ef3ddd33d5fb39820fe7ef243fb))
* CANT-171 — CANT-103 rules 1–4 in the reference client ([#117](https://github.com/Einlanzerous/catenary/issues/117)) ([00b9c9e](https://github.com/Einlanzerous/catenary/commit/00b9c9ef353ca45dcd7b433ec97ca653c45ce87e))
* CANT-175 — a live journal write refused as stale pulls a catch-up ([#113](https://github.com/Einlanzerous/catenary/issues/113)) ([12cc297](https://github.com/Einlanzerous/catenary/commit/12cc29701122032204da5f8164ff23084e001295))
* CANT-34 — migrate components off types.ts onto the generated types ([#97](https://github.com/Einlanzerous/catenary/issues/97)) ([9d3764e](https://github.com/Einlanzerous/catenary/commit/9d3764e88b5b69ee22dda2509ba8c024c1ff6e4d))
* CANT-37 — resync UI with numeric progress toward head_seq ([#104](https://github.com/Einlanzerous/catenary/issues/104)) ([765a1db](https://github.com/Einlanzerous/catenary/commit/765a1db30973700511193d31cf8c27adf774640c))
* CANT-38 — auth UI: login, device naming, session list, revoke ([#109](https://github.com/Einlanzerous/catenary/issues/109)) ([405c15b](https://github.com/Einlanzerous/catenary/commit/405c15b9498fb91a72589b6b20d951bb431e5418))
* CANT-39 — delete the mock store; the smoke renders against a live server ([#114](https://github.com/Einlanzerous/catenary/issues/114)) ([80d2546](https://github.com/Einlanzerous/catenary/commit/80d2546dfd8e27b2af45c39e078416d2c88ecab0))


### Bug Fixes

* CANT-172 — the soak compares only once every client holds what the server has ([#111](https://github.com/Einlanzerous/catenary/issues/111)) ([8f35cb6](https://github.com/Einlanzerous/catenary/commit/8f35cb6df096599f0acee0f7a2a543c2761681db))
* CANT-174 — soakrig's first start moves off a stolen port, the restart never does ([#118](https://github.com/Einlanzerous/catenary/issues/118)) ([94379d8](https://github.com/Einlanzerous/catenary/commit/94379d837e708fdcfa098f4bcd4226882f51234c))
* CANT-179 — three internal/client tests deterministic under -race load ([#120](https://github.com/Einlanzerous/catenary/issues/120)) ([c3a2ce0](https://github.com/Einlanzerous/catenary/commit/c3a2ce0e697539b48d64f1e1ef25a500ba4fbde3))
* readstate.ts's stale @/types import, missed by CANT-34 ([#98](https://github.com/Einlanzerous/catenary/issues/98)) ([99f4f8d](https://github.com/Einlanzerous/catenary/commit/99f4f8de5ed2b9dbd88d46dbe732d9151811539e))

## [0.11.1](https://github.com/Einlanzerous/catenary/compare/v0.11.0...v0.11.1) (2026-09-25)


### Bug Fixes

* CANT-146 — an offboard re-emits one budget to an author, not one per room ([#92](https://github.com/Einlanzerous/catenary/issues/92)) ([37b2d3b](https://github.com/Einlanzerous/catenary/commit/37b2d3b2dcebf03185a96f6d838b3a70aa58afd2))

## [0.11.0](https://github.com/Einlanzerous/catenary/compare/v0.10.0...v0.11.0) (2026-09-24)


### Features

* CANT-137 — member counts count active members, and an offboard reaches the room ([#84](https://github.com/Einlanzerous/catenary/issues/84)) ([435df0f](https://github.com/Einlanzerous/catenary/commit/435df0f1697a991a170964b1408ffa1f4ebcf472))
* CANT-138 — web shows a deactivated person, quietly ([#86](https://github.com/Einlanzerous/catenary/issues/86)) ([798f5a4](https://github.com/Einlanzerous/catenary/commit/798f5a47a85394571cf45189185b2c82db284e3b))
* CANT-144 — the wire states the clamp, names what re-emits, and narrows the compatibility bullet ([#89](https://github.com/Einlanzerous/catenary/issues/89)) ([f36243c](https://github.com/Einlanzerous/catenary/commit/f36243cd4e895e3a7e12f0187a1e90e90507f869))
* CANT-145 — web clamps the read fraction to the room, and the two-member guard is pinned ([#90](https://github.com/Einlanzerous/catenary/issues/90)) ([534edb0](https://github.com/Einlanzerous/catenary/commit/534edb0fa307b45b63b5909793a05815a88f4981))


### Bug Fixes

* CANT-142 — pace the slow-consumer burst against real drain, not the scheduler ([#87](https://github.com/Einlanzerous/catenary/issues/87)) ([ef5a640](https://github.com/Einlanzerous/catenary/commit/ef5a64079f72ad06cca71aeb76215944f6a114ee))
* CANT-143 — an offboard and its reversal re-emit the messages the person had read ([#88](https://github.com/Einlanzerous/catenary/issues/88)) ([337ef26](https://github.com/Einlanzerous/catenary/commit/337ef266da3c9f2e43411a1901b0de400cbcb63a))

## [0.10.0](https://github.com/Einlanzerous/catenary/compare/v0.9.0...v0.10.0) (2026-09-21)


### Features

* CANT-118 — spent credentials are swept once their window has passed ([#77](https://github.com/Einlanzerous/catenary/issues/77)) ([1ec8eb2](https://github.com/Einlanzerous/catenary/commit/1ec8eb2faee83e03ccac377734b43184251e6a84))
* CANT-121 — the client's credential is durable state, not configuration ([#72](https://github.com/Einlanzerous/catenary/issues/72)) ([afdf201](https://github.com/Einlanzerous/catenary/commit/afdf20110acbad43b6fc1b4b6a6125d5b5c162e1))
* CANT-122 — the hello timeout closes 4002, so a bare 1008 means a client bug ([#70](https://github.com/Einlanzerous/catenary/issues/70)) ([b0d95d3](https://github.com/Einlanzerous/catenary/commit/b0d95d33753b909e371ff287def32bfed07a069c))
* CANT-123 — the reference client stops when it must, and only then ([#74](https://github.com/Einlanzerous/catenary/issues/74)) ([355c8db](https://github.com/Einlanzerous/catenary/commit/355c8dbe2d6a4d9cee2a3e97fa55e88bb00295c9))
* CANT-124 — the client refreshes before it needs to, and when it is refused ([#73](https://github.com/Einlanzerous/catenary/issues/73)) ([c8e067b](https://github.com/Einlanzerous/catenary/commit/c8e067b7e4baa764c0e01458b4bc6a9dd75a0f9c))
* CANT-125 — RotateRefresh accepts a proposed successor, and routes its collision ([#75](https://github.com/Einlanzerous/catenary/issues/75)) ([10bddac](https://github.com/Einlanzerous/catenary/commit/10bddac590414c6d5dee55e546701f536dc54a85))
* CANT-126 — a lost refresh response no longer costs the device ([#76](https://github.com/Einlanzerous/catenary/issues/76)) ([9a32829](https://github.com/Einlanzerous/catenary/commit/9a328292ee1039fb7a6070815e7aa7152d8cc10f))
* CANT-127 — an unsettled credential waits for Catenary before it refreshes again ([#78](https://github.com/Einlanzerous/catenary/issues/78)) ([6383fb3](https://github.com/Einlanzerous/catenary/commit/6383fb336b3d3740809e561441df3201d18afabb))
* CANT-129 — a refused access token is presented once per refresh hold ([#80](https://github.com/Einlanzerous/catenary/issues/80)) ([ccbf849](https://github.com/Einlanzerous/catenary/commit/ccbf849688748c91e879d56f361da72e39e87644))
* CANT-131 — the provisioning surface: its own listener, one credential, three operations ([#82](https://github.com/Einlanzerous/catenary/issues/82)) ([e7e9c34](https://github.com/Einlanzerous/catenary/commit/e7e9c3475b6a704c5af4f96eaa993c135ff4b953))

## [0.9.0](https://github.com/Einlanzerous/catenary/compare/v0.8.0...v0.9.0) (2026-09-18)


### Features

* CANT-117 — the device list and the scoped revoke, as a generated wire type ([#65](https://github.com/Einlanzerous/catenary/issues/65)) ([7e58c56](https://github.com/Einlanzerous/catenary/commit/7e58c56d3c8f6ad590e01c9fa3f626652baa2b12))
* CANT-73 — service accounts and bot tokens, minted and ended from the CLI ([#66](https://github.com/Einlanzerous/catenary/issues/66)) ([2ae5b5e](https://github.com/Einlanzerous/catenary/commit/2ae5b5e4a772a4573fa5eb99ed767d2488e9705f))


### Bug Fixes

* CANT-119 — the soak harness fails the handover gate for its own timing, not the server's ([#67](https://github.com/Einlanzerous/catenary/issues/67)) ([5053015](https://github.com/Einlanzerous/catenary/commit/5053015ee6b0c26b3f447d871e3b04c008738db7))

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
