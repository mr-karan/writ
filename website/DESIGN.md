# Website design

The landing page and the docs share one system, taken from the redesigned control UI: a calm neutral ground, white cards with 1 px rules, quiet sans text, and mono for code. The page and the product then read as one thing, and the screenshots sit in the page without a jump. The landing page was rebuilt on 2026-11-16 from the plan and prototype of that date, which replaced the blueprint direction: a navy field and a condensed heavy headline. Earlier audits of the old page are in this file's history.

## Direction

- **One neutral ground.** Pale grey in light, near black in dark. Bands alternate between the ground and white cards to separate sections, with no rules or numbers.
- **One accent.** Blue, for links, focus, and the active step. The primary button is ink on the ground, not the accent.
- **Status colors only where the product uses them.** Pending is amber, approved and applied are green, refused and failed are red, and in progress is the accent.
- **The mark.** The proofreader's caret in coral on an ink square. The coral is spent nowhere else.
- **Real product, not drawings.** Every product image is a capture of the control UI, with a caption that says when it was made.

## The landing page

A platform engineer who has never heard of Writ should know what it is from the first screen, and reach "how do I try it" in seven sections. Each section is a short heading, one or two sentences, and one visual. Explanations, tables, and reference live in the docs.

| Section | Visual |
| --- | --- |
| Hero | The category chip, a headline that says what Writ does and where, a 28-word subhead, two buttons, the version 0 line, and the top of a plan page as a reviewer sees it, with its plan, approve, apply stepper. Under it, four facts where other vendors put logos. |
| What Writ is | A drawn map: your Git host, you, and your CI job on the left, Writ on your server in the middle, your cloud on the right. Under it, three cards on what goes wrong without a control plane. On a phone the cards become one list. |
| How it works | Plan, approve, apply as three stations on a line, each with its real output from `TestTofuApproveApply`, and the refusal of a stale plan. |
| The control UI | Four screenshots behind tabs: the overview, a plan, drift, and runs. |
| What you get | Five cards of mixed widths, each a claim, its limit, a docs link, and a visual: who holds which credential, a secret shown as `(sensitive)`, a drift table, a policy refusal, and `writ mcp`. A strip under them covers sign-in, imports, and state on S3. |
| Compare | Six rows against Spacelift and HCP Terraform, cut from `docs/compare.md`, with one sentence on what they have that Writ doesn't. Under it, the README's limits and three facts a reader can check. On a phone each row is a block with one line for each tool. |
| Try it | The build commands with a copy button, a cut real run, six questions, and the closing band. |

Rules the page keeps:

- The copy lives in `lib/landing.ts`, so a copy review reads one file. Every terminal output and the limits list come from `content/landing.json`, which `scripts/gen-content.ts` cuts from the README and the docs. A fence that no longer matches fails the build.
- The page says "the plan it kept", "with its secrets hidden", and "where an approval rule applies". "Sealed", "masked plan", "grants", and "risk classes" stay in the docs.
- Each claim names its limit in one sentence at body size. Every fact about another tool has a source and a date, on the comparison pages.
- The full comparison tables against Atlantis, OTF, Terrakube, and the paid control planes live in `docs/compare.md`. The page shows six rows of it for two tools. When `docs/compare.md` changes, change `compare` in `lib/landing.ts` with it.
- The headline says what Writ is. "The approved plan is exactly what applies" is a feature point in the strip under the benefit cards, not the headline.

## Type

| Role | Setting |
| --- | --- |
| Headline | Archivo, normal width, weight 640, 36 to 64 px fluid, line height 1.03, tracking -0.034em |
| Section heading | Weight 620, 28 to 42 px fluid, line height 1.1, tracking -0.022em |
| Card heading | Weight 620, 19 px |
| Lede | 17 to 20 px, line height 1.5, secondary ink, 44rem measure |
| Body | 17 px, 16 px on a phone, line height 1.6 |
| Caption | 13 px, tertiary ink |
| Code and output | Chivo Mono, 13 px, line height 1.65 |

Both faces are self-hosted Latin subsets from pinned `@fontsource` packages. Monospace is used only for code, output, and addresses. The docs use the same faces at normal width.

## Color

The tokens are hex values in `app/global.css`, light first and dark under `.dark`. The Fumadocs `--color-fd-*` variables are aliases of them, so the docs take the same palette. Terminal blocks are dark in both themes and take the dark syntax colors.

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| `bg` | `#f6f7f9` | `#0d0f12` | Page ground |
| `surface` | `#ffffff` | `#15181d` | Cards, frames, bands |
| `surface-2` | `#eef0f3` | `#1b1f25` | Chips, code, tab track, docs code blocks |
| `ink` | `#14171c` | `#eceef2` | Headings, body, primary button |
| `ink-2` | `#454c59` | `#b0b7c3` | Secondary text |
| `ink-3` | `#636b7b` | `#8f97a5` | Captions |
| `rule`, `rule-strong` | `#dde1e7`, `#c3c9d2` | `#272c34`, `#3a414c` | 1 px borders |
| `accent` | `#2f55d4` | `#8fa8ff` | Links, focus, the active step |
| `ok`, `warn`, `bad` | `#17703f`, `#855600`, `#b4232a` | `#6ad496`, `#f0bd5e`, `#ff9090` | Status only |
| `mark` | `#ff8f6b` | `#ff8f6b` | The caret in the wordmark |

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

`ink-3` is a little darker than the prototype's `#697182`, which was 4.3:1 on `surface-2`. The 1 px `rule-strong` borders are about 1.6:1. They frame cards and screenshots, and no control depends on them alone: each button has its text and fill.

## Motion

- One animated figure: the three steps. The line draws from station to station, each number fills, and each status pill changes, looping every 3.2 s once the figure is half in view. A click on a step stops it and shows that step's output.
- It animates `width`, color, and border only. `components/landing/flow.tsx` is its script. No animation library is used.
- The server renders the finished figure and all three outputs. A page without script shows all of it.
- Under `prefers-reduced-motion` nothing moves, and the figure stays finished.

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

## Audit, 2026-11-16

On the static export, in Chromium 157, in both themes, at 1440 by 900 and 390 by 844, with reduced motion. Lighthouse 13.6.0 ran on the export served with gzip, on a machine under load (load average 16 to 29 on 16 cores), so its mobile timings vary between runs.

| Check | Result |
| --- | --- |
| First viewport | At 1440 and at 390 it holds the category chip, the headline, the subhead, both buttons, the version 0 line, and the plan screenshot with its stepper. |
| Length | 8,627 px at 1440 and 11,727 px at 390. Prototype: 8,516 and 11,405. Old page: 13,127 and 18,002. |
| Words | 1,368 in `main`, with eight `h2` headings: seven sections and the questions. Prototype: 1,391. Old page: 3,546. Subhead: 25 words. |
| Phone | `scrollWidth` is 390. The crops and long output lines scroll inside their own frames. |
| Lighthouse, mobile | Performance 94, 93, 94, 88, 87, and 94 in six runs of the final page. The runs under 90 had 260 to 300 ms of blocking time while other work loaded the machine; the others had 50 to 60 ms. Accessibility, Best Practices, and SEO 100 in every run. Simulated LCP 2.7 to 3.2 s, CLS 0. |
| Lighthouse, desktop | 100 in all four categories. LCP 0.7 s. |
| Weight | 196.6 KB of gzipped JavaScript, against 196.1 KB on the old page. 19.2 KB of CSS and 18.4 KB of HTML. No request to another host. |
| Without script | Every section reads. The tour's tabs switch through radio inputs and CSS, all three outputs show, and the copy button is left out. |
| Links | Every `/docs` link and anchor on the built site resolves, checked offline with lychee. |

Left as it is:

- Mobile LCP is the hero screenshot, and Lighthouse's simulated slow network puts it near 3 s. Measured, the image arrives in under 200 ms and paints 128 ms after. A preload of it in the head and skipping the mono font's preload each left the simulated LCP where it was, so neither was kept. The plan's stricter targets, performance 95 and LCP 2.0 s, aren't met.
- A dark-theme reader also downloads the light hero image, which loads eagerly because light is the default theme. That's 54 KB on desktop and 17 KB on a phone.
- The screenshots show the control UI of branch `ui-v2-7`, which must merge before this page ships.
