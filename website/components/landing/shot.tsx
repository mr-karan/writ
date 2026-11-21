import type { ReactNode } from 'react';
import shots from '@/lib/ui-shots.json';

function Picture({
  name,
  mobileName,
  scheme,
  alt,
  eager,
}: {
  name: string;
  mobileName?: string;
  scheme: 'light' | 'dark';
  alt: string;
  eager: boolean;
}) {
  const hasOwnPhone = `${name}-390-light` in shots.sizes;
  const wide = hasOwnPhone ? `${name}-1280-${scheme}` : `${name}-${scheme}`;
  const phoneBase = mobileName ?? (hasOwnPhone ? name : undefined);
  const narrow = phoneBase ? `${phoneBase}-390-${scheme}` : undefined;
  const size = shots.sizes[wide as keyof typeof shots.sizes];
  const small = narrow ? shots.sizes[narrow as keyof typeof shots.sizes] : undefined;

  return (
    <picture className={`shot-${scheme}`}>
      {narrow && small && <source media="(max-width: 700px)" type="image/avif" srcSet={`/ui/${narrow}.avif`} width={small.width} height={small.height} />}
      {narrow && small && <source media="(max-width: 700px)" type="image/webp" srcSet={`/ui/${narrow}.webp`} width={small.width} height={small.height} />}
      <source type="image/avif" srcSet={`/ui/${wide}.avif`} />
      <img
        src={`/ui/${wide}.webp`}
        width={size.width}
        height={size.height}
        alt={alt}
        loading={eager && scheme === 'light' ? 'eager' : 'lazy'}
        fetchPriority={eager && scheme === 'light' ? 'high' : undefined}
        decoding="async"
      />
    </picture>
  );
}

/**
 * A real screenshot of the control UI in its light and dark captures. The
 * page shows the one that matches its theme, and hides the other with
 * display: none, so a screen reader reads one description either way. Page
 * shots (or crops with a `mobileName`) swap to native 390 px captures on
 * narrow viewports so UI text stays legible.
 */
export function Shot({ name, mobileName, alt, eager = false }: { name: string; mobileName?: string; alt: string; eager?: boolean }) {
  return (
    <>
      <Picture name={name} mobileName={mobileName} scheme="light" alt={alt} eager={eager} />
      <Picture name={name} mobileName={mobileName} scheme="dark" alt={alt} eager={eager} />
    </>
  );
}

/** A frame with the example address of the page it shows: the control UI is on your own host. */
export function Frame({ path, children, className = '' }: { path: string; children: ReactNode; className?: string }) {
  return (
    <div className={`frame ${className}`}>
      <div className="frame-bar">
        <span className="url">writ.example.com{path}</span>
      </div>
      {children}
    </div>
  );
}

export const shotPaths = shots.paths;
export const shotDate = shots.date;
