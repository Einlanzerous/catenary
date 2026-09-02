# CANT-13 — the initial schema and the migration harness

> **The Switchyard plan is the decision of record.** It is versioned, its criteria carry verdicts, and it gates the ticket: **Switchyard `CANT-13`, plan rev 3** (Switchyard is estate-internal, so there is no link that resolves from a clone of this public repository). This file is a derived stub. It carries **outcomes and no reasoning**, because reasoning is what drifts — two documents making the same argument is the CHRN-79 shape, and the second copy is the one that goes stale.

Three migrations, applied in-process on boot, against an empty database. Six tables plus `log_counter`. No backfill and no cutover, and that will never be true again in this project.

## Rulings, as settled

| | settled |
|---|---|
| **0 · `kind` or `type`** | `conversations.kind`, matching the wire. D4 and IDEA-23 say `type`; a translation layer between two names for one concept is where bugs live. |
| **1 · the whole `Transcript`** | `messages.transcript_text` **and** `attachments.transcript_state` **and** `attachments.transcript_json` all land now. Only CANT-59's `to_tsvector` index defers. |
| **2 · dedup scope** | `unique (author_id, client_id)`, matching `ClientSend.client_id`'s normative *"deduplicates on (account, client_id)"*. Not device-scoped: `sender_device_id` is nullable for bots and Postgres treats NULLs as distinct, so that scope gives device-less senders no deduplication at all. |
| **2 · dedup ordering** | Idempotency is checked **before either ordinal is drawn**. On the residual race, catch the unique violation, roll back, re-select by key. The rollback un-draws both ordinals because they come from rows rather than a sequence. |
| **3 · its own database and role** | `deploy/provision.sql`. Credentials in Signet, never in a compose file. |
| **4 · every FK's `ON DELETE`** | `messages.author_id` RESTRICT · `messages.reply_to` SET NULL · `attachments.message_id` CASCADE · the rest RESTRICT. **`messages.author_id` being RESTRICT is what keeps CANT-33 off the Mode C list**: a Purser offboard sets `users.deactivated_at` and cannot delete a user who has authored anything, so it cannot destroy authored messages. |
| **5 · `retention_days IS NULL`** | Inherit the global infinite default, **not** zero days. Stated as a column comment, with `CHECK (retention_days >= 1)` mirroring the wire's `minimum: 1`. |
| **6 · one `attachments` table** | Per-kind CHECKs enforce each kind's required set. |

## The three things the shape says that are easy to get wrong

- **`log_counter` holds exactly one row for the whole deployment**, enforced by `CHECK (id = 1)`. Not one per account: that would be dense, and sparsity is the entire division of labour between `log_seq` and `seq`.
- **`head_seq` is not a column.** It is served from `conversations.last_seq`, which the inserting transaction bumps under the conversation's row lock.
- **`messages.at` is server-assigned**, defaulted to `clock_timestamp()` rather than `now()` — a transaction's start time can order two concurrent inserts opposite to the row lock that ordered their seqs, and the wire promises timestamps sort chronologically as strings.

**Promoting a direct to a group** writes three things: `kind = 'group'`, a `name`, and `direct_key = NULL`.

## What each table deliberately omits, and who owns it

| table | omitted | owner |
|---|---|---|
| `users` | `kind` (person vs bot) | **CANT-73.** `users` is small, so a later `ADD COLUMN … DEFAULT 'person'` is free; it does not belong on the list of things that must be right first time. |
| `devices` | token lifetimes, refresh rotation | **CANT-28.** `devices` exists here so that ticket has something to hang a token on. |
| `conversations` | nothing | — |
| `conversation_members` | nothing | — |
| `messages` | what an edit or a delete **means**. `edited_at` and `deleted` exist; `updated_log_seq` exists so the ticket cannot be forced into a full-table rewrite. | **CANT-63.** |
| `messages` | the `to_tsvector` index over text and transcripts. The columns land here. | **CANT-59.** |
| `messages` | the ordinal **draw**. This migration creates the counters and constrains when they may be drawn; it does not implement it. | **CANT-14**, Mode C. |
| `attachments` | where the bytes live. `storage_key` is opaque for this reason and `url` is derived at serve time. | **CANT-47.** |

## The guard

`schema/mapping/wire-fields.json` maps all 116 wire fields to `column`, `derived` or `client-local`, and `internal/store/wiremap_test.go` fails on an unmapped field, a stale entry, a missing column, a type contradiction, and a required wire field over a nullable column that says nothing about why.

It exists because the wire schema and the DDL have no mechanical relation, so "the two disagree" was undefined and a guard written against it could only be reinterpreted. It is proved to fail four ways rather than asserted.

**It found four seams on its first run**, one of which neither plan revision had named: `Conversation.name` is **required** on the wire and the column is nullable, because a direct has no name — the rail shows the other member, which is per reader. The served name for a direct is derived, not stored.
