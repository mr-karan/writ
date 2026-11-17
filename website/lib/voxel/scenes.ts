// The pictures of the landing page. Each is a scene: layers of voxels seen
// through one view, drawn from a fixed seed.

import { A, B, type Family, noise, Rng, type TonePath, type View, type Voxel, World } from './iso';

export interface Layer {
  /** What the layer is, for the class that styles or moves it. */
  name: string;
  paths: TonePath[];
}

/** Where one lone cube stands on the picture: the near corner of its top face. */
export interface Spark {
  x: number;
  y: number;
}

export interface Scene {
  width: number;
  height: number;
  layers: Layer[];
  /** Cubes that are drawn on their own, so they can light up on their own. */
  sparks: Spark[];
}

const vox = (family: Family, level: number): Voxel => ({ family, level });

/** Where a place on the ground falls on a picture, in units of the picture. */
const onScreen = (view: View, x: number, y: number) => ({ x: (x - y - view.u) * A, y: (x + y - view.v) * B });

function sceneOf(view: View, layers: { name: string; world: World }[], sparks: [number, number, number][]): Scene {
  return {
    width: view.cols * A,
    height: view.rows * B,
    layers: layers.map(({ name, world }) => ({ name, paths: world.paths(view) })),
    sparks: sparks.map(([x, y, z]) => ({ x: (x - y - view.u) * A, y: (x + y - 2 * z - view.v) * B })),
  };
}

interface Island {
  cx: number;
  cy: number;
  /** Radius in cubes and height in cubes at the middle. */
  r: number;
  h: number;
  seed: number;
  /** Where this is true the ground is planned and not built: a wire frame. */
  planned?: (x: number, y: number) => boolean;
}

/** An island of moss over bone that steps down to a sand shore. Returns the height of the ground at a place. */
function island(w: World, o: Island): (x: number, y: number) => number {
  const rough = noise(o.seed);
  const patch = noise(o.seed + 7);
  const top = (x: number, y: number) => {
    const edge = Math.hypot(x - o.cx, y - o.cy) / o.r + (rough(x / 8, y / 8) - 0.5) * 0.55;
    return edge >= 1 ? 0 : Math.max(1, Math.min(o.h, Math.ceil((1 - edge) * o.h * 2.2)));
  };
  for (let x = o.cx - o.r - 6; x <= o.cx + o.r + 6; x++)
    for (let y = o.cy - o.r - 6; y <= o.cy + o.r + 6; y++) {
      const t = top(x, y);
      for (let z = 1; z <= t; z++) {
        const ground = z < t ? vox('b', 2 + (z % 2)) : t === 1 ? vox('b', 4) : vox('m', patch(x / 5, y / 5) > 0.55 ? 4 : 3);
        w.set(x, y, z, o.planned?.(x, y) ? { ...ground, wire: true } : ground);
      }
    }
  return top;
}

interface Sea {
  cx: number;
  cy: number;
  /** Whole out to `near` cubes from the middle, and gone by `far`. */
  near: number;
  far: number;
  seed: number;
  /** From 0 to 1: how much more of the sea is gone at a place, for room the page needs. */
  open?: (x: number, y: number) => number;
}

/** The sea: flat cubes of indigo round a place, whole near it and breaking into floes far out. */
function sea(w: World, o: Sea): void {
  const swell = noise(o.seed);
  const floe = noise(o.seed + 3);
  for (let x = o.cx - o.far; x <= o.cx + o.far; x++)
    for (let y = o.cy - o.far; y <= o.cy + o.far; y++) {
      if (w.has(x, y, 1)) continue;
      const out = Math.max((Math.hypot(x - o.cx, y - o.cy) - o.near) / (o.far - o.near), o.open?.(x, y) ?? 0);
      if (out >= 1 || floe(x / 3.2, y / 3.2) < out * 1.15 - 0.1) continue;
      const shore = w.has(x + 1, y, 1) || w.has(x - 1, y, 1) || w.has(x, y + 1, 1) || w.has(x, y - 1, 1);
      w.set(x, y, 0, { family: 'i', level: shore ? 5 : swell(x / 9, y / 9) > 0.52 ? 4 : 3, fade: out > 0.7 ? 2 : out > 0.42 ? 1 : 0 });
    }
}

interface Keep {
  cx: number;
  cy: number;
  /** The height of the ground it stands on. */
  ground: number;
  /** Half the width of the walls, the walls' height, and the tower's height above the ground. */
  half: number;
  wall: number;
  tower: number;
}

/**
 * The keep: four walls with corner towers round a court, and a tall tower
 * in the middle. The ochre is what only the keep holds. It lies in the
 * court, shows through the gate and the tower's windows, and burns on the
 * tower's top. Returns the height of the tower's top.
 */
function keep(w: World, k: Keep): number {
  const { cx, cy, half } = k;
  const g = k.ground;
  const stone = (z: number) => vox('b', z % 3 === 0 ? 4 : 5);
  w.box(cx - half - 1, cy - half - 1, g, cx + half + 1, cy + half + 1, g, vox('b', 3));
  w.box(cx - half + 1, cy - half + 1, g + 1, cx + half - 1, cy + half - 1, g + 1, (x, y) => vox('o', (x * 3 + y * 5) % 4 === 0 ? 3 : 4));

  for (let i = -half; i <= half; i++)
    for (let z = g + 1; z <= g + k.wall + 1; z++) {
      // The top course is the battlement: every other cube.
      if (z === g + k.wall + 1 && (i + half) % 2 === 1) continue;
      for (const [x, y] of [
        [cx + i, cy - half],
        [cx + i, cy + half],
        [cx - half, cy + i],
        [cx + half, cy + i],
      ])
        w.set(x, y, z, stone(z));
    }
  const gate = Math.max(1, Math.round(half / 5));
  w.box(cx - gate, cy + half, g + 1, cx + gate, cy + half, g + Math.min(k.wall - 1, gate + 2), vox('o', 3));

  for (const [sx, sy] of [
    [-1, -1],
    [1, -1],
    [-1, 1],
    [1, 1],
  ]) {
    const x = cx + sx * half;
    const y = cy + sy * half;
    w.box(x - 1, y - 1, g + 1, x + 1, y + 1, g + k.wall + 2, (_x, _y, z) => stone(z + 1));
    for (const [dx, dy] of [
      [-1, -1],
      [1, -1],
      [-1, 1],
      [1, 1],
    ])
      w.set(x + dx, y + dy, g + k.wall + 3, vox('b', 5));
  }

  const core = Math.max(1, Math.round(half * 0.36));
  const top = g + k.tower;
  w.box(cx - core, cy - core, g + 1, cx + core, cy + core, top, (_x, _y, z) => stone(z));
  for (let z = g + 4; z < top - 1; z += 4) {
    w.set(cx + core, cy, z, vox('o', 3));
    w.set(cx + core, cy, z + 1, vox('o', 3));
    if (core < 3) {
      w.set(cx, cy + core, z, vox('o', 3));
      w.set(cx, cy + core, z + 1, vox('o', 3));
    }
  }
  if (core >= 3) {
    // A tower wide enough carries a key on its lit face: a ring, a shaft, and two teeth.
    const ring = top - 3;
    for (const [dx, dz] of [[-1, 0], [0, 0], [1, 0], [-1, -1], [1, -1], [-1, -2], [0, -2], [1, -2], [0, -3], [0, -4], [0, -5], [0, -6], [0, -7], [0, -8], [0, -9], [1, -7], [1, -9]])
      w.set(cx + dx, cy + core, ring + dz, vox('o', 3));
  }
  for (let i = -core; i <= core; i++)
    for (const [x, y] of [
      [cx + i, cy - core],
      [cx + i, cy + core],
      [cx - core, cy + i],
      [cx + core, cy + i],
    ])
      if ((i + core) % 2 === 0) w.set(x, y, top + 1, vox('b', 5));
  w.set(cx, cy, top + 1, vox('o', 4));
  w.set(cx, cy, top + 2, vox('o', 4));
  return top + 2;
}

/** A cloud: a flat heap of pale cubes, two deep in the middle. */
function cloud(w: World, cx: number, cy: number, z: number, r: number, seed: number): void {
  const puff = noise(seed);
  for (let x = cx - r - 4; x <= cx + r + 4; x++)
    for (let y = cy - r - 4; y <= cy + r + 4; y++) {
      // Longer across the picture than it is deep.
      const edge = Math.hypot((x - cx + (y - cy)) * 0.9, (x - cx - (y - cy)) * 0.42) / r + (puff(x / 4, y / 4) - 0.5) * 0.7;
      if (edge >= 1) continue;
      w.set(x, y, z, vox('b', 4));
      if (edge < 0.5) w.set(x, y, z + 1, vox('b', 4));
      if (edge < 0.62) w.set(x, y, z - 1, vox('b', 4));
    }
}

/** An outpost: a hut of indigo with a mast. It holds no ochre. */
function outpost(w: World, x: number, y: number, ground: number): void {
  w.box(x - 1, y - 1, ground + 1, x + 1, y + 1, ground + 2, vox('i', 3));
  w.set(x, y, ground + 3, vox('i', 4));
  w.set(x, y, ground + 4, vox('i', 4));
}

/**
 * The hero: a keep on an island, and the only thing on it that reaches the
 * cloud. Outposts on the shore send to the gate and hold nothing. The
 * ground at the lower left is still a plan, a wire frame, and it turns to
 * built ground as it nears the island.
 */
function heroScene(): Scene {
  const land = new World();
  const sky = new World();
  const view: View = { u: -126, v: -122, cols: 180, rows: 200 };

  const height = island(land, { cx: 0, cy: 0, r: 37, h: 5, seed: 11 });
  const built = noise(77);
  const planned = (x: number, y: number) => built(x / 4, y / 4) < (y - x - 44) / 24;
  island(land, { cx: -20, cy: 46, r: 20, h: 4, seed: 23, planned });
  // A tower that is planned too.
  land.box(-27, 50, 5, -24, 53, 12, { family: 'b', level: 4, wire: true });

  const top = keep(land, { cx: 0, cy: 0, ground: 5, half: 10, wall: 4, tower: 19 });

  // The road from the gate to the shore, and the outposts beside it.
  for (let y = 12; y <= 34; y++) for (const x of [-1, 0, 1]) if (height(x, y) > 1) land.set(x, y, height(x, y), vox('b', 5));
  // A plan on its way up the road, and the sill it must cross: the approval.
  for (const y of [17, 23, 29]) land.set(0, y, height(0, y) + 1, vox('i', 4));
  for (const x of [-1, 0, 1]) land.set(x, 12, height(x, 12), vox('o', 4));
  outpost(land, -8, 27, height(-8, 27));
  outpost(land, 9, 29, height(9, 29));

  // The headline stands at the top left of the picture, so the sea stays out of it.
  const open = (x: number, y: number) => {
    const at = onScreen(view, x, y);
    return Math.max(Math.min(1, Math.max(0, (800 - at.x) / 200) * Math.max(0, (500 - at.y) / 120)), (at.y - 690) / 110);
  };
  sea(land, { cx: 2, cy: 16, near: 44, far: 74, seed: 5, open });

  cloud(sky, 13, -13, 48, 10, 3);
  cloud(sky, 44, -48, 34, 7, 9);
  const sparks: [number, number, number][] = [];
  for (let z = top + 3; z < 46; z += 3) sparks.push([0, 0, z]);

  return sceneOf(
    view,
    [
      { name: 'land', world: land },
      { name: 'sky', world: sky },
    ],
    sparks,
  );
}

/**
 * What Writ is, as a strip of sea: two outposts on the left that hold
 * nothing, the keep in the middle, and the cloud on the right. Stepping
 * stones run from the outposts to the keep. Only the keep sends cubes to
 * the cloud.
 */
function mapScene(): Scene {
  const land = new World();
  const sky = new World();
  const view: View = { u: -74, v: -56, cols: 145, rows: 104 };

  const home = island(land, { cx: 0, cy: 0, r: 15, h: 3, seed: 31 });
  const top = keep(land, { cx: 0, cy: 0, ground: 3, half: 6, wall: 3, tower: 11 });
  for (const [x, y] of [
    [-24, 22],
    [-17, 32],
  ]) {
    const ground = island(land, { cx: x, cy: y, r: 6, h: 2, seed: 40 + x });
    outpost(land, x, y, ground(x, y));
  }
  // Stepping stones across the water, from each outpost toward the gate.
  for (let i = 0; i < 6; i++) {
    land.set(-18 + i * 2, 20 - i * 2, 1, vox('b', 4));
    land.set(-12 + i * 2, 27 - i * 2, 1, vox('b', 4));
  }
  for (let y = 7; y <= 14; y++) if (home(0, y) > 1) land.set(0, y, home(0, y), vox('b', 5));
  sea(land, { cx: -5, cy: 9, near: 20, far: 36, seed: 6 });

  cloud(sky, 26, -26, 15, 8, 4);
  // From the tower's top across to the cloud.
  const sparks: [number, number, number][] = [];
  for (let i = 1; i <= 5; i++) sparks.push([i * 3, -i * 3, top + 1 + (i > 3 ? i - 3 : 0)]);

  return sceneOf(
    view,
    [
      { name: 'land', world: land },
      { name: 'sky', world: sky },
    ],
    sparks,
  );
}

/** The sea the page ends on: islands at the right, one with a small keep, and paper at the left for the name. */
function footScene(): Scene {
  const land = new World();
  const view: View = { u: -150, v: -58, cols: 180, rows: 100 };

  island(land, { cx: 0, cy: 0, r: 20, h: 4, seed: 51 });
  keep(land, { cx: 0, cy: 0, ground: 4, half: 6, wall: 3, tower: 10 });
  island(land, { cx: -26, cy: 22, r: 10, h: 3, seed: 52 });
  island(land, { cx: 24, cy: -28, r: 12, h: 3, seed: 53 });
  island(land, { cx: -30, cy: 34, r: 7, h: 2, seed: 54, planned: () => true });
  const open = (x: number, y: number) => {
    const at = onScreen(view, x, y);
    return Math.max((800 - at.x) / 220, (at.y - 310) / 80, (70 - at.y) / 70);
  };
  sea(land, { cx: -6, cy: 6, near: 30, far: 74, seed: 8, open });

  return sceneOf(view, [{ name: 'land', world: land }], []);
}

/** The pictures of the page, each drawn once for a build. */
export const pictures = { hero: heroScene(), map: mapScene(), foot: footScene() };

/** How a path of one tone is painted, as a style that reads the page's tokens. */
function paint(tone: string): string {
  const [base, fade] = tone.split(' ');
  const opacity = fade === 'vx-f1' ? ';opacity:.6' : fade === 'vx-f2' ? ';opacity:.3' : '';
  if (base === 'vx-wf') return 'fill:var(--color-bg);stroke:var(--color-bg)';
  if (base === 'vx-wl') return 'fill:none;stroke:var(--vx-i5);stroke-width:.8';
  const color = `var(--${base})`;
  return `fill:${color};stroke:${color}${opacity}`;
}

/**
 * A picture as an SVG file with one group for each layer. It carries no
 * colors of its own: a page that draws a layer with <use> hands down its
 * tokens, so the picture follows the page's theme.
 */
export function pictureSvg(scene: Scene): string {
  const layers = scene.layers.map((layer) => `<g id="${layer.name}">${layer.paths.map((p) => `<path style="${paint(p.tone)}" d="${p.d}"/>`).join('')}</g>`);
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${scene.width} ${scene.height}">${layers.join('')}</svg>`;
}

/**
 * The rule that opens a section: a course of flat slabs in one family that
 * starts whole at the left and thins out to nothing.
 */
export function ruleScene(family: Family, seed: number): { width: number; height: number; paths: TonePath[] } {
  const rng = new Rng(seed);
  const byTone = new Map<string, string[]>();
  let x = 0;
  while (x < 760) {
    const w = rng.int(3, 14) * 4;
    const out = Math.max(0, (x - 90) / 600);
    if (!rng.chance(out ** 1.3)) {
      const tone = `vx-${family}${rng.int(2, 4)}${out > 0.6 ? ' vx-f2' : out > 0.3 ? ' vx-f1' : ''}`;
      byTone.set(tone, [...(byTone.get(tone) ?? []), `M${x} 0h${w}v5h${-w}z`]);
    }
    x += w;
  }
  return { width: 1160, height: 5, paths: [...byTone].map(([tone, d]) => ({ tone, d: d.join('') })) };
}
