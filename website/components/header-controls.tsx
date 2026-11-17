'use client';

import { useSearchContext } from 'fumadocs-ui/contexts/search';
import { ThemeSwitch } from 'fumadocs-ui/layouts/shared/slots/theme-switch';

/** The search button and the theme switch, for the landing page's header. */
export function HeaderControls() {
  const { setOpenSearch } = useSearchContext();
  return (
    <div className="flex items-center gap-1">
      <button type="button" className="icon-button" aria-label="Search the docs" onClick={() => setOpenSearch(true)}>
        <svg viewBox="0 0 20 20" width="18" height="18" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round">
          <circle cx="9" cy="9" r="5.5" />
          <path d="m13.5 13.5 3.5 3.5" />
        </svg>
      </button>
      <ThemeSwitch mode="light-dark" />
    </div>
  );
}
