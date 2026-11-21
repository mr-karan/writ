# Writ website

The landing page and the docs site. It is a static export of a Next.js app built on [Fumadocs](https://github.com/fuma-nama/fumadocs): the build writes plain files to `out/`, and any web server can host them.

## Run and build

Bun and Node come from nixpkgs. From `website/`:

```
nix shell nixpkgs#bun nixpkgs#nodejs
bun install --frozen-lockfile
bun run dev        # http://localhost:3000, rebuilds the pages from docs/ first
bun run build      # writes the static site to out/
bun run typecheck
```

To look at the built site, serve `out/` with any static server, for example `python3 -m http.server --directory out 4318`.

Rerun `bun run content` after you edit a file under `docs/` while `bun run dev` is running.

## Where the text comes from

The site holds no copy of the docs. `docs/` and `README.md` stay the source.

- `content.manifest.ts` says which file goes on which page, the page titles, and the navigation order. Each page is one whole file today. The manifest can also join parts of several files by selector.
- `scripts/gen-content.ts` runs before `dev` and `build`. It writes the pages into `content/docs/`, writes each one again as plain Markdown under `public/raw/` with an index in `public/llms.txt`, and writes `content/landing.json`: the list of current limits and every terminal output and code block that the landing page shows, cut from `README.md`, `docs/quick-start.md`, `docs/agents.md`, `docs/write-a-policy.md`, `docs/api.md`, and `docs/aws-credentials.md`. All of that is git-ignored.
- A selector that no longer matches its source fails the build. When that happens, fix the selector in `content.manifest.ts` or in `scripts/gen-content.ts`.

To add a docs page, add an entry to `pages` and to `nav` in `content.manifest.ts`. If other docs link to the new file, add it to `fileRoutes`.

## Dependencies

Every version is exact. `bunfig.toml` makes Bun save exact versions, and `bun.lock` pins the transitive ones. Until 2026-11-16 each version was also at least two weeks old when it was added.

| Package | Why it is here |
| --- | --- |
| `next`, `react`, `react-dom` | The framework Fumadocs runs on, used with `output: 'export'`. |
| `fumadocs-core` | The page tree, static search, and Shiki highlighting at build time. |
| `fumadocs-ui` | The docs layout, sidebar, table of contents, search dialog, and theme switch. |
| `fumadocs-mdx` | Compiles the generated Markdown pages when the site builds. |
| `tailwindcss`, `@tailwindcss/postcss` | Fumadocs UI is styled with Tailwind v4. |
| `@fontsource-variable/archivo` | The text and display face. One variable file with a width axis, served by the site. |
| `@fontsource-variable/chivo-mono` | The code face, served by the site. |
| `typescript`, `@types/*` | Type checking. |

The site makes no request to another host and sets no cookie. The theme choice is kept in `localStorage`.

## The landing page

`app/page.tsx` puts the sections together. Each is a server component in `components/landing/`, and their copy is in `lib/landing.ts`. Two small client components run in the browser: `flow.tsx`, which plays the three steps, and `copy-button.tsx`. The page shows everything without them, and its tabs switch with CSS.

Every terminal output and the list of what Writ is not yet come from `content/landing.json`, which `scripts/gen-content.ts` cuts from the README and the docs. Do not type an output into the page. A fence that no longer matches fails the build.

The screenshots in `public/ui/` are real captures of the control UI. After the UI changes, shoot them again with `just ui-shots` from the repository root. `DESIGN.md` says how.

## Design

`PRODUCT.md` records who the site is for and what it must keep true. `DESIGN.md` records the visual system: the tokens, the type, the layout, each component, and the rules a change has to keep. Both follow the formats of [Impeccable](https://github.com/pbakaus/impeccable), whose detector found nothing on the built pages on 2026-11-17: `npx impeccable detect http://127.0.0.1:4318/` against a served `out/`.
