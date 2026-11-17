import type { ReactNode } from 'react';

const paths: Record<string, ReactNode> = {
  check: <path d="m3 8.5 3.2 3.2L13 5" />,
  cross: (
    <>
      <circle cx="8" cy="8" r="6" />
      <path d="m5.5 5.5 5 5M10.5 5.5l-5 5" />
    </>
  ),
  doc: (
    <>
      <path d="M4 2.5h7l3 3v10H4z" />
      <path d="M6.5 9h5M6.5 12h3" />
    </>
  ),
  key: (
    <>
      <circle cx="6" cy="9" r="3" />
      <path d="M9 9h7M13 9v3M15.5 9v2" />
    </>
  ),
  eye: (
    <>
      <path d="M2 9s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z" />
      <circle cx="9" cy="9" r="2" />
    </>
  ),
  code: <path d="m6.5 5.5-3.5 3.5 3.5 3.5M11.5 5.5 15 9l-3.5 3.5" />,
  flask: <path d="M7 2.5v5L3 15h12l-4-7.5v-5M6 2.5h6" />,
  log: <path d="M4 3h10v12H4zM7 7h4M7 10h4" />,
};

/** One of the page's small line icons. Each is decorative: the text beside it says the same. */
export function Icon({ name, size = 18 }: { name: keyof typeof paths | string; size?: number }) {
  const small = name === 'check' || name === 'cross';
  return (
    <svg
      width={size}
      height={size}
      viewBox={small ? '0 0 16 16' : '0 0 18 18'}
      fill="none"
      stroke="currentColor"
      strokeWidth={small ? 2.2 : 1.8}
      aria-hidden="true"
    >
      {paths[name]}
    </svg>
  );
}
