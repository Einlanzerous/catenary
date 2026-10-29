# CANT-332 — tokens: the focus ring, the pressed border, `text.meta` contrast and the `↗ LINK` chip

> **The Switchyard plan on CANT-332 is the decision of record** (rev 6, approved). This file is a derived stub. It carries **outcomes, the picked values per theme and the measured ratios, not the argument**, because the argument is what drifts and the second copy of it is the one that goes stale.

Built by **CANT-342** (the tokens, both runtimes, PR #249), **CANT-343** (the web half, PR #253) and **CANT-344** (the Flutter half, PR #252). **CANT-346** (light `text-quote` at 3.99:1, and the `accent-wire` comment) is filed alongside and open.

## What was picked

| ruling | outcome |
|---|---|
| 0 · the focus ring | `text.primary` everywhere; **no new token** |
| 1 · the pressed and active border | a new token, **`line-strong`**, dark `#626D6C`, light `#7C827E` |
| 2 · `text.meta` | **raised**, dark `#747E7D`, light `#666B69` |
| 3 · the `↗ LINK` chip | **never copper**, guarded on both sides |
| 4 · meta text on `surface.raised` | takes **`text.secondary`** (the open rail row's stamp, the lightbox bar); raised `text.meta` stays as a named exception at its measured 4.15:1 |

## Values

| token | dark | light |
|---|---|---|
| `line-strong` | `#626D6C` | `#7C827E` |
| `text-meta` | `#747E7D` | `#666B69` |

Both live in `web/src/styles/tokens.css` and `app/lib/tokens.dart` (`lineStrong`, `textMeta`), held equal by `app/test/tokens_test.dart`.

## Measured ratios

WCAG relative luminance with the 0.04045 sRGB threshold, unrounded, asserted in `app/test/tokens_test.dart` and recomputed from both token files on main.

| pair | ground | dark | light |
|---|---|---|---|
| `text-meta` | `surface-base` | 4.659 | 5.018 |
| `text-meta` | `surface-rail` | 4.535 | 4.636 |
| `text-meta` | `surface-lift` | 4.50001 | 4.517 |
| `text-meta` | `surface-raised` | 4.151 | 4.153 |
| `line-strong` | `surface-base` | 3.639 | 3.631 |
| `line-strong` | `surface-rail` | 3.542 | 3.354 |
| `line-strong` | `surface-lift` | 3.515 | 3.268 |
| `line-strong` | `surface-raised` | 3.243 | 3.005 |
| `text-secondary` | `surface-raised` | 6.621 | 4.718 |
| `text-primary` | `surface-raised` | 14.560 | 13.536 |

`text-primary` and `text-secondary` clear 4.5:1 on all four grounds in both themes; the lowest is light `text-secondary` on raised, above. `text-meta` is asserted at 4.5:1 or better on base, rail and lift, and at a floor of 4.15 on raised, which fails the build below it. `line-strong` is asserted at 3:1 or better on all four grounds.

## The `↗ LINK` guard

- **Web:** `web/linkpreview.test.ts` SSR-renders `ReplyStub.vue` and fails if the chip takes the accent class or its CSS rule stops resolving to `--text-meta`.
- **Flutter:** the quoted-stub test in `app/test/link_preview_test.dart` asserts the chip's color is `textMeta`, with `reason: 'not copper'`.

## Where each half landed

- **Web (CANT-343):** the `:focus-visible` outlines in `base.css` and `ImageAttachment.vue` take `var(--text-primary)`; the pressed link card's border is `var(--line-strong)`; the open rail row's quiet stamp and the lightbox bar take `text.secondary`; `rail-new.test.ts` fails if the stamp or bar revert to `text.meta`.
- **Flutter (CANT-344):** the link card's pressed border takes `lineStrong`; an open rail row's stamp, with nothing unread, takes `textSecondary`.
- `git grep -n 'TODO(CANT-332)'` returns nothing on main.

## Disclosed in the rows

- **`surface-avatar` is outside the `line-strong` bound.** It is a tile fill, not a ground a border sits on; the picked values give 2.40:1 dark and 2.50:1 light there, which no value picked here could lift to 3:1.
- **Light `line-strong` on raised passes by 0.005** (3.0051 against a floor of 3.0). That margin is accepted.
- **CANT-343 added `.frame:focus-visible::after`.** The image frame's inset outline was painted under its own canvas and image, so on a photo the ring never showed; the frame's last child now draws the same ring at the same width and offset.

## Not verified

These are computed ratios only: no rendering on a physical display, and no check of anti-aliased thin strokes. The `↗ LINK` guards observe the emitted class and the resolved rule, not the pixels.
