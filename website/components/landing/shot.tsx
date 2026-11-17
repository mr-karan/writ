import type { ReactNode } from 'react';
import shots from '@/lib/ui-shots.json';

function Picture({ name, scheme, phone, alt, eager }: { name: string; scheme: 'light' | 'dark'; phone: boolean; alt: string; eager: boolean }) {
  const wide = `${name}-1280-${scheme}`;
  const narrow = `${name}-390-${scheme}`;
  const single = `${name}-${scheme}`;
  const main = phone ? wide : single;
  const size = shots.sizes[main as keyof typeof shots.sizes];
  const small = shots.sizes[narrow as keyof typeof shots.sizes];
  return (
    <picture className={`shot-${scheme}`}>
      {phone && <source media="(max-width: 700px)" type="image/avif" srcSet={`/ui/${narrow}.avif`} width={small.width} height={small.height} />}
      {phone && <source media="(max-width: 700px)" type="image/webp" srcSet={`/ui/${narrow}.webp`} width={small.width} height={small.height} />}
      <source type="image/avif" srcSet={`/ui/${main}.avif`} />
      <img
        src={`/ui/${main}.webp`}
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
 * display: none, so a screen reader reads one description either way. A page shot also has a capture
 * made at 390 px, which a phone gets, so its text stays readable.
 */
export function Shot({ name, alt, eager = false }: { name: string; alt: string; eager?: boolean }) {
  const phone = `${name}-390-light` in shots.sizes;
  return (
    <>
      <Picture name={name} scheme="light" phone={phone} alt={alt} eager={eager} />
      <Picture name={name} scheme="dark" phone={phone} alt={alt} eager={eager} />
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
