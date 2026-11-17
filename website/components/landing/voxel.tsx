import type { Family } from '@/lib/voxel/iso';
import { pictures, ruleScene } from '@/lib/voxel/scenes';

/**
 * One of the page's pictures. Its voxels are in a static SVG file, which
 * the page draws with <use>, so the picture takes the page's tokens and
 * follows the theme without script. The cubes that light up in turn are
 * drawn here. With no label it is decoration and hidden from a screen
 * reader.
 */
export function Voxel({ name, className = '', label }: { name: keyof typeof pictures; className?: string; label?: string }) {
  const scene = pictures[name];
  return (
    <svg
      className={`voxel ${className}`}
      viewBox={`0 0 ${scene.width} ${scene.height}`}
      role={label ? 'img' : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      {scene.layers.map((layer) => (
        <use key={layer.name} className={`vx-${layer.name}`} href={`/voxel/${name}.svg#${layer.name}`} />
      ))}
      {scene.sparks.map((s, i) => (
        <g key={`${s.x}-${s.y}`} className="vx-spark" transform={`translate(${s.x} ${s.y})`} style={{ animationDelay: `${i * 0.18}s` }}>
          <path className="vx-o5" d="M-8-4l8-4l8 4l-8 4z" />
          <path className="vx-o4" d="M-8-4l8 4v8l-8-4z" />
          <path className="vx-o3" d="M0 0l8-4v8l-8 4z" />
        </g>
      ))}
    </svg>
  );
}

/** The rule that opens a section: slabs that thin out to the right. */
export function SlabRule({ family, seed }: { family: Family; seed: number }) {
  const rule = ruleScene(family, seed);
  return (
    <svg className="voxel slab-rule" viewBox={`0 0 ${rule.width} ${rule.height}`} preserveAspectRatio="xMinYMid slice" aria-hidden="true">
      {rule.paths.map((p) => (
        <path key={p.tone} className={p.tone} d={p.d} />
      ))}
    </svg>
  );
}
