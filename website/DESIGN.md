---
name: Writ website
description: The landing page and docs of Writ, set like a technical record.
colors:
  bg: "#f6f7f9"
  surface: "#ffffff"
  surface-2: "#eef0f3"
  ink: "#14171c"
  ink-2: "#454c59"
  ink-3: "#636b7b"
  rule: "#dde1e7"
  rule-strong: "#c3c9d2"
  accent: "#2f55d4"
  accent-soft: "#e8edfc"
  ok: "#17703f"
  ok-soft: "#e3f3e9"
  warn: "#855600"
  warn-soft: "#faf0d9"
  bad: "#b4232a"
  bad-soft: "#fbe8e8"
  mark: "#ff8f6b"
  term-bg: "#14171c"
  term-ink: "#e6e9ee"
  term-dim: "#9aa2b1"
  # The dark theme, under .dark in app/global.css.
  dark-bg: "#0d0f12"
  dark-surface: "#15181d"
  dark-surface-2: "#1b1f25"
  dark-ink: "#eceef2"
  dark-ink-2: "#b0b7c3"
  dark-ink-3: "#8f97a5"
  dark-rule: "#272c34"
  dark-rule-strong: "#3a414c"
  dark-accent: "#8fa8ff"
  dark-accent-soft: "#1b2440"
  dark-ok: "#6ad496"
  dark-ok-soft: "#14291d"
  dark-warn: "#f0bd5e"
  dark-warn-soft: "#2c2311"
  dark-bad: "#ff9090"
  dark-bad-soft: "#331819"
  dark-mark-ground: "#23272e"
  dark-term-bg: "#090a0c"
  # Syntax colors of code blocks: light, then the dark set that terminals also take.
  syn-comment: "#5a6273"
  syn-punct: "#4b5260"
  syn-type: "#0b6676"
  syn-function: "#7a35a8"
  syn-dark-punct: "#a9b0bc"
  syn-dark-string: "#7fd6a0"
  syn-dark-number: "#f2c26b"
  syn-dark-type: "#7fd1de"
  syn-dark-function: "#d0a7ff"
  syn-dark-remove: "#ff9c9c"
typography:
  display:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(2.5rem, 1.25rem + 5vw, 4.5rem)"
    fontWeight: 680
    lineHeight: 1.02
    letterSpacing: "-0.03em"
  headline:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.625rem, 1.25rem + 1.5vw, 2.25rem)"
    fontWeight: 650
    lineHeight: 1.12
    letterSpacing: "-0.02em"
  title:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.25rem"
    fontWeight: 650
    lineHeight: 1.25
    letterSpacing: "-0.016em"
  page-title:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.875rem, 1.5rem + 1.4vw, 2.375rem)"
    fontWeight: 680
    lineHeight: 1.08
    letterSpacing: "-0.03em"
  title-sm:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.0625rem"
    fontWeight: 650
    lineHeight: 1.35
  lede:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(1.0625rem, 1rem + 0.3vw, 1.1875rem)"
    fontWeight: 400
    lineHeight: 1.55
  lede-docs:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 400
    lineHeight: 1.55
  body:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "17px"
    fontWeight: 400
    lineHeight: 1.6
  ui:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1rem"
    fontWeight: 400
    lineHeight: 1.6
  small:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 400
    lineHeight: 1.6
  label:
    fontFamily: "Archivo Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.8125rem"
    fontWeight: 400
    lineHeight: 1.6
  code:
    fontFamily: "Chivo Mono Variable, ui-monospace, Menlo, Consolas, monospace"
    fontSize: "0.8125rem"
    fontWeight: 400
    lineHeight: 1.65
rounded:
  sm: "4px"
  md: "8px"
  pill: "999px"
spacing:
  gutter: "clamp(24px, 4vw, 56px)"
  section: "clamp(64px, 7.6vw, 112px)"
  section-top: "clamp(24px, 2.8vw, 36px)"
components:
  button-primary:
    backgroundColor: "{colors.ink}"
    textColor: "{colors.surface}"
    rounded: "{rounded.md}"
    padding: "0 20px"
    height: "46px"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    padding: "0 20px"
    height: "46px"
  terminal:
    backgroundColor: "{colors.term-bg}"
    textColor: "{colors.term-ink}"
    rounded: "{rounded.md}"
  frame:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.md}"
---

# Design System: Writ website

`PRODUCT.md` has the product record. `docs/style.md` has the voice, and it wins for prose on docs pages. The token values above are the light theme of `app/global.css`. The dark values are in that file under `.dark`.

## Overview

**Creative North Star: "The written record"**

A writ is a written order. The site is set like one: a technical record that an engineer reads to decide whether to trust the thing it describes. One pale ground, ink, one left-aligned grid, and hairline rules between sections. The only boxes on a page are the things that are objects: a screenshot of the control UI, and a terminal with real output. Everything else is type on the ground, grouped by space.

The landing page was rebuilt on 2026-11-17 after a critique with the Impeccable rubric. The page before it had honest content in the standard product-page template: a chip over a centered headline, a fact bar, icon cards, numbered step cards, a bento grid, and a dark closing band. That form is what this system replaces. The palette, the two typefaces, the screenshots, and the three-step figure's script carry over from it.

**Key Characteristics:**

- Left-aligned on one 12-column rhythm: a section's heading takes 5 columns and its lede takes 7.
- Rules, not cards. A 1 px rule opens each section and divides each row.
- Real product only: captures of the control UI and output from real runs, each with a caption that says where it came from.
- A limit sits beside its claim at body size, marked with the caret of the wordmark.
- Calm in both themes. Nothing moves except the three-step figure.

## Colors

A cool neutral ground with ink text, one blue accent, the product's own status colors, and one coral mark.

### Primary

- **Ink** (`ink`): headings, body, the primary button, the border of the Writ node in the map, and the rule that opens the comparison table.
- **Link Blue** (`accent`): links, focus rings, and the active step of the three-step figure. It never fills a large area.

### Secondary

- **Caret Coral** (`mark`): the caret in the wordmark, and the small caret before each limit. Nowhere else.

### Neutral

- **Ground** (`bg`): the whole page, in every section.
- **Surface** (`surface`) and **Surface 2** (`surface-2`): inside frames, the Writ node, inline code, and the frame's address bar.
- **Ink 2** and **Ink 3**: secondary text and captions.
- **Rule** and **Rule Strong**: 1 px dividers. `rule-strong` opens a section and borders an object. `rule` divides rows inside one.
- **Ok, Warn, Bad** with their soft grounds: status only, as the product uses them. Applied and approved are green, pending is amber, refused and failed are red.

### Named Rules

**The One Ground Rule.** Every section sits on `bg`. Sections are separated by a rule and space, never by a band of another color.

**The Caret Rule.** The coral caret means "here is a limit". It marks every limit sentence and every item of "What Writ isn't, yet", and nothing else.

**The Status Rule.** Green, amber, and red appear only where the product would show that status. A check mark beside a fact isn't a status, so facts get no icon.

Contrast, computed from the tokens with the WCAG formula on 2026-11-16. Every text pair is at least 4.5:1 in both themes:

| Pair | Light | Dark |
| --- | --- | --- |
| `ink` on `bg` | 16.8 | 16.5 |
| `ink-2` on `surface` | 8.6 | 8.8 |
| `ink-3` on `surface-2`, the lowest text pair | 4.7 | 5.6 |
| `accent` on `accent-soft` | 5.3 | 6.7 |
| `ok` on `ok-soft`, `warn` on `warn-soft`, `bad` on `bad-soft` | 5.3, 5.6, 5.5 | 8.4, 9.0, 7.5 |
| Terminal dim text on the terminal | 7.0 | 7.7 |
| The docs' lowest syntax color on `surface-2` | 5.4 | 6.4 |

The caret is a graphic beside a sentence that states the limit in words, so no meaning rests on its color. The 1 px rules are about 1.6:1, and no control depends on one alone.

## Typography

**Display and Body Font:** Archivo, variable, at its normal width (with ui-sans-serif, system-ui)
**Code Font:** Chivo Mono, variable (with ui-monospace, Menlo, Consolas)

**Character:** one grotesque doing every job, with the hierarchy carried by size and weight. Mono is for code, output, and addresses only.

Both faces are self-hosted Latin subsets from pinned `@fontsource` packages.

### Hierarchy

- **Display** (680, 40 to 72 px fluid, 1.02): the landing headline only. At most 13.5em wide, so it breaks into two lines on a desktop.
- **Headline** (650, 26 to 36 px fluid, 1.12): a section heading. On the docs, the page title is 680 at 30 to 38 px.
- **Title** (650, 20 px, 1.25): a claim in "What changes", a step's name, and the headings of the two lists under the comparison. **Title, small** is 17 px, for the titles of short columns and the wordmark.
- **Body** (400, 17 px, 1.6): running text on the landing page. **UI** (16 px) is the landing page's body on a phone, the docs' body, buttons, and tabs. **Small** (15 px) is for supporting text: limits, table cells, and short columns.
- **Lede** (17 to 19 px, `ink-2`, at most 38rem wide): the paragraph beside a section heading. On the docs the lede is 18 px, and running text is at most 34rem wide, which keeps lines under 80 characters.
- **Label** (400, 13 px, `ink-3`): captions under output and screenshots, table column heads, and a terminal's title bar.
- **Code** (400, 13 px, 1.65): terminals and code blocks. Inline code is 0.84em on `surface-2`.

### Named Rules

**The No Eyebrow Rule.** No chip, kicker, or uppercase label sits above a heading. The heading says it.

**The Left Edge Rule.** Headings and text align left on the grid. Nothing on the landing page is centered.

## Layout

The container is 1160 px at most, with 24 px gutters and 16 px on a phone. Sections stack on one ground. Each opens with a 1 px `rule-strong` rule, 24 to 36 px of space, then its head: the heading in 5 of 12 columns, the lede in the other 7. There are 64 to 112 px under a section, so there's always more space above a heading than under it.

The same 5 and 7 split runs down the page: the steps beside their output, each claim beside its evidence, the questions beside their answers, and the install text beside its terminals. Under 860 px every split becomes one column, in source order.

The landing page's sections:

| Section | What it shows |
| --- | --- |
| Hero | The headline, then the lede and two buttons beside four facts as a definition list (license, where it runs, price, status with a link to the limits), then the top of a plan page as a reviewer sees it. |
| What Writ is | A drawn map: your Git host, you, and your CI job, then Writ on your server, then your cloud. Under it, three plain columns on what goes wrong without a control plane. |
| How a change gets applied | Plan, approve, apply down a line, with each step's real output from `TestTofuApproveApply` beside it, and the refusal of a stale plan as a limit. |
| The control UI | Four screenshots behind text tabs: the overview, a plan, drift, and runs. |
| What changes | Five rows, each a claim, its limit, and a docs link beside its evidence: who holds which credential, a secret shown as `(sensitive)`, a drift table, a policy refusal, and `writ mcp`. Four short notes under them. |
| Compare | Six rows against Spacelift and HCP Terraform, cut from `docs/compare.md`, then what Writ isn't yet and what a reader can check. On a phone each row is a block with one line for each tool. |
| Questions | Six questions. |
| Run it on your machine | What you need, two buttons, the install commands with a copy button, and a cut real run. The page ends here, on the commands. |

Rules the page keeps:

- The copy lives in `lib/landing.ts`. Every terminal output and the limits list come from `content/landing.json`, which `scripts/gen-content.ts` cuts from the README and the docs. A fence that no longer matches fails the build.
- The page says "the plan it kept", "with its secrets hidden", and "where an approval rule applies". "Sealed", "masked plan", "grants", and "risk classes" stay in the docs.
- Each claim names its limit in one sentence at body size. Every fact about another tool has a source and a date, on the comparison pages.
- When `docs/compare.md` changes, change `compare` in `lib/landing.ts` with it.

The docs are Fumadocs' layout: a sidebar tree, the reading column, and a table of contents. A docs page opens with its title and the first paragraph of its source file, set as the lede. The manifest's description goes to the page's metadata, the search index, and `llms.txt`. Each `h2` opens with a rule, as a section does on the landing page.

## Elevation & Depth

Flat. Depth comes from rules and from the two surface tones. One soft shadow (`0 1px 2px` plus `0 18px 40px -18px`) sits under a screenshot frame, because a frame is a window onto another page. Nothing else has a shadow, and nothing is blurred: the header is opaque.

**The Objects Only Rule.** A border with a radius means "this is a thing you could open": a screenshot, a terminal, a node of the map. Text never sits in a card.

## Shapes

Corners are 8 px on frames, terminals, map nodes, and buttons, and 4 px on inline code. Status pills are fully round, as in the control UI. Rules are 1 px. The Writ node of the map has a 1.5 px ink border, the one heavier line on the page.

## Components

### Buttons

- **Shape:** 8 px radius, 46 px tall, 36 px in the header.
- **Primary:** ink fill with ground-colored text. One per view: "Get started" in the hero, "Follow the quick start" at the end.
- **Ghost:** transparent with a `rule-strong` border that turns ink on hover.
- **Focus:** a 2 px accent outline, 3 px off.

### Limit

A sentence at body size in ink, with the coral caret in a 14 px column before it. `components/landing/limit.tsx`. Use it for a limit and for nothing else.

### Frame

A screenshot of the control UI with a one-line address bar in mono that shows `writ.example.com` and the page's path. No window dots. The hero's frame crops the plan page to 1280 by 600. The tour's frames scroll inside.

### Terminal

Dark in both themes, 8 px radius, an optional title bar in sans, and output exactly as `writ` printed it. A caption under it says which run it came from.

### Steps

Three numbered stations down a 2 px line. The number is the sequence, so it stays. A done station is filled green, the active one is ringed in accent. The line fills from station to station and each step's output takes its turn every 3.2 s once the figure is half in view. A click stops it and shows that step's output. The server renders the finished figure and all three outputs, so the page reads without script, and nothing moves under `prefers-reduced-motion`. `components/landing/flow.tsx` is its script, with no animation library.

### Tabs

Text tabs on a rule: the chosen one is ink with a 2 px ink underline. They're radio inputs and CSS, so they switch without script.

### Navigation

A 60 px opaque header: the wordmark, four text links (How it works, Compare, Docs, GitHub), search, the theme switch, and one primary button. Under 860 px the text links go, and the docs carry them.

## Do's and Don'ts

### Do:

- **Do** put a limit beside its claim with the `Limit` component.
- **Do** show a real capture or real output for every claim, with a caption that says where it came from.
- **Do** keep the 5 and 7 split, so headings share one left edge down the page.
- **Do** end the page on the install commands.

### Don't:

- **Don't** put text in a card, or a card in a card.
- **Don't** add a chip, an eyebrow, an icon tile, a fact bar with check marks, a bento grid, or a closing call-to-action band.
- **Don't** center a heading or a lede.
- **Don't** use green, amber, or red for anything that isn't a status, or coral for anything that isn't the mark or a limit.
- **Don't** use mono for anything that isn't code, output, or an address.
- **Don't** draw the product. Shoot it.

## Screenshots

`just ui-shots [ref]` shoots every image of the control UI from the `uishowcase` test of `internal/ui` at a Git ref, by default `HEAD`. It checks the ref out into a throwaway worktree, gives the demo data readable names (alice, bob, the repository `infra`, the stacks `billing` and `dns`), serves the showcase on loopback, and shoots each page with headless Chromium in light and dark, at 1280 and 390 px wide and twice the pixel density. It writes AVIF and WebP to `public/ui/`, and their sizes, paths, and date to `lib/ui-shots.json`, which the captions and frames read.

| Image | Page | Size |
| --- | --- | --- |
| `hero-{1280,390}` | A plan in a reviewer's view, the top 680 and 470 px | 54 KB and 17 KB AVIF, light |
| `overview`, `plan`, `stack`, `runs` at 1280 and 390 | The tour's pages, up to 1,600 px tall | 24 to 84 KB AVIF each |
| `secret` | The replaced resource whose keepers show `(sensitive)` | 20 KB AVIF |
| `drift` | The stack page's drift table | 12 KB AVIF |

Re-shoot after the control UI changes:

1. Merge the UI change, then run `just ui-shots` on this branch, or `just ui-shots <ref>` for another ref.
2. If it stops with "the showcase at REF has no ...", the showcase's names or its reviewer changed. Update `renames` or the reviewer patch in `scripts/ui-shots.ts`.
3. If a page's layout moved, check the crops: `secret` is the first `article.change[data-kind="replace"]` of the plan page, and `drift` is `#drift-changes` of the stack page.
4. Look at the images in `public/ui/`, then build the site and commit both.

At `e0f4d85` the showcase's reviewer server mounts no control UI, so its dev sign-in link answers 404 and the showcase fails. The script mounts one in its worktree until the showcase does.

`scripts/shoot.ts` drives Chromium over the DevTools protocol, so the site takes no browser library. It also takes a JSON list of shots on its own, for screenshots of the site itself.

## Audit, 2026-11-17

`impeccable` 4.9.2, on the static export served on loopback, in Chromium:

| Page | Before | After |
| --- | --- | --- |
| Landing page | 3 warnings (`icon-tile-stack`), 2 advisories | 0 |
| `/docs/quick-start` | 6 warnings (`line-length`), 1 advisory | 0 |
| `/docs/config` | 2 warnings (`line-length`) | 0 |

The page is 8,318 px long at 1280 and 12,284 px at 390, down from 9,303 and 13,687. `scrollWidth` is 390 on a phone. Lighthouse hasn't been run again since the rebuild. The audit of 2026-11-16 is in this file's history.

A scan of the whole `website/` directory also reads the build output in `out/`, and reports there what the served pages don't have: padding it can't see without the stylesheet, the `--` of command-line flags counted as dashes, and the radii and generated font names of the Fumadocs and Next.js bundles. The source files have no finding.
