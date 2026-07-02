# Handoff: PiecesOfLife — "Pallu" saree-woven redesign

## Overview
A cohesive UI/UX redesign for **PiecesOfLife**, a self-hosted private newsletter for small friend/family groups. Members answer prompts each round, attach photos/links, and later read a compiled "issue" of everyone's responses. The redesign gives the app a warm, ceremonial, unmistakably-designed identity drawn from **saree textile design** ("Pallu"): temple/tent zari borders, an allover mango (paisley/ambi) buti print, gold-thread rules, a brocade medallion logo, and a rich rani-pink / peacock-teal / marigold palette on ivory.

## About the design files
`PiecesOfLife.dc.html` is a **design reference prototype written in HTML/CSS** — it shows the intended look, layout, states, and copy. It is **not production code to paste in**. Your task is to **recreate this visual system inside the existing server-rendered Go + HTML-template + static-CSS app**, using the app's existing template structure and routes. Translate the inline styles in the prototype into your static CSS (ideally a small set of CSS custom properties / utility classes), and apply the markup patterns to the existing Go templates.

> The prototype is a single-file "design canvas" containing THREE explored directions stacked as sections (turn 1 = calm light theme, turn 2 = riso periodical, turn 3 = **"Pallu" saree — the approved direction**). **Implement turn 3 (options `3a`–`3d`).** Turns 1–2 are kept only for reference and can be ignored.

> To VIEW the prototype: it is a "Design Component" that needs its `support.js` runtime, so it renders in the design tool it was authored in. For a plain-browser reference, ask the designer for a standalone bundled HTML export (offered below), or read the markup/styles directly — everything is inline and legible.

## Fidelity
**High-fidelity.** Exact colors, fonts, spacing, radii, and status treatments are specified below and should be matched. Recreate pixel-close using your own CSS; keep the existing app's HTML semantics and accessibility.

---

## Design tokens

### Color
| Token | Hex | Use |
|---|---|---|
| `--rani` | `#7a0f38` | Primary. Nav/header bg, masthead accents, primary text on ivory headings |
| `--rani-deep` | `#5c0a2a` | Hover/pressed on rani |
| `--sindoor` | `#c3362b` | Vermillion — submitted state, drop-caps, alerts, active editor border |
| `--marigold` | `#e6b23c` | Gold accent — borders, active nav underline, "waiting" chips, buttons on rani |
| `--zari` | `#c8962c` | Deeper gold — gold-thread rules, dashed borders |
| `--peacock` | `#0e6b6b` | Secondary. Member-ledger panels, "saved" state, section eyebrows |
| `--sage` | `#4f7042` | "Collecting" / progress-complete green |
| `--ivory` | `#fbf4e3` | Page/card background |
| `--ivory-2` | `#fffaf0` | Input/answer field background |
| `--ink` | `#2a0e15` | Body & reading text (high contrast on ivory) |
| `--ink-soft` | `#8a6d55` | Muted/meta text |
| `--line` | `#ddc9a6` | Hairline dividers on ivory |

### Type
- **Display / headings & numerals:** `Rozha One` (Google Fonts, 400). Used for month names ("June 2026"), big numerals (01/02/03, stat counts), section titles, "PiecesOfLife Demo".
- **Reading / long-form:** `Newsreader` (Google Fonts, 400/500, italic available). Used for answers, prompts' supporting copy, member quotes, drop-cap paragraphs.
- **UI labels / meta / buttons:** `Mukta` (Google Fonts, 400/500/600/700). Used for eyebrows, status chips, nav, button labels — usually uppercase with `letter-spacing:.04–.24em`.
- Wordmark "PiecesOfLife" in nav bars is currently `Marcellus` 22px; you may standardize it to Rozha One.

Google Fonts import:
```
https://fonts.googleapis.com/css2?family=Rozha+One&family=Newsreader:ital,opsz,wght@0,6..72,400;0,6..72,500;1,6..72,400;1,6..72,500&family=Mukta:wght@400;500;600;700&display=swap
```

### Radius, borders, shadow
- Card radius `5px` (deliberately low — feels like printed cloth, not app chrome). Chips/buttons `3px`. Logo tile `7–13px`.
- Card border: `1.5px solid var(--rani)`.
- Card drop shadow (offset, print-like): `14px 14px 0 rgba(122,15,56,.18)`.
- Active editor field: `2px solid var(--sindoor)`. Saved field: `1px solid var(--rani)`.

### Status labels (use everywhere state is shown)
Pill, `Mukta` 700, ~11px, `letter-spacing:.05em`, `border-radius:3px`, a leading `●` or `✓`/`▲`:
- **Draft** — text `#fff` on `#8a6d55`
- **Saved** — `#fff` on `--peacock` (`● SAVED 2m`)
- **Saving…** — `--ink` on `--marigold`
- **Collecting** — `#fff` on `--sage`
- **Waiting** — `--ink` on `--marigold` (`● WAITING`)
- **Submitted** — `#fff` on `--sindoor` (`✓ SUBMITTED`)
- **Published** — `--marigold` text on `--rani` (`▲ PUBLISHED`)

---

## Motifs (reusable CSS + assets)

**1. Temple / tent zari border** (`.temple`, and flipped `.temple-up`) — a band of downward gold triangles on rani, capped with a gold rule. Put at top of mastheads and bottom of framed cards.
```css
.temple{height:26px;background:#7a0f38;
  background-image:linear-gradient(135deg,#e6b23c 26%,transparent 26%),
                   linear-gradient(225deg,#e6b23c 26%,transparent 26%);
  background-size:26px 26px;border-bottom:3px solid #c8962c}
.temple-up{height:26px;background:#7a0f38;
  background-image:linear-gradient(45deg,#e6b23c 26%,transparent 26%),
                   linear-gradient(-45deg,#e6b23c 26%,transparent 26%);
  background-size:26px 26px;background-position:0 100%;border-top:3px solid #c8962c}
```

**2. Zari gold-thread rule** (`.zari`) — thin woven gold divider under nav bars.
```css
.zari{height:8px;border-top:1px solid #a8791f;border-bottom:1px solid #a8791f;
  background:repeating-linear-gradient(90deg,#c8962c 0 8px,#e6b23c 8px 12px,#c8962c 12px 20px)}
```

**3. Mango buti print** (`assets/mango.png`) — an allover paisley print (recolored from the client's supplied paisley `assets/source-paisley.png` to gold-on-transparent). Use as a low-opacity overlay layer, NOT a full-strength background:
```css
.mango{background-image:url(/static/img/mango.png);background-size:84px auto}
```
Place as an absolutely-positioned layer with `opacity` per context: `0.5` beside content (direction/side panels), `0.3` behind the ivory masthead title, `0.2` over the teal ledger. Keep it clear of body text.

**4. Brocade medallion logo** (`assets/logo.svg`) — a hand-built vector interpretation of the client's supplied brocade tile (`assets/source-medallion.png`): an 8-arm floral snowflake in a double-diamond frame, marigold ornament on a **rani** ground (no border — it melts into the rani nav bar). Ships as pure SVG; recolor via the fills if needed. Sizes used: 80px (direction header emblem), 76px (masthead emblem, centered), 32px (nav-bar tile beside wordmark).

---

## Screens / views (implement 3a–3d)

### 3b — Member response editor  *(the heart; route `/issues/{id}/respond`)*
Two-column frame. **Left ledger** (`--peacock` bg, 280px, faint mango overlay): eyebrow "OPEN NOW · YOUR TURN", big month in Rozha One, one line of context (prompt count + "closes in N days · private until the issue is woven"), a gold divider, a "PROGRESS — 2 OF 3" segmented bar (filled segments `--marigold`), and an "IN THIS ROUND" member list with per-member status word (WRITING/WAITING). **Right column** (ivory): one block per question — big Rozha numeral (`01/02/03`) at left (color `#d9bf94` inactive, `--sindoor` for the one being edited), prompt in Rozha, answer field in Newsreader (`--ivory-2`), a status chip top-right, and `＋ PHOTO` / `🔗 LINK` outline buttons. Empty question uses a dashed `--zari` border and italic Newsreader placeholder. **Submit bar** at bottom: rani panel, "Edit anytime until June is woven & posted." + `PREVIEW` (outline) and `SUBMIT TO THE ISSUE →` (marigold) buttons.
- **Autosave states** cycle on the active field: field border `--sindoor` while focused; chip goes `● SAVING…` (marigold) → `● SAVED {relative time}` (peacock). Persist drafts server-side/localStorage as today.
- **Question suggestion**: keep the existing suggest-a-question form; style as a small ivory card with a Newsreader input + marigold `Send suggestion` button (see turn-1 `1b` right rail for the pattern if you need one).

### 3c — Published issue (reading view)  *(route `/issues/{id}` when published)*
Full masthead: nav → `.temple` → centered **logo.svg emblem** → eyebrow "ISSUE No.N · WOVEN & POSTED" (peacock) → month in Rozha One (~62px) → "N people · N answers · N photos" → `.zari`. Faint `.mango` (opacity .3) behind the masthead. Article body max-width ~600px, centered: each **prompt** as a Rozha heading, then each member's answer as `avatar + NAME (Mukta) + answer (Newsreader 18px/1.65)`. First answer uses a `--sindoor` Rozha **drop-cap**. Reactions = small ivory pills (`❤ 1`, `🌿 2`) + dashed `＋ REACT`. Comments = `--marigold` left-rule, `NAME` (Mukta) + comment (Newsreader). Close card with `.temple-up`.

### 3d — Admin dashboard  *(route `/admin`)*
Header "THE LOOM" eyebrow + loop name (Rozha). Tabs: Members / Questions / Settings (Mukta outline chips). Main framed card: rani header row with month + `● COLLECTING · 6D 23H` chip + `SEND REMINDER` (outline) and `▲ PUBLISH NOW` (marigold) buttons. Stat row (bordered cells): **Members responded** with big Rozha `N / M`, a progress bar, **and the participation toggle** (see below); plus Cadence and Photos cells. Then "WHO HAS RESPONDED" ledger: per member `avatar + name/email + status chip + NUDGE button` (nudge only for waiting members).

**⚠ Progress-count behavior (product decision to implement):** today progress counts *all* active users including admins, which reads oddly (e.g. `0 / 2` when the only non-admin hasn't answered). **Change: exclude admins from the denominator by default**, and add a per-issue **"Count me in this round"** toggle on the admin (the green switch in the stat cell) that adds the admin back into the expected-responders set when on. Label copy: "Admins are left out of progress unless this is on."

### 3a — Brand/system reference
Not a screen — the palette, type, motif, and status-label reference. Use it to build your CSS tokens/partials.

### Also specified in the prototype (turns 1–2, same content, apply the 3x styling)
Archive with "Open now" panel + honest empty states (`1d`/`2d`), onboarding wizard (`1g`), question bank + settings (`1h`), profile/preferences (`1j`), the four empty states (`1i`), and mobile frames (`1k`). Re-skin these with the Pallu tokens/motifs above.

---

## Interactions & behavior
- **Autosave**: debounce on input; optimistic `Saving…` → `Saved` with relative timestamp; drafts editable until publish.
- **Submit**: marks the member submitted (status chip → `✓ SUBMITTED`); answers remain **editable until the issue is published**.
- **Publish** (admin): locks all responses (no more edits), flips issue to Published, reveals the reading view.
- **Nudge / Send reminder**: existing reminder actions; just restyled.
- **Nav active state**: current section label in `--marigold` with a 2px marigold underline.
- Buttons/toggles/chips need visible `:hover`, `:focus-visible`, and pressed states; keep everything keyboard-reachable and label all icon-only controls.

## Responsive / mobile (first-class)
- Single column below ~720px: the editor's left ledger collapses to a compact header strip above the questions; the submit bar becomes a **sticky bottom bar** (`PREVIEW` + `SUBMIT`), full-width buttons.
- Reading view: masthead scales down (month ~32px), answers stack; keep the temple/zari bands.
- Min tap target 44px; body/reading text ≥16px on mobile.
- Motif overlays (`.mango`) should sit behind content and never reduce text contrast — drop their opacity further on small screens if needed.

## Assets (in `assets/`)
- `logo.svg` — brand medallion (rani ground, marigold ornament, borderless). Ship in your static assets.
- `mango.png` — gold paisley buti tile for the `.mango` overlay.
- `source-paisley.png`, `source-medallion.png` — the client-supplied originals the above were derived from (reference only; do not ship).

## Files
- `PiecesOfLife.dc.html` — the full design prototype (all three directions). Implement sections `3a`–`3d`; read inline styles for exact values.
- `README.md` — this document (self-sufficient).
