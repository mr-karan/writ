---
name: Writ website
description: The landing page and docs of Writ, set like a technical record.
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
  # The voxels of the pictures: indigo, bone, moss, ochre, each at its middle level.
  vx-indigo: "#2f3a7c"
  vx-bone: "#d8d1bf"
  vx-moss: "#7b8d52"
  vx-ochre: "#ecb233"
  # The dark theme, under .dark in app/global.css.
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
  # Syntax colors of code blocks: light, then the dark set that terminals also take.
  syn-comment: "#5a6273"
  syn-punct: "#4b5260"
  syn-type: "#0b6676"
  syn-function: "#7a35a8"
  syn-dark-punct: "#adafbe"
  syn-dark-string: "#7fd6a0"
  syn-dark-number: "#f2c26b"
  syn-dark-type: "#7fd1de"
  syn-dark-function: "#d0a7ff"
  syn-dark-remove: "#ff9c9c"
typography:
  display:
    fontFamily: "Newsreader, Iowan Old Style, Palatino, Georgia, serif"
    fontSize: "clamp(2.625rem, 1.3rem + 4.7vw, 4.75rem)"
    fontWeight: 500
    lineHeight: 1.02
    letterSpacing: "-0.028em"
  headline:
    fontFamily: "Newsreader, Iowan Old Style, Palatino, Georgia, serif"
    fontSize: "clamp(1.875rem, 1.3rem + 2vw, 2.75rem)"
    fontWeight: 500
    lineHeight: 1.08
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
    backgroundColor: "{colors.btn}"
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

**Creative North Star: "The keep"**

A writ is a written order, and Writ is the one server that holds the cloud keys. The landing page draws that as a place: an island of small cubes, seen from above, with a stone keep on it. The ochre is what only the keep holds. It lies in the keep's court, makes the key on its tower, and climbs in single cubes from the tower to the cloud. Huts on the shore send a plan up the road to the gate and hold no ochre. At the lower left the land is a wire frame that fills in toward the island: a plan, not yet built.

Under the pictures the page is still a technical record: paper, ink, one left-aligned grid, real output, and real screenshots. The pictures carry the idea, and the text carries the claims.

The landing page was redrawn on 2026-11-17. The owner asked for voxel art after a set of reference images, and then asked for more distance from them. So the palette is indigo, bone, moss, and ochre, and the pictures are isometric voxels from the site's own generator. Nothing is traced or taken from the references.

**Key Characteristics:**

- Three pictures: the hero, the strip in "What Writ is", and the sea the page ends on. Each is drawn by code from a fixed seed.
- Left-aligned on one 12-column rhythm: a section's heading takes 5 columns and its lede takes 7.
- A rule of slabs opens each section. Hairline rules divide rows inside one.
- Real product only: captures of the control UI and output from real runs, each with a caption that says where it came from.
- A limit sits beside its claim at body size, marked with the caret of the wordmark.
- Calm in both themes. The only motion is the cubes that climb to the cloud, a slow drift of the cloud, and the three-step figure.

## Colors

A paper ground with indigo-black ink, indigo for links and the primary button, the product's own status colors, and ochre for what the keep holds.

### Primary

- **Ink** (`ink`): headings and body.
- **Indigo** (`accent`, `btn`): links, focus rings, the active step of the three-step figure, and the primary button.

### Secondary

- **Ochre** (`gild`, `mark`, and `vx-o*`): the second sentence of the headline, the mark, the caret before each limit, the rule over the keep's column in the map, and in the pictures only what the keep holds. Nowhere else.

### Neutral

- **Ground** (`bg`): the whole page, in every section.
- **Surface** (`surface`) and **Surface 2** (`surface-2`): inside frames, inline code, and the frame's address bar.
- **Ink 2** and **Ink 3**: secondary text and captions.
- **Rule** and **Rule Strong**: 1 px dividers.
- **Ok, Warn, Bad** with their soft grounds: status only, as the product uses them.

### The pictures

Four families of six levels each, `--vx-i1` to `--vx-o6` in `app/global.css`: indigo for the sea and the huts, bone for stone, sand, and cloud, moss for the land, and ochre. A cube's top face is one level paler than its left face, and its right face one level darker. The dark theme has its own values: the same sea by night.

### Named Rules

**The One Ground Rule.** Every section sits on `bg`. Sections are separated by a rule and space, never by a band of another color.

**The Caret Rule.** The ochre caret means "here is a limit". It marks every limit sentence and every item of "What Writ isn't, yet", and nothing else.

**The Ochre Rule.** In a picture, ochre is only on or from the keep. A hut, a plan on the road, and the planned land have none.

**The Status Rule.** Green, amber, and red appear only where the product would show that status. A check mark beside a fact isn't a status, so facts get no icon.

Contrast, computed from the tokens with the WCAG formula on 2026-11-17. Every text pair is at least 4.5:1 in both themes:

| Pair | Light | Dark |
| --- | --- | --- |
| `ink` on `bg` | 15.1 | 15.6 |
| `ink-2` on `surface` | 8.8 | 8.6 |
| `ink-3` on `surface-2`, the lowest text pair | 5.1 | 5.2 |
| `accent` on `accent-soft` | 7.4 | 6.9 |
| `ok` on `ok-soft`, `warn` on `warn-soft`, `bad` on `bad-soft` | 5.5, 5.5, 5.2 | 8.1, 9.0, 7.7 |
| `gild` on `bg`, the headline's second sentence and the caret | 4.6 | 9.7 |
| The primary button's text on `btn` | 12.9 | 14.9 |
| Terminal dim text on the terminal | 6.8 | 7.8 |
| The docs' lowest syntax color on `surface-2` | 5.0 | 6.3 |

The 1 px rules are about 1.4:1, and no control depends on one alone. The pictures are decoration beside text that says the same thing, and the hero's picture has a text description.

## Typography

**Display Font:** Newsreader at weight 500, for the landing page's `h1` and `h2` (with Iowan Old Style, Palatino, Georgia)
**Body Font:** Archivo, variable, at its normal width (with ui-sans-serif, system-ui)
**Code Font:** Chivo Mono, variable (with ui-monospace, Menlo, Consolas)

**Character:** a text serif for the headings, a grotesque for everything else, and the name set heavy and tight: Archivo at 800 with -0.045em tracking. Mono is for code, output, and addresses only.

Every face is a self-hosted Latin subset from a pinned `@fontsource` package. Newsreader is one static weight, 24 KB, because the variable file is 58 KB and cost the phone score.

### Hierarchy

- **Display** (Newsreader 500, 42 to 76 px fluid, 1.02): the landing headline only, at most 10.8em wide. Its second sentence is a line of its own in `gild`.
- **Headline** (Newsreader 500, 30 to 44 px fluid, 1.08): a section heading. On the docs, the page title is Archivo 680 at 30 to 38 px.
- **Title** (650, 20 px, 1.25): a claim in "What changes", a step's name, and the headings of the two lists under the comparison. **Title, small** is 17 px, for the titles of short columns.
- **Body** (400, 17 px, 1.6): running text on the landing page. **UI** (16 px) is the landing page's body on a phone, the docs' body, buttons, and tabs. **Small** (15 px) is for supporting text: limits, table cells, and short columns.
- **Lede** (17 to 19 px, `ink-2`, at most 38rem wide): the paragraph beside a section heading. On the docs the lede is 18 px, and running text is at most 34rem wide, which keeps lines under 80 characters.
- **Label** (400, 13 px, `ink-3`): captions under output and screenshots, table column heads, the labels of the hero's facts, and a terminal's title bar.
- **Code** (400, 13 px, 1.65): terminals and code blocks. Inline code is 0.84em on `surface-2`.

### Named Rules

**The No Eyebrow Rule.** No chip, kicker, or uppercase label sits above a heading. The heading says it.

**The Left Edge Rule.** Headings and text align left on the grid. Nothing on the landing page is centered. The pictures sit to the right of the text or run the full width under it.

## Layout

The container is 1160 px at most, with 24 px gutters and 16 px on a phone. Sections stack on one ground. Each opens with a rule of slabs, 26 to 40 px of space, then its head: the heading in 5 of 12 columns, the lede in the other 7. There are 72 to 136 px under a section, so there's always more space above a heading than under it.

The same 5 and 7 split runs down the page: the steps beside their output, each claim beside its evidence, the questions beside their answers, and the install text beside its terminals. Under 860 px every split becomes one column, in source order.

The landing page's sections:

| Section | What it shows |
| --- | --- |
| Hero | The headline, the lede, and two buttons at the top left. The picture of the keep runs the full width under them, and on a wide page the keep rises to the right of the text. Four facts close the hero under it (license, where it runs, price, status with a link to the limits). Under 980 px the picture moves under the text and shows the keep. |
| What Writ is | A strip of sea: two huts on the left, the keep in the middle, the cloud on the right, with ochre cubes going from the tower to the cloud. Under it the three parties in three columns, in the same order. Then three plain columns on what goes wrong without a control plane. |
| How a change gets applied | Plan, approve, apply down a line, with each step's real output from `TestTofuApproveApply` beside it, and the refusal of a stale plan as a limit. |
| The control UI | Four screenshots behind text tabs: the overview, a plan, drift, and runs. |
| What changes | Five rows, each a claim, its limit, and a docs link beside its evidence: who holds which credential, a secret shown as `(sensitive)`, a drift table, a policy refusal, and `writ mcp`. Four short notes under them. |
| Compare | Six rows against Spacelift and HCP Terraform, cut from `docs/compare.md`, then what Writ isn't yet and what a reader can check. On a phone each row is a block with one line for each tool. |
| Questions | Six questions. |
| Run it on your machine | What you need, two buttons, the install commands with a copy button, and a cut real run. The commands are the last thing to read. |
| Footer | The name, set large, with the keep's island seen from farther off, then the license line and the links. |

Rules the page keeps:

- The headline doesn't name OpenTofu, at the owner's request. The lede under it does, in its first sentence, and so does the page's title.
- The copy lives in `lib/landing.ts`. Every terminal output and the limits list come from `content/landing.json`, which `scripts/gen-content.ts` cuts from the README and the docs. A fence that no longer matches fails the build.
- The page says "the plan it kept", "with its secrets hidden", and "where an approval rule applies". "Sealed", "masked plan", "grants", and "risk classes" stay in the docs.
- Each claim names its limit in one sentence at body size. Every fact about another tool has a source and a date, on the comparison pages.
- When `docs/compare.md` changes, change `compare` in `lib/landing.ts` with it.

The docs are Fumadocs' layout: a sidebar tree, the reading column, and a table of contents. A docs page opens with its title and the first paragraph of its source file, set as the lede. The manifest's description goes to the page's metadata, the search index, and `llms.txt`. Each `h2` opens with a rule, as a section does on the landing page.

## Elevation & Depth

Flat, outside the pictures. Depth comes from rules and from the two surface tones. One soft shadow (`0 1px 2px` plus `0 18px 40px -18px`) sits under a screenshot frame, because a frame is a window onto another page. Nothing else has a shadow, and nothing is blurred: the header is opaque.

**The Objects Only Rule.** A border with a radius means "this is a thing you could open": a screenshot or a terminal. Text never sits in a card.

## Shapes

Corners are 8 px on frames, terminals, and buttons, and 4 px on inline code. Status pills are fully round, as in the control UI. Rules are 1 px. The keep's column of the map opens with a 2 px ochre rule, the one heavier line on the page.

## Components

### Buttons

- **Shape:** 8 px radius, 46 px tall, 36 px in the header.
- **Primary:** deep indigo fill with paper-colored text, and bone with dark text in the dark theme. One per view: "Get started" in the hero, "Follow the quick start" at the end.
- **Ghost:** transparent with a `rule-strong` border that turns ink on hover.
- **Focus:** a 2 px accent outline, 3 px off.

### Limit

A sentence at body size in ink, with the ochre caret in a 14 px column before it. `components/landing/limit.tsx`. Use it for a limit and for nothing else.

### Pictures

`lib/voxel/iso.ts` is the generator. A scene puts cubes into a `World`. The world decides what hides what for each triangle of the isometric lattice, merges the faces of one tone in one plane into large polygons, and writes one path for each tone. A planned cube is a wire frame: faces of the ground's color and the lines between them. `lib/voxel/scenes.ts` has the three pictures and the section rule.

Each picture is a static file, `/voxel/hero.svg`, `/voxel/map.svg`, and `/voxel/foot.svg`, written at build time by `app/voxel/[name]/route.ts`: 17, 6, and 7 KB gzipped. Its paths have no colors of their own. They read the `--vx-*` tokens, and `components/landing/voxel.tsx` draws each layer with `<use>`, which hands the page's tokens down. So a picture follows the theme with no script, and the document doesn't carry it: inline, the three pictures made the page's HTML 88 KB gzipped, against 23 KB now.

To change a picture, change its scene and look at it: `bun run build`, then serve `out/`. To add ochre, read the Ochre Rule first.

### Frame

A screenshot of the control UI with a one-line address bar in mono that shows `writ.example.com` and the page's path. No window dots. The tour's frames scroll inside.

### Terminal

Indigo-black in both themes, 8 px radius, an optional title bar in sans, and output exactly as `writ` printed it. A caption under it says which run it came from.

### Steps

Three numbered stations down a 2 px line. The number is the sequence, so it stays. A done station is filled green, the active one is ringed in accent. The line fills from station to station and each step's output takes its turn every 3.2 s once the figure is half in view. A click stops it and shows that step's output. The server renders the finished figure and all three outputs, so the page reads without script, and nothing moves under `prefers-reduced-motion`. `components/landing/flow.tsx` is its script, with no animation library.

### Tabs

Text tabs on a rule: the chosen one is ink with a 2 px ink underline. They're radio inputs and CSS, so they switch without script.

### Navigation

A 60 px opaque header: the wordmark with its caret laid in slabs, four text links (How it works, Compare, Docs, GitHub), search, the theme switch, and one primary button. Under 860 px the text links go, and the docs carry them.

## Do's and Don'ts

### Do:

- **Do** put a limit beside its claim with the `Limit` component.
- **Do** show a real capture or real output for every claim, with a caption that says where it came from.
- **Do** keep the 5 and 7 split, so headings share one left edge down the page.
- **Do** end the page's reading on the install commands.
- **Do** keep ochre for what the keep holds.

### Don't:

- **Don't** put text in a card, or a card in a card.
- **Don't** add a chip, an eyebrow, an icon tile, a fact bar with check marks, a bento grid, or a closing call-to-action band.
- **Don't** center a heading or a lede.
- **Don't** use green, amber, or red for anything that isn't a status.
- **Don't** bring in a picture that isn't drawn by the generator: no stock image, no rendered voxel art.
- **Don't** use mono for anything that isn't code, output, or an address.
- **Don't** draw the product. Shoot it. The pictures draw the idea, never the control UI.

## Screenshots

`just ui-shots [ref]` shoots every image of the control UI from the `uishowcase` test of `internal/ui` at a Git ref, by default `HEAD`. It checks the ref out into a throwaway worktree, gives the demo data readable names (alice, bob, the repository `infra`, the stacks `billing` and `dns`), serves the showcase on loopback, and shoots each page with headless Chromium in light and dark, at 1280 and 390 px wide and twice the pixel density. It writes AVIF and WebP to `public/ui/`, and their sizes, paths, and date to `lib/ui-shots.json`, which the captions and frames read.

| Image | Page | Size |
| --- | --- | --- |
| `hero-{1280,390}` | A plan in a reviewer's view, the top 680 and 470 px. The page no longer shows it: the hero has the picture of the keep | 54 KB and 17 KB AVIF, light |
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

Lighthouse 12.8.2 on the static export, served gzipped on loopback, in Chromium, with its default throttling:

| | Before the redraw | After |
| --- | --- | --- |
| Performance, phone | 96 | 92 and 93, over two runs |
| Performance, desktop | not run | 100 |
| Accessibility | not run | 100 |
| Largest contentful paint, phone | 2.7 s | 3.3 s |
| Layout shift | 0 | 0 |
| Page weight | 469 KiB | 511 KiB |
| The page's HTML, gzipped | 20 KB | 23 KB |

The largest paint is the hero's lede in both. The redraw adds one font file of 24 KB and the hero's picture of 17 KB. `scrollWidth` is 390 on a phone.

`impeccable` hasn't been run since the redraw. Its last run, on the page before this one, is in this file's history.
