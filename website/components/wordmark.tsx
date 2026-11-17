// The mark is a proofreader's caret: the sign for "a change goes here".
export function Wordmark({ className = '' }: { className?: string }) {
  return (
    <span className={`wordmark ${className}`}>
      <svg viewBox="0 0 32 32" width="26" height="26" aria-hidden="true">
        <rect width="32" height="32" rx="7" fill="var(--color-mark-ground)" />
        <path d="M8 23.5 16 9l8 14.5" fill="none" stroke="var(--color-mark)" strokeWidth="4" strokeLinecap="square" />
      </svg>
      <span>Writ</span>
    </span>
  );
}
