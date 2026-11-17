import type { ReactNode } from 'react';

/** A terminal block: dark in both themes, with a title bar. */
export function Term({ title, action, children, small = false }: { title?: string; action?: ReactNode; children: ReactNode; small?: boolean }) {
  return (
    <div className={small ? 'term mini-term' : 'term'}>
      {title && (
        <div className="term-bar">
          <span>{title}</span>
          {action}
        </div>
      )}
      {children}
    </div>
  );
}
