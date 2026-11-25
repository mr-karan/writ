'use client';

import { useEffect, useState } from 'react';

/** Copies a block's commands. Without script it doesn't render, and the commands stay selectable. */
export function CopyButton({ text, className = 'copy' }: { text: string; className?: string }) {
  const [label, setLabel] = useState<string | null>(null);
  const [ready, setReady] = useState(false);
  useEffect(() => setReady(true), []);
  if (!ready) return null;
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setLabel('Copied');
    } catch {
      setLabel('Select and copy');
    }
    setTimeout(() => setLabel(null), 1600);
  };
  return (
    <button type="button" className={className} onClick={copy} aria-live="polite">
      {label ?? 'Copy'}
    </button>
  );
}
