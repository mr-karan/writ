import localFont from 'next/font/local';

// Every face ships with the site from pinned @fontsource packages, so no page
// asks a font host for anything. Latin subsets only: the docs are English.

// Archivo, with its width axis, set at its normal width.
export const sans = localFont({
  src: '../node_modules/@fontsource-variable/archivo/files/archivo-latin-wdth-normal.woff2',
  variable: '--font-archivo',
  weight: '100 900',
  display: 'swap',
  declarations: [{ prop: 'font-stretch', value: '62% 125%' }],
  fallback: ['ui-sans-serif', 'system-ui', 'sans-serif'],
});

// Newsreader at one weight, for the landing page's headings. One weight is
// under half the size of the variable file, and the headline is all it sets.
export const serif = localFont({
  src: '../node_modules/@fontsource/newsreader/files/newsreader-latin-500-normal.woff2',
  variable: '--font-newsreader',
  weight: '500',
  display: 'swap',
  fallback: ['Iowan Old Style', 'Palatino', 'Georgia', 'serif'],
});

// Chivo Mono, for code, terminal output, and tabular figures.
export const mono = localFont({
  src: '../node_modules/@fontsource-variable/chivo-mono/files/chivo-mono-latin-wght-normal.woff2',
  variable: '--font-chivo-mono',
  weight: '100 900',
  display: 'swap',
  fallback: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
});
