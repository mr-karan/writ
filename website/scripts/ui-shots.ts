// Shoots the control UI for the landing page, from the showcase test of
// internal/ui at a Git ref (default HEAD), and writes AVIF and WebP files
// to public/ui/, and their sizes, paths, and date to lib/ui-shots.json.
//
// It checks the ref out into a throwaway worktree, gives the showcase's
// demo data names that read well (people alice and bob, repository infra,
// stacks billing and dns), builds the showcase test, serves it on
// loopback, and shoots each page in light and dark at 1280 and 390 CSS
// pixels wide. Run it as `just ui-shots [ref]`. It needs chromium,
// avifenc, cwebp, git, and go on PATH; the recipe brings the first three.

import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createWriteStream, existsSync } from 'node:fs';
import { copyFile, mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { shoot, type Shot } from './shoot';

const websiteDir = join(import.meta.dirname, '..');
const repoDir = join(websiteDir, '..');
const outDir = join(websiteDir, 'public', 'ui');
const ref = process.argv[2] ?? 'HEAD';

// What each landing image shows: a page of the showcase, shot at each
// width down to at most a height, or for a crop, the element it is cut to.
const tall = { 1280: 1600, 390: 1600 };
const images: { name: string; page: string; heights: Record<number, number>; clip?: string }[] = [
  { name: 'overview', page: 'overview', heights: tall },
  { name: 'plan', page: 'plan-others', heights: tall },
  { name: 'resources', page: 'stack-many', heights: tall },
  { name: 'stack', page: 'stack-drift', heights: tall },
  { name: 'runs', page: 'runs', heights: tall },
  { name: 'secret', page: 'plan-others', heights: { 1280: 0 }, clip: 'details.change[data-kind="replace"]' },
  { name: 'drift', page: 'stack-drift', heights: { 1280: 0 }, clip: '#drift-changes' },
];

// Each pair must match the showcase at least once, so a change there
// fails here instead of shooting the old names.
const renames: [string, string][] = [
  ['DevUser: "tester"', 'DevUser: "alice"'],
  ['DevUser: "reviewer"', 'DevUser: "bob"'],
  ['"slow"', '"billing"'],
  ['"queue"', '"dns"'],
  ['"demo"', '"infra"'],
  ['/ui/repos/demo', '/ui/repos/infra'],
  ['Customer A', 'Alice'],
];

// At e0f4d85 the reviewer's server has no control UI, so its dev sign-in
// link answers 404 and the showcase fails. Mount one when it lacks it.
const reviewerMade = 'DevUser: "bob", Version: "showcase"})\n\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n';
const reviewerUI = '\tif ru, err := ui.New(ui.Options{Server: reviewer}); err != nil {\n\t\tt.Fatal(err)\n\t} else {\n\t\tru.Mount()\n\t}\n';

function run(cmd: string, args: string[], cwd: string, env: NodeJS.ProcessEnv = process.env): string {
  const r = spawnSync(cmd, args, { cwd, env, encoding: 'utf8' });
  if (r.status !== 0) throw new Error(`${cmd} ${args.join(' ')}: exit ${r.status}\n${r.stdout}${r.stderr}`);
  return r.stdout.trim();
}

async function seed(src: string): Promise<void> {
  const file = join(src, 'internal', 'ui', 'showcase_test.go');
  if (!existsSync(file)) throw new Error(`ui-shots: ${ref} has no internal/ui/showcase_test.go`);
  let text = await readFile(file, 'utf8');
  for (const [from, to] of renames) {
    if (!text.includes(from)) throw new Error(`ui-shots: the showcase at ${ref} has no ${from}; update the renames in scripts/ui-shots.ts`);
    text = text.replaceAll(from, to);
  }
  if (!text.includes('ui.Options{Server: reviewer}')) {
    if (!text.includes(reviewerMade)) throw new Error('ui-shots: the showcase makes its reviewer differently; update scripts/ui-shots.ts');
    text = text.replace(reviewerMade, reviewerMade + reviewerUI);
  }
  await writeFile(file, text);
}

async function waitFor(path: string, test: ChildProcess, log: string): Promise<void> {
  for (let i = 0; i < 600; i++) {
    if (existsSync(path)) return;
    if (test.exitCode !== null) throw new Error(`ui-shots: the showcase exited with ${test.exitCode}:\n${await readFile(log, 'utf8')}`);
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error('ui-shots: the showcase wrote no pages.json in 2 minutes');
}

const work = await mkdtemp(join(process.env.TMPDIR ?? tmpdir(), 'writ-ui-shots-'));
const src = join(work, 'src');
const served = join(work, 'served');
const pngs = join(work, 'png');
let test: ChildProcess | undefined;
try {
  const commit = run('git', ['rev-parse', '--short', `${ref}^{commit}`], repoDir);
  run('git', ['worktree', 'add', '--quiet', '--detach', src, commit], repoDir);
  await seed(src);
  run('go', ['test', '-c', '-tags', 'uishowcase', '-o', join(work, 'showcase.test'), './internal/ui'], src);

  await mkdir(served);
  await mkdir(pngs);
  const log = join(work, 'showcase.log');
  const logFile = createWriteStream(log);
  test = spawn(join(work, 'showcase.test'), ['-test.run', '^TestShowcase$'], {
    cwd: join(src, 'internal', 'ui'),
    env: { ...process.env, WRIT_SHOWCASE_OUT: served, WRIT_SHOWCASE_HOLD: '10m' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  test.stdout?.on('data', (b) => logFile.write(b));
  test.stderr?.on('data', (b) => logFile.write(b));
  await waitFor(join(served, 'pages.json'), test, log);

  const [site] = JSON.parse(await readFile(join(served, 'pages.json'), 'utf8')) as { base: string; login: string; pages: Record<string, string> }[];
  const shots: Shot[] = [{ url: site.login, width: 1280, height: 800 }];
  const made: string[] = [];
  const paths: Record<string, string> = {};
  for (const image of images) {
    paths[image.name] = site.pages[image.page];
    for (const [w, maxHeight] of Object.entries(image.heights)) {
      const width = Number(w);
      for (const scheme of ['light', 'dark'] as const) {
        const name = image.clip ? `${image.name}-${scheme}` : `${image.name}-${width}-${scheme}`;
        made.push(name);
        shots.push({
          url: site.base + site.pages[image.page],
          out: join(pngs, `${name}.png`),
          width,
          height: width === 390 ? 844 : 800,
          dpr: 2,
          scheme,
          full: !image.clip,
          maxHeight: maxHeight || undefined,
          clip: image.clip,
        });
      }
    }
  }
  await shoot(shots);

  // Encode every image in the scratch directory first, so a failed
  // encoder leaves website/public/ui as it was. Then replace only the files
  // this script makes; anything else in website/public/ui stays.
  const encoded = join(work, 'encoded');
  await mkdir(encoded, { recursive: true });
  const sizes: Record<string, { width: number; height: number }> = {};
  for (const name of made) {
    const png = join(pngs, `${name}.png`);
    run('avifenc', ['--speed', '4', '-q', '50', '--jobs', 'all', png, join(encoded, `${name}.avif`)], work);
    run('cwebp', ['-quiet', '-q', '78', '-m', '6', png, '-o', join(encoded, `${name}.webp`)], work);
    const head = (await readFile(png)).subarray(16, 24);
    sizes[name] = { width: Math.round(head.readUInt32BE(0) / 2), height: Math.round(head.readUInt32BE(4) / 2) };
    const kb = (f: string) => stat(join(encoded, f)).then((s) => Math.round(s.size / 1024));
    console.log(`ui-shots: ${name} ${sizes[name].width}x${sizes[name].height}, avif ${await kb(`${name}.avif`)} KB, webp ${await kb(`${name}.webp`)} KB`);
  }
  await mkdir(outDir, { recursive: true });
  for (const name of made) {
    for (const ext of ['avif', 'webp']) {
      await copyFile(join(encoded, `${name}.${ext}`), join(outDir, `${name}.${ext}`));
    }
  }
  const date = new Date().toISOString().slice(0, 10);
  await writeFile(join(websiteDir, 'lib', 'ui-shots.json'), JSON.stringify({ date, commit, paths, sizes }, null, 2) + '\n');
  console.log(`ui-shots: ${made.length} images of the control UI at ${commit}, ${date}`);
} finally {
  if (test && test.exitCode === null) {
    await writeFile(join(served, 'stop'), '');
    await new Promise((r) => setTimeout(r, 500));
    test.kill();
  }
  spawnSync('git', ['worktree', 'remove', '--force', src], { cwd: repoDir });
  await rm(work, { recursive: true, force: true });
}
