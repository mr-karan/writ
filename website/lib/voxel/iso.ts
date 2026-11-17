// A world of voxels, drawn in isometric projection as SVG paths. A scene
// puts cubes into a World, and the World works out which faces can be
// seen and writes one path for each tone. Everything is placed from a
// seeded generator, so a build draws the same picture every time.
//
// The viewer looks down the diagonal from +x, +y, +z. A cube shows its
// top, its +y face on the left, and its +x face on the right. In that
// view every face is two triangles of one triangular lattice, so what
// hides what is decided for each triangle of the lattice: the nearest
// face wins it.

/** Half the width of a cube on screen, and a quarter of its height. */
export const A = 8;
export const B = 4;

/** A family of the palette: indigo, bone, moss, ochre. */
export type Family = 'i' | 'b' | 'm' | 'o';

export interface Voxel {
  family: Family;
  /** From 2, dark, to 5, pale. The top face is drawn a level paler and the right face a level darker. */
  level: number;
  /** Drawn as a wire frame: planned, and not built. */
  wire?: boolean;
  /** Steps of fading into the ground: 0, 1, or 2. */
  fade?: number;
}

export class Rng {
  private s: number;
  constructor(seed: number) {
    this.s = seed >>> 0;
  }
  /** mulberry32 */
  next(): number {
    this.s = (this.s + 0x6d2b79f5) >>> 0;
    let t = this.s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  }
  /** A whole number from a to b, both included. */
  int(a: number, b: number): number {
    return a + Math.floor(this.next() * (b - a + 1));
  }
  chance(p: number): boolean {
    return this.next() < p;
  }
}

/** Smooth noise from 0 to 1 over the plane, the same for the same seed. */
export function noise(seed: number): (x: number, y: number) => number {
  const at = (ix: number, iy: number) => {
    let h = Math.imul(ix, 374761393) ^ Math.imul(iy, 668265263) ^ Math.imul(seed, 1274126177);
    h = Math.imul(h ^ (h >>> 13), 1274126177);
    return ((h ^ (h >>> 16)) >>> 0) / 4294967296;
  };
  const ease = (t: number) => t * t * (3 - 2 * t);
  return (x, y) => {
    const ix = Math.floor(x);
    const iy = Math.floor(y);
    const fx = ease(x - ix);
    const fy = ease(y - iy);
    const top = at(ix, iy) * (1 - fx) + at(ix + 1, iy) * fx;
    const bottom = at(ix, iy + 1) * (1 - fx) + at(ix + 1, iy + 1) * fx;
    return top * (1 - fy) + bottom * fy;
  };
}

export interface TonePath {
  /** The classes of the path: "vx-b4", "vx-m3 vx-f1" for faded faces, "vx-wf" for the faces of a wire frame, or "vx-wl" for its lines. */
  tone: string;
  d: string;
}

/** The part of the lattice a picture shows: its top left corner, and its size, in lattice steps. */
export interface View {
  u: number;
  v: number;
  cols: number;
  rows: number;
}

type Face = 'top' | 'left' | 'right';

const OFFSET = 512;
const key = (x: number, y: number, z: number) => (x + OFFSET) * 1048576 + (y + OFFSET) * 1024 + (z + OFFSET);
const SHADE: Record<Face, number> = { top: 1, left: 0, right: -1 };

function toneOf(v: Voxel, face: Face): string {
  if (v.wire) return 'vx-wf';
  const level = Math.max(1, Math.min(6, v.level + SHADE[face]));
  return `vx-${v.family}${level}${v.fade ? ` vx-f${v.fade}` : ''}`;
}

/** The two triangles of the lattice that a face covers, as column and row of each. */
function triangles(face: Face, u: number, v: number): [number, number, number, number] {
  if (face === 'top') return [u - 1, v - 1, u, v - 1];
  if (face === 'left') return [u - 1, v, u - 1, v + 1];
  return [u, v, u, v + 1];
}

export class World {
  private readonly voxels = new Map<number, { x: number; y: number; z: number; v: Voxel }>();

  set(x: number, y: number, z: number, v: Voxel): void {
    this.voxels.set(key(x, y, z), { x, y, z, v });
  }

  clear(x: number, y: number, z: number): void {
    this.voxels.delete(key(x, y, z));
  }

  has(x: number, y: number, z: number): boolean {
    return this.voxels.has(key(x, y, z));
  }

  /** A solid box of one voxel, corners included. */
  box(x0: number, y0: number, z0: number, x1: number, y1: number, z1: number, v: Voxel | ((x: number, y: number, z: number) => Voxel | undefined)): void {
    for (let x = x0; x <= x1; x++)
      for (let y = y0; y <= y1; y++)
        for (let z = z0; z <= z1; z++) {
          const vox = typeof v === 'function' ? v(x, y, z) : v;
          if (vox) this.set(x, y, z, vox);
        }
  }

  /** The paths of what a viewer sees through `view`, one for each tone. */
  paths(view: View): TonePath[] {
    const cubes = [...this.voxels.values()];
    // The nearest face to cover each triangle, as depth and the face's number.
    const depth = new Map<number, number>();
    const winner = new Map<number, number>();
    const faces: Face[] = ['top', 'left', 'right'];
    const shown = (c: { x: number; y: number; z: number }, f: Face) =>
      f === 'top' ? !this.has(c.x, c.y, c.z + 1) : f === 'left' ? !this.has(c.x, c.y + 1, c.z) : !this.has(c.x + 1, c.y, c.z);
    const tri = (col: number, row: number) => (col + 4096) * 16384 + (row + 4096);

    cubes.forEach((c, i) => {
      const d = c.x + c.y + c.z;
      const u = c.x - c.y;
      const v = c.x + c.y - 2 * c.z;
      faces.forEach((f, fi) => {
        if (!shown(c, f)) return;
        const t = triangles(f, u, v);
        for (const k of [tri(t[0], t[1]), tri(t[2], t[3])]) {
          if ((depth.get(k) ?? -Infinity) < d) {
            depth.set(k, d);
            winner.set(k, i * 3 + fi);
          }
        }
      });
    });

    const X = (u: number) => (u - view.u) * A;
    const Y = (v: number) => (v - view.v) * B;
    const byTone = new Map<string, string[]>();
    const add = (tone: string, d: string) => {
      const list = byTone.get(tone) ?? [];
      list.push(d);
      byTone.set(tone, list);
    };
    // Whole faces of one tone in one plane, as cells of that plane: they are drawn as a few large faces.
    const planes = new Map<string, Set<number>>();
    const cell = (a: number, b: number) => (a + OFFSET) * 4096 + (b + OFFSET);
    // The edges of the wire frame, by direction and by the line each lies on.
    const edges = new Map<string, Set<number>>();
    const edge = (dir: string, line: number, at: number) => {
      const k = `${dir}|${line}`;
      edges.set(k, (edges.get(k) ?? new Set()).add(at));
    };

    cubes.forEach((c, i) => {
      const u = c.x - c.y;
      const v = c.x + c.y - 2 * c.z;
      if (u < view.u - 2 || u > view.u + view.cols + 2 || v < view.v - 4 || v > view.v + view.rows + 2) return;
      faces.forEach((f, fi) => {
        if (!shown(c, f)) return;
        const t = triangles(f, u, v);
        const first = winner.get(tri(t[0], t[1])) === i * 3 + fi;
        const second = winner.get(tri(t[2], t[3])) === i * 3 + fi;
        if (!first && !second) return;
        const tone = toneOf(c.v, f);
        if (first && second) {
          const [plane, a, b] = f === 'top' ? [c.z, c.x, c.y] : f === 'left' ? [c.y, c.x, c.z] : [c.x, c.y, c.z];
          const k = `${tone}|${f}|${plane}`;
          planes.set(k, (planes.get(k) ?? new Set()).add(cell(a, b)));
          if (!c.v.wire) return;
          // A slope down to the right is a line of constant v - u, and a slope up to the right one of constant v + u.
          if (f === 'top') {
            edge('up', v - 2 + u, u - 1);
            edge('up', v + u, u);
            edge('down', v - 2 - u, u);
            edge('down', v - u, u - 1);
          } else if (f === 'left') {
            edge('down', v - u, u - 1);
            edge('down', v + 2 - u, u - 1);
            edge('fall', u - 1, v - 1);
            edge('fall', u, v);
          } else {
            edge('up', v + u, u);
            edge('up', v + 2 + u, u);
            edge('fall', u, v);
            edge('fall', u + 1, v - 1);
          }
        } else if (f === 'top') {
          add(tone, first ? `M${X(u - 1)} ${Y(v - 1)}l${A} ${-B}v${2 * B}z` : `M${X(u)} ${Y(v - 2)}l${A} ${B}l${-A} ${B}z`);
        } else if (f === 'left') {
          add(tone, first ? `M${X(u - 1)} ${Y(v - 1)}l${A} ${B}l${-A} ${B}z` : `M${X(u)} ${Y(v)}v${2 * B}l${-A} ${-B}z`);
        } else {
          add(tone, first ? `M${X(u + 1)} ${Y(v - 1)}v${2 * B}l${-A} ${-B}z` : `M${X(u)} ${Y(v)}l${A} ${B}l${-A} ${B}z`);
        }
      });
    });

    for (const [k, cells] of planes) {
      const [tone, f, at] = k.split('|');
      const plane = Number(at);
      for (const start of [...cells].sort((p, q) => p - q)) {
        if (!cells.has(start)) continue;
        const a0 = Math.floor(start / 4096) - OFFSET;
        const b0 = (start % 4096) - OFFSET;
        // The widest run of cells along b, then as many such runs along a as are whole.
        let m = 1;
        while (cells.has(cell(a0, b0 + m))) m++;
        let n = 1;
        const whole = (a: number) => {
          for (let j = 0; j < m; j++) if (!cells.has(cell(a, b0 + j))) return false;
          return true;
        };
        while (whole(a0 + n)) n++;
        for (let i = 0; i < n; i++) for (let j = 0; j < m; j++) cells.delete(cell(a0 + i, b0 + j));
        if (f === 'top') {
          // a runs along x and b along y.
          const u = a0 - (b0 + m);
          const v = a0 + b0 + m - 2 * (plane + 1);
          add(tone, `M${X(u)} ${Y(v)}l${m * A} ${-m * B}l${n * A} ${n * B}l${-m * A} ${m * B}z`);
        } else if (f === 'left') {
          // a runs along x and b up z, in the plane y + 1.
          const u = a0 - plane - 1;
          const v = a0 + plane + 1 - 2 * (b0 + m);
          add(tone, `M${X(u)} ${Y(v)}l${n * A} ${n * B}v${2 * m * B}l${-n * A} ${-n * B}z`);
        } else {
          // a runs along y and b up z, in the plane x + 1.
          const u = plane + 1 - a0;
          const v = plane + 1 + a0 - 2 * (b0 + m);
          add(tone, `M${X(u)} ${Y(v)}v${2 * m * B}l${-n * A} ${n * B}v${-2 * m * B}z`);
        }
      }
    }

    const lines: string[] = [];
    for (const [k, at] of edges) {
      const [dir, l] = k.split('|');
      const line = Number(l);
      const step = dir === 'fall' ? 2 : 1;
      for (const start of [...at].sort((p, q) => p - q)) {
        if (at.has(start - step)) continue;
        let n = 1;
        while (at.has(start + n * step)) n++;
        if (dir === 'fall') lines.push(`M${X(line)} ${Y(start)}v${2 * n * B}`);
        else if (dir === 'down') lines.push(`M${X(start)} ${Y(line + start)}l${n * A} ${n * B}`);
        else lines.push(`M${X(start)} ${Y(line - start)}l${n * A} ${-n * B}`);
      }
    }

    // A minus sign separates two numbers as well as a space does.
    const join = (list: string[]) => list.join('').replaceAll(' -', '-');
    const paths = [...byTone.entries()].sort(([p], [q]) => (p < q ? -1 : 1)).map(([tone, list]) => ({ tone, d: join(list) }));
    if (lines.length) paths.push({ tone: 'vx-wl', d: join(lines) });
    return paths;
  }
}
