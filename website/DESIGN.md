---
name: Writ website
description: The landing page and docs of Writ, visual-first and sparse.
colors:
  bg: "#f6f4ec"
  surface: "#fdfcf6"
  surface-2: "#ece9dd"
  ink: "#1b1d2e"
  ink-2: "#45485a"
  ink-3: "#5d6072"
  rule: "#dedacb"
  rule-strong: "#c6c1b0"
  accent: "#33409a"
  accent-soft: "#e6e8f6"
  ok: "#1b6b4c"
  ok-soft: "#e0f0e6"
  warn: "#855600"
  warn-soft: "#f7eed6"
  bad: "#b03426"
  bad-soft: "#f9e6e0"
  btn: "#232a5c"
  gild: "#9a6204"
  mark: "#e2a425"
  term-bg: "#171a2c"
  term-ink: "#e8e8e2"
  term-dim: "#a0a2b3"
  dark-bg: "#10121f"
  dark-surface: "#171a2a"
  dark-surface-2: "#1e2133"
  dark-ink: "#ecebe4"
  dark-ink-2: "#b5b6c4"
  dark-ink-3: "#9193a5"
  dark-rule: "#282b3d"
  dark-rule-strong: "#3c4057"
  dark-accent: "#a3adf0"
  dark-accent-soft: "#1f2547"
  dark-ok: "#6fd0a0"
  dark-ok-soft: "#132a1f"
  dark-warn: "#f0bd5e"
  dark-warn-soft: "#2c2311"
  dark-bad: "#ff9a85"
  dark-bad-soft: "#351b15"
  dark-btn: "#ece6d2"
  dark-gild: "#ecb233"
  dark-term-bg: "#0a0b15"
typography:
  display:
    fontFamily: "Newsreader, Iowan Old Style, Palatino, Georgia, serif"
    fontSize: "clamp(2.25rem, 1.5rem + 2.8vw, 3.625rem)"
    fontWeight: 500
    lineHeight: 1.06
    letterSpacing: "-0.028em"
  headline:
    fontFamily: "Newsreader, Iowan Old Style, Palatino, Georgia, serif"
    fontSize: "clamp(1.625rem, 1.25rem + 1.4vw, 2.25rem)"
    fontWeight: 500
    lineHeight: 1.12
    letterSpacing: "-0.02em"
  title:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.0625rem"
    fontWeight: 600
    lineHeight: 1.3
  body:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.55
  code:
    fontFamily: "Chivo Mono Variable, ui-monospace, Menlo, Consolas, monospace"
    fontSize: "0.8125rem"
    fontWeight: 400
    lineHeight: 1.65
rounded:
  sm: "4px"
  md: "8px"
  lg: "12px"
spacing:
  gutter: "clamp(16px, 3vw, 24px)"
  section: "clamp(52px, 6.5vw, 88px)"
---

# Design System: Writ website

`PRODUCT.md` holds the product record. `docs/style.md` sets the voice for docs prose. The tokens above define the light theme in `app/global.css`; dark values live under `.dark` in the same file.

## Direction

The landing page is visual-first and sparse. Every section leads with a real product screenshot, an architecture diagram, real Rego/terminal output, or a compact icon grid. Long explanations, comparison tables, security lists, and FAQs live in `/docs`, not on the landing page.

**Core constraints:**

- Total visible prose on the landing page (excluding code blocks, nav, and footer) stays under 250 words.
- No paragraph on the landing page exceeds 25 words.
- Real product evidence above the fold: the hero pairs an 8-word headline, a one-line subhead, and two CTAs directly above a full-width framed capture of the real control UI with pure-CSS view tabs (Overview, Plan, Resources, Drift, Runs).
- Subtle motion only (workflow connector flow), disabled under `prefers-reduced-motion`.
- Full parity across light and dark themes and across 390 px phone and desktop viewports, with zero horizontal overflow.

## Colors

Warm paper ground (`bg`) with indigo-black ink (`ink`), crisp elevated surfaces (`surface`) with 1 px hairline rules (`rule`, `rule-strong`), indigo for links, icons, and the primary button (`accent`, `btn`), and ochre (`gild`, `mark`) reserved for the second line of the hero headline, the `writ serve` trust-boundary bar, the wordmark caret, and the scope limit caret.

All text pairs exceed 4.5:1 WCAG contrast in both light and dark themes:

| Pair | Light | Dark |
| --- | --- | --- |
| `ink` on `bg` | 15.1 | 15.6 |
| `ink-2` on `surface` | 8.8 | 8.6 |
| `ink-3` on `surface-2` | 5.1 | 5.2 |
| `accent` on `accent-soft` | 7.4 | 6.9 |
| `gild` on `bg` | 4.6 | 9.7 |
| `on-btn` on `btn` | 12.9 | 14.9 |
| `term-dim` on `term-bg` | 6.8 | 7.8 |

## Typography

- **Display and section headings (`h1`, `h2`):** Newsreader at weight 500.
- **UI, card titles, and body:** Archivo variable at weights 400 and 600 (800 only for the wordmark).
- **Code, terminal output, and URL bars:** Chivo Mono variable.

All fonts are self-hosted Latin subsets from pinned `@fontsource` packages with no external font requests.

## Layout & Sections

Container max width is `1160px` (`24px` side gutters on desktop, `16px` on phones). Sections sit on one ground separated by subtle 1 px dividers.

| Section | Visual lead | Purpose |
| --- | --- | --- |
| Header | Sticky 60 px bar | Wordmark, Docs, GitHub, search icon, theme switch, Quick start CTA. |
| Hero | Tabbed control UI window (`Overview`, `Plan`, `Resources`, `Drift`, `Runs`) | Direct category headline ("The self-hosted control plane for OpenTofu"), one-line differentiator subhead, two CTAs (`Quick start`, `GitHub`), real UI above the fold, and a 4-item open-source/platform proof strip. |
| Workflow (`#how`) | Inline SVG pipeline diagram (`Git push -> Plan -> Policy -> Approve -> Apply -> Audit`) | Shows the trust boundary between CI (OIDC only) and `writ serve` (cloud roles and state keys), with one link to `/docs/plan-to-apply`. |
| Features (`#features`) | 6-card icon grid (3x2 desktop, 1x6 phone) | Core capabilities: No CI cloud keys, OPA Rego policy, Masked secrets, Scheduled drift checks, Sandboxed Linux runs, MCP for agents. |
| Showcases (`#policy`) | Full-width UI captures + 5/7 Rego split | Full-width masked secret capture (`secret` on desktop, native `plan-390` on phones), 5/7 split Rego rule + refusal output from `content/landing.json`, and full-width drift table capture (`drift` on desktop, native `stack-390` on phones). |
| Try it (`#try`) | Elevated CTA band + copyable terminal | 4-line install and `writ serve --dev` command, Quick start and Compare CTAs, and one ochre-caret scope line linking to `/docs/status`. |
| Footer | Compact single band | Wordmark, Apache 2.0 notice, and links to docs, security, comparison, GitHub, and `llms.txt`. |

## Components

- **Product Window (`.product-window`):** 12 px radius, `1px solid var(--color-rule-strong)` border, layered elevation shadow, pure-CSS radio tabs in the bar so view switching requires no JavaScript. Uses `<Shot>` for automatic light/dark and 1280/390 responsive captures.
- **Pipeline Diagram (`.pipe-diagram`):** Dashed CI zone and solid `writ serve` zone with a 2 px ochre top accent, inline vector stage icons, and subtle animated connector flow that stops under `prefers-reduced-motion`. Stacks cleanly on narrow screens.
- **Feature Cards (`.feat-card`):** `var(--color-surface)` with 1 px hairline border, 36 px tinted icon tile, short title, and one concise sentence.
- **Terminals (`.term`):** Dark in both themes, build-time Shiki tokenization via `lib/code.tsx`, copy button via `CopyButton`. Every snippet comes from `content/landing.json` cut by `scripts/gen-content.ts`.
- **Limit (`.limit`):** Ochre proofreader's caret beside the concise platform scope note.

## Screenshots

`just ui-shots [ref]` captures every control UI image in `public/ui/` (light and dark, 1280 and 390 widths) and writes dimensions and paths to `lib/ui-shots.json`. Re-run after control UI changes.
