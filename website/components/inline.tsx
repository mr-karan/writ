import Link from 'next/link';
import type { ReactNode } from 'react';

/** Renders the inline Markdown the docs use in tables and lists: code, bold, and links. */
export function Inline({ text }: { text: string }): ReactNode {
  return text.split(/(`[^`]+`|\*\*[^*]+\*\*|\[[^\]]+\]\([^)]+\))/).map((part, i) => {
    if (part.startsWith('`')) return <code key={i}>{part.slice(1, -1)}</code>;
    if (part.startsWith('**')) return <strong key={i}>{part.slice(2, -2)}</strong>;
    const link = /^\[([^\]]+)\]\(([^)]+)\)$/.exec(part);
    if (link) {
      return (
        <Link key={i} href={link[2]}>
          {link[1]}
        </Link>
      );
    }
    return part;
  });
}
