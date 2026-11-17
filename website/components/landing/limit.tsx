import type { ReactNode } from 'react';

/** A limit, marked with the proofreader's caret of the wordmark: a correction to the claim above it. */
export function Limit({ children }: { children: ReactNode }) {
  return (
    <p className="limit">
      <Caret />
      <span>{children}</span>
    </p>
  );
}

export function Caret() {
  return (
    <svg className="caret" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
      <path d="M2.5 13 8 3.5 13.5 13" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="square" />
    </svg>
  );
}
