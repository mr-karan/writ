// Cuts the repository's Markdown into the pages content.manifest.ts lists and
// writes them to content/docs (for Fumadocs) and public/ (plain Markdown and
// llms.txt, for agents). Both outputs are git-ignored: docs/ stays the only
// copy of the text. A selector that no longer matches its source fails the
// build, so the site cannot drift from the docs without someone noticing.

import { mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { dirname, join, posix } from 'node:path';
import { fileRoutes, nav, pages, type NavEntry, type Page, type Part } from '../content.manifest';
import { repoUrl, site } from '../lib/site';

const websiteDir = join(import.meta.dirname, '..');
const repoDir = join(websiteDir, '..');
const contentDir = join(websiteDir, 'content', 'docs');
const rawDir = join(websiteDir, 'public', 'raw');

interface Block {
  text: string;
  /** Heading level, or 0 when the block is not a heading. */
  level: number;
}

const sources = new Map<string, Block[]>();

async function blocksOf(file: string): Promise<Block[]> {
  const cached = sources.get(file);
  if (cached) return cached;
  const blocks = splitBlocks(await readFile(join(repoDir, file), 'utf8'));
  sources.set(file, blocks);
  return blocks;
}

/** Splits Markdown on blank lines, keeping each code fence whole. */
function splitBlocks(markdown: string): Block[] {
  const blocks: Block[] = [];
  let current: string[] = [];
  let fence: string | null = null;
  const flush = () => {
    if (current.length === 0) return;
    const text = current.join('\n');
    const heading = fence === null ? /^(#{1,6}) /.exec(text) : null;
    blocks.push({ text, level: heading && current.length === 1 ? heading[1].length : 0 });
    current = [];
  };
  for (const line of markdown.split('\n')) {
    const marker = /^\s*(`{3,}|~{3,})/.exec(line);
    if (fence !== null) {
      current.push(line);
      if (marker && marker[1].startsWith(fence)) {
        fence = null;
        flush();
      }
      continue;
    }
    if (marker) {
      // A fence directly under a list item stays in that item's block.
      fence = marker[1];
      current.push(line);
      continue;
    }
    if (line.trim() === '') {
      flush();
      continue;
    }
    if (/^#{1,6} /.test(line)) flush();
    current.push(line);
    if (/^#{1,6} /.test(line)) flush();
  }
  flush();
  return blocks;
}

function fail(page: Page, message: string): never {
  throw new Error(`content.manifest.ts, page "${page.slug}": ${message}`);
}

function headingText(block: Block): string {
  return block.text.replace(/^#+ /, '');
}

/** Moves every heading so that the shallowest one in the part sits at `top`. */
function relevel(blocks: Block[], top: number): Block[] {
  const levels = blocks.filter((b) => b.level > 0).map((b) => b.level);
  if (levels.length === 0) return blocks;
  const shift = top - Math.min(...levels);
  return blocks.map((b) =>
    b.level === 0 ? b : { level: b.level + shift, text: `${'#'.repeat(b.level + shift)} ${headingText(b)}` },
  );
}

async function select(page: Page, part: Part): Promise<Block[]> {
  if ('heading' in part) return [{ text: `## ${part.heading}`, level: 2 }];
  const blocks = await blocksOf(part.file);

  if ('whole' in part) {
    return blocks.filter((b) => b.level !== 1);
  }
  if ('intro' in part) {
    const start = blocks.findIndex((b) => b.level === 1) + 1;
    const end = blocks.findIndex((b, i) => i >= start && b.level > 0);
    return blocks.slice(start, end === -1 ? undefined : end);
  }
  if ('section' in part) {
    const at = blocks.findIndex((b) => b.level > 0 && headingText(b) === part.section);
    if (at === -1) fail(page, `${part.file} has no heading "${part.section}"`);
    const level = blocks[at].level;
    const end = blocks.findIndex((b, i) => i > at && b.level > 0 && b.level <= level);
    const body = blocks.slice(at + 1, end === -1 ? undefined : end);
    if (part.keepHeading) return relevel([blocks[at], ...body], 2);
    return relevel(body, 2);
  }
  if ('block' in part) {
    const at = blocks.findIndex((b) => b.text.startsWith(part.block));
    if (at === -1) fail(page, `${part.file} has no block starting with "${part.block}"`);
    return blocks.slice(at, at + (part.count ?? 1));
  }
  if ('table' in part) {
    const table = blocks.find((b) => b.text.startsWith(part.table));
    if (!table) fail(page, `${part.file} has no table starting with "${part.table}"`);
    const [header, rule, ...rows] = table.text.split('\n');
    const kept = part.rows.map((prefix) => {
      const row = rows.find((r) => r.startsWith(prefix));
      if (!row) fail(page, `the table "${part.table}" in ${part.file} has no row starting with "${prefix}"`);
      return row;
    });
    return [{ text: [header, rule, ...kept].join('\n'), level: 0 }];
  }
  for (const block of blocks) {
    const line = block.text.split('\n').find((l) => l.startsWith(`- ${part.item}`));
    if (line) return [{ text: line.slice(2), level: 0 }];
  }
  return fail(page, `${part.file} has no list item starting with "${part.item}"`);
}

/** Names a language for a fence that has none, so the site can highlight it. */
function fenceLanguage(code: string): string {
  const lines = code.split('\n').map((l) => l.trim());
  const first = lines.find((l) => l !== '') ?? '';
  if (lines.some((l) => l.startsWith('$ '))) return 'console';
  if (/^(error|warning)\[|^Plan[: ]|^ok: |^[+>~-] |^\+\/- |^-\/\+ |^ADDR\s/.test(first)) return 'console';
  if (/^(writ|just|mkdir|cp|cd|export|go|tofu|jq) /.test(first)) return 'sh';
  return 'text';
}

function tagFence(block: Block): Block {
  const match = /^(\s*)(`{3,}|~{3,})([^\n]*)\n([\s\S]*)\n\s*\2\s*$/.exec(block.text);
  if (!match) return block;
  const [, indent, fence, info, code] = match;
  if (info.trim() !== '') return block;
  return { ...block, text: `${indent}${fence}${fenceLanguage(code)}\n${code}\n${indent}${fence}` };
}

/** Rewrites a relative link so it lands on the site page, or in the repository. */
function rewriteLinks(page: Page, text: string, sourceFile: string): string {
  return text.replace(/\]\(([^)\s]+)\)/g, (whole, target: string) => {
    if (/^[a-z]+:/.test(target)) return whole;
    const [path, anchor] = target.split('#');
    const file = path === '' ? sourceFile : posix.normalize(posix.join(dirname(sourceFile), path));
    if (file.startsWith('..')) fail(page, `link "${target}" in ${sourceFile} leaves the repository`);
    const keyed = anchor ? fileRoutes[`${file}#${anchor}`] : undefined;
    if (keyed) return `](${keyed})`;
    const route = fileRoutes[file];
    if (route) return `](${route}${anchor ? `#${anchor}` : ''})`;
    if (file === sourceFile) fail(page, `anchor "${target}" in ${sourceFile} has no route in fileRoutes`);
    return `](${repoUrl(file)})`;
  });
}

async function render(page: Page): Promise<{ body: string; files: string[] }> {
  const out: string[] = [];
  const files: string[] = [];
  for (const part of page.parts) {
    const blocks = await select(page, part);
    const file = 'file' in part ? part.file : null;
    if (file && !files.includes(file)) files.push(file);
    for (const block of blocks) {
      const tagged = tagFence(block);
      const isFence = /^\s*(`{3,}|~{3,})/.test(tagged.text);
      out.push(isFence || !file ? tagged.text : rewriteLinks(page, tagged.text, file));
    }
  }
  return { body: out.join('\n\n') + '\n', files };
}

function frontMatter(page: Page, files: string[]): string {
  const quote = (s: string) => JSON.stringify(s);
  return [
    '---',
    `title: ${quote(page.title)}`,
    `description: ${quote(page.description)}`,
    `sources: [${files.map(quote).join(', ')}]`,
    '---',
    '',
  ].join('\n');
}

function metaFor(entries: NavEntry[]): { root: object; folders: Map<string, object> } {
  const folders = new Map<string, object>();
  const list = entries.map((entry) => {
    if (typeof entry === 'string') return entry;
    if ('separator' in entry) return `---${entry.separator}---`;
    folders.set(entry.folder, { title: entry.title, pages: entry.pages });
    return entry.folder;
  });
  return { root: { title: 'Docs', pages: list }, folders };
}

function route(slug: string): string {
  const path = slug.replace(/(^|\/)index$/, '');
  return path === '' ? '/docs' : `/docs/${path}`;
}

async function write(path: string, text: string): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, text);
}

// Both directories hold only what this script writes, so stale pages are
// cleared before each run.
await rm(contentDir, { recursive: true, force: true });
await rm(rawDir, { recursive: true, force: true });

const llms: string[] = [
  `# ${site.name}`,
  '',
  `> ${site.description}`,
  '',
  'Every page of the documentation is also served as plain Markdown at the URLs below.',
  '',
  '## Docs',
  '',
];

for (const page of pages) {
  const { body, files } = await render(page);
  await write(join(contentDir, `${page.slug}.md`), frontMatter(page, files) + '\n' + body);
  const rawPath = `/raw/${page.slug.replace(/\/index$/, '')}.md`;
  await write(join(websiteDir, 'public', rawPath), `# ${page.title}\n\n${body}`);
  llms.push(`- [${page.title}](${rawPath}): ${page.description} (page: ${route(page.slug)})`);
}

const meta = metaFor(nav);
await write(join(contentDir, 'meta.json'), JSON.stringify(meta.root, null, 2) + '\n');
for (const [folder, value] of meta.folders) {
  await write(join(contentDir, folder, 'meta.json'), JSON.stringify(value, null, 2) + '\n');
}
await write(join(websiteDir, 'public', 'llms.txt'), llms.join('\n') + '\n');

// What the landing page shows of the docs. Every list and every terminal
// output on it is cut from the same Markdown as the docs, so the page cannot
// state a gap or show an output that the repository does not.
async function listAt(file: string, start: string): Promise<string[]> {
  const block = (await blocksOf(file)).find((b) => b.text.startsWith(start));
  if (!block) throw new Error(`gen-content: ${file} has no list starting with "${start}"`);
  return block.text.split('\n').map((line) => line.replace(/^- /, ''));
}

/** The body of the code fence that holds a line starting with `line`, without its fence lines and indent. */
async function fenceWith(file: string, line: string): Promise<string> {
  const block = (await blocksOf(file)).find(
    (b) => /^\s*(`{3,}|~{3,})/.test(b.text) && b.text.split('\n').some((l) => l.trimStart().startsWith(line)),
  );
  if (!block) throw new Error(`gen-content: ${file} has no code fence with a line starting with "${line}"`);
  const lines = block.text.split('\n');
  const indent = /^\s*/.exec(lines[0])?.[0].length ?? 0;
  return lines
    .slice(1, -1)
    .map((l) => l.slice(indent))
    .join('\n');
}

/** A list item's bold lead and the first sentence after it, which states the limit. */
function leadAndFirst(item: string): { lead: string; text: string } {
  const m = /^\*\*(.+?)\*\* (.+)$/.exec(item);
  if (!m) throw new Error(`gen-content: the README's list item "${item.slice(0, 40)}" has no bold lead`);
  return { lead: m[1], text: m[2].split(/(?<=\.) +(?=[A-Z`])/)[0] };
}

/**
 * The lines of a transcript from the first that starts with `from` to the
 * first after it that starts with `to`, without the lines that `drop` matches.
 */
function cut(text: string, from: string, to: string, drop: RegExp): string {
  const lines = text.split('\n');
  const start = lines.findIndex((l) => l.startsWith(from));
  const end = lines.findIndex((l, i) => i > start && l.startsWith(to));
  if (start === -1 || end === -1) throw new Error(`gen-content: no lines from "${from}" to "${to}"`);
  return lines
    .slice(start, end + 1)
    .filter((l) => !drop.test(l))
    .join('\n');
}

// The landing page links where the README does.
const landingPage: Page = { slug: 'landing', title: '', description: '', parts: [] };
const landing = {
  limits: (await listAt('README.md', '- **Version 0.**')).map((item) => leadAndFirst(rewriteLinks(landingPage, item, 'README.md'))),
  build: await fenceWith('docs/quick-start.md', 'curl -fsSL '),
  run: cut(
    await fenceWith('docs/quick-start.md', '$ writ plan --remote'),
    '$ writ plan --remote',
    'State version',
    /^(commit plan of|Outputs that change|Plan [0-9a-f]{32} for|It is a commit plan|Run [A-Z0-9]{26} )/,
  ),
  plan: await fenceWith('docs/quick-start.md', 'OpenTofu 1.13.9 plan of app@prod'),
  denied: await fenceWith('docs/quick-start.md', '$ writ approve --remote'),
  approved: await fenceWith('docs/quick-start.md', 'Approved plan '),
  apply: await fenceWith('docs/quick-start.md', 'OpenTofu apply of app@prod'),
  policyDeny: cut(await fenceWith('docs/policy.md', 'Policy: denied'), 'Policy: denied', '  deny:', /(?!)/),
  mcpAdd: await fenceWith('docs/agents.md', '$ claude mcp add'),
};
await write(join(websiteDir, 'content', 'landing.json'), JSON.stringify(landing, null, 2) + '\n');

console.log(`gen-content: wrote ${pages.length} pages from ${sources.size} source files`);
