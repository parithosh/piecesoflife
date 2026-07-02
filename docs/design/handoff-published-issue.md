# Handoff: PiecesOfLife — Published Issue (paginated magazine reading view)

## Overview
Redesign of the **published issue** page for PiecesOfLife (a private, self-hosted newsletter for a small group). After an issue is published, members read everyone's answers here. This spec is the **paginated ("one question per spread")** reading experience the team chose — it should read like a printed periodical: one prompt per page, generous serif columns, quiet chrome, comments tucked away until wanted.

This supersedes the current published-issue layout (which had a full-width text column, a heavy dashboard-style filter toolbar, and an always-open comment box under every answer).

## About this file
`PiecesOfLife.dc.html` is a **design reference prototype in HTML/CSS** — it shows the intended look/behavior, not production code to paste in. Recreate it inside the existing **server-rendered Go + HTML-template + static-CSS** app, using the app's own templates, routes, and CSS. Everything in the prototype is inline-styled and legible; translate those inline styles into your static CSS (ideally a few CSS custom properties + small utility classes).

**Implement section `4c`** (paginated reading view). Section `4a` is the same content as a continuous scroll — keep it around if you want to offer a scroll/focus toggle (recommended as a nice-to-have, see "Optional: mode toggle"), but `4c` is the target. Ignore turns 1–3 except as the shared style system (tokens/motifs below).

## Fidelity
**High-fidelity.** Match the colors, fonts, spacing, and treatments below. Keep semantics accessible and keyboard-friendly.

---

## Design tokens

### Color
| Token | Hex | Use |
|---|---|---|
| `--rani` | `#7a0f38` | Primary. Nav bg, question headings, primary buttons, avatar (self) |
| `--sindoor` | `#c3362b` | Drop-cap letter, accents |
| `--marigold` | `#e6b23c` | Gold accents: active nav underline, rule under headings, "next" arrow, active pager dot |
| `--zari` | `#c8962c` | Deeper gold: thin rules, dashed borders, small arrows |
| `--peacock` | `#0e6b6b` | Eyebrows/kickers ("QUESTION TWO", "WOVEN & POSTED"), other members' avatar, active comment toggle |
| `--ivory` | `#fbf4e3` | Page/card background |
| `--ivory-2` | `#fffaf0` | (inputs/fields if needed) |
| `--strip` | `#f7edd8` | Utility/pager strip background |
| `--ink` | `#2a0e15` | Body reading text (high contrast) |
| `--ink-soft` | `#8a6d55` | Captions, muted meta |
| `--muted` | `#a99374` | Inactive labels, "add a comment" |
| `--line` | `#e6d3ae` / `#e4d3b2` / `#dcc9a6` | Hairline rules (answer divider / strip border / ornament) |

### Type (Google Fonts)
Import:
```
https://fonts.googleapis.com/css2?family=Rozha+One&family=Newsreader:ital,opsz,wght@0,6..72,400;0,6..72,500;1,6..72,400;1,6..72,500&family=Mukta:wght@400;500;600;700&display=swap
```
- **`Rozha One`** (400) — display: month name, question headings (~40px), drop-cap letter, section labels. High-contrast Devanagari-flavored serif.
- **`Newsreader`** (400/500, + italic) — reading: all answer bodies (19px / line-height 1.72), comments (15px), captions (italic).
- **`Mukta`** (400–700) — UI: eyebrows, bylines, pager labels, nav, comment toggles. Usually uppercase with `letter-spacing` .04–.22em.
- Wordmark in nav is `Marcellus` 22px (optional; can standardize to Rozha One).

### Shape
- Card: `border:1.5px solid var(--rani)`, `border-radius:5px`, offset print shadow `14px 14px 0 rgba(122,15,56,.18)`.
- Buttons / pager: `border-radius:4px`.
- Reading measure (the important one): **answer column capped at `max-width:620px`, centered.** ~62–66 characters. Do not let body text run full width.

### Shared motifs (from the "Pallu" system — reuse the same CSS everywhere)
```css
/* gold thread rule under the nav */
.zari{height:8px;border-top:1px solid #a8791f;border-bottom:1px solid #a8791f;
  background:repeating-linear-gradient(90deg,#c8962c 0 8px,#e6b23c 8px 12px,#c8962c 12px 20px)}
/* allover paisley (buti) overlay — asset mango.png, used at low opacity behind panels */
.mango{background-image:url(/static/img/mango.png);background-size:84px auto}
```
Assets to ship: `logo.svg` (brand medallion — rani ground, marigold ornament, no border) and `mango.png` (gold paisley tile). Both are in this bundle's `assets/`.

---

## Layout — the paginated spread (section 4c)

Top to bottom, inside the framed ivory card:

### 1. Nav bar
Rani (`--rani`) bar: left = `logo.svg` (32px) + "PiecesOfLife" wordmark; right = ISSUES (active: marigold text + 2px marigold underline) / PHOTOS / circular avatar. Directly below the nav: the `.zari` gold rule.

### 2. Issue strip (context + progress)
A thin strip (`--strip` bg, bottom border `--line`), space-between:
- **Left:** small `logo.svg` (30px) + a two-line block — kicker "WOVEN & POSTED" (Mukta 700, 10px, `.22em`, `--peacock`) over the issue name "July 2026" (Rozha One, ~22px, `--ink`).
- **Right:** a **segmented progress bar** = one pill per question. Answered/past questions filled `--rani`, the **current** one filled `--marigold` (and slightly wider), upcoming ones `#e0cfae`. Followed by a label "Question 2 of 6" (Mukta, `--muted`).

### 3. Single-question spread (the page body)
`padding: ~46px 36px 30px`, with a faint `.mango` overlay (opacity ~.16) bleeding from the top-left corner behind the content. Inner column `max-width:620px; margin:0 auto`:
- **Kicker:** "QUESTION TWO" — Mukta 700, 12px, `.2em`, `--peacock`, centered.
- **Heading:** the question — Rozha One ~40px / line-height 1.14, `--rani`, centered, `text-wrap:balance`.
- **Ornament rule:** centered — a hairline, a small marigold diamond (8px square rotated 45°), a hairline.
- **Answers** (one per member who answered this question), stacked:
  - **Byline row:** circular avatar (36px; self = `--rani` bg, others = `--peacock` bg, initial in ivory) + name in Mukta 700, 14px, `.04em`, `--ink`. (Optional second line: location/pronoun in Mukta 12px `--muted`.)
  - **Body:** Newsreader 19px / 1.72, `--ink`. **The first answer on the page gets a drop cap:** first letter in Rozha One ~62px, `--sindoor`, `float:left`, `margin:7px 12px 0 0`.
  - **Photo (if attached):** a `<figure>` — framed plate (`border:1px solid var(--rani)`, radius 4px, ~230px tall) with an italic Newsreader caption (`--ink-soft`) beneath. (In the prototype the plate is a placeholder fill; use the real image.)
  - **Comment toggle line** (see Comments below).
  - Answers after the first are separated by a top border `1px solid var(--line)` with ~30px padding.

### 4. Pager (bottom)
A strip (`--strip` bg, top border `--line`), space-between, three parts:
- **Previous button** (left): ivory bg, `1.5px solid #e2cfa8` border, radius 4px. Contains a `--zari` "←", a tiny "PREVIOUS" label (Mukta 10px, `.1em`, `--muted`), and the previous question's text (truncated). Hidden/disabled on the first question.
- **Dot indicators** (center): one dot per question; current = marigold diamond, past = `--rani` dot, upcoming = `#e0cfae` dots. Clicking a dot jumps to that question.
- **Next button** (right): `--rani` bg, ivory text, radius 4px. A marigold "NEXT" label + next question's text + a marigold "→". On the last question this becomes "Back to all issues" (or a "Finish" affordance).

---

## Comments (collapsed by default)
Do **not** render an open comment box under every answer. Instead, per answer show a single quiet toggle line:
- If comments exist: `▾ N comments` (Mukta 600, 12px, `--peacock`), expands the thread.
- If none: `▸ add a comment` (same, `--muted`), expands a composer.

When expanded, the thread is indented with a left rule (`2px solid #e6d3ae`, `padding-left:16px`), each comment = name (Mukta 700, `--rani`) + "· just now" (Mukta 500, `--muted`) + body (Newsreader 15px / 1.5). The composer is a **one-line** field ("Add a comment…", italic Newsreader, bottom-border only) that expands to a full textarea + Post button on focus. Post button = `--rani` bg, ivory text.

No "No comments yet." empty text — the `▸ add a comment` line is the whole empty state.

---

## Interactions & behavior
- **Pagination** = client-side or server-rendered per-question view. **Give each question a deep-linkable URL** (e.g. `/issues/{id}?q={n}` or `/issues/{id}/q/{slug}`) so a member can share/bookmark a specific prompt and the browser back button works. Preserve `Who`/sort params across pages.
- **Keyboard:** `←` / `→` move between questions; pager buttons and dots are real `<a>`/`<button>` with focus states and `aria-label`s ("Previous question: …", "Go to question 3"). Progress strip and dots get `aria-current` on the active one.
- **Comment toggle:** button with `aria-expanded`; composer textarea expands on focus; Post disabled while empty.
- **Publishing already happened** — this view is read-only for answers (no editing); only comments are interactive.
- **Photos:** click to open full-size (lightbox or new view), with `alt` text.

## Responsive / mobile (first-class)
- Single column; question heading scales to ~30px; answer column is just the padded full width (still readable since it's one phone-width column).
- Pager becomes a sticky bottom bar: `←` and `→` icon buttons flanking the "Question 2 of 6" label; the question-title previews can drop on narrow widths. Keep 44px min tap targets.
- Issue-strip progress pills can collapse to just "2 / 6" on very narrow screens.
- `.mango` overlay opacity should stay low enough never to hurt text contrast.

## Optional: mode toggle (recommended, not required)
If you want to also offer the continuous-scroll reading view (prototype section `4a`), add a small "Read: One at a time · All at once" toggle in the issue strip. Default to **paginated**. The scroll view is the same components without the pager, all questions stacked, with a small diamond ornament as a section break between questions. It's the better mobile default and makes deep-links trivial — consider switching to scroll automatically on small screens.

## Assets (in `assets/`)
- `logo.svg` — brand medallion (ship in static assets).
- `mango.png` — paisley buti tile for `.mango` overlays.
- `source-*.png` — client-supplied originals these were derived from (reference only; don't ship).

## Files
- `PiecesOfLife.dc.html` — full prototype. Implement section **`4c`**; read its inline styles for exact values. `4a` = scroll variant, `4d` = the scroll-vs-paginated rationale.
- `README.md` — this document (self-sufficient).
