import './global.css';
import type { Metadata, Viewport } from 'next';
import type { ReactNode } from 'react';
import { Provider } from '@/components/provider';
import { site } from '@/lib/site';
import { mono, sans, serif } from './fonts';

export const metadata: Metadata = {
  title: { default: `${site.name}: ${site.tagline}`, template: `%s · ${site.name}` },
  description: site.description,
};

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#f6f4ec' },
    { media: '(prefers-color-scheme: dark)', color: '#10121f' },
  ],
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" className={`${sans.variable} ${serif.variable} ${mono.variable}`} suppressHydrationWarning>
      <body className="flex min-h-screen flex-col">
        <a href="#main-content" className="skip-link">
          Skip to content
        </a>
        <Provider>{children}</Provider>
      </body>
    </html>
  );
}
