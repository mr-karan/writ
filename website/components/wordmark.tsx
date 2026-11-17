// The mark is a proofreader's caret, the sign for "a change goes here", laid in slabs.
export function Wordmark({ className = '' }: { className?: string }) {
  return (
    <span className={`wordmark ${className}`}>
      <svg viewBox="0 0 32 32" width="26" height="26" aria-hidden="true">
        <rect width="32" height="32" rx="7" fill="var(--color-mark-ground)" />
        <path d="M13.5 7.5h5v3.2h-5zM10.5 11.9h5v3.2h-5zM16.5 11.9h5v3.2h-5zM7.5 16.3h5v3.2h-5zM19.5 16.3h5v3.2h-5zM4.5 20.7h5v3.2h-5zM22.5 20.7h5v3.2h-5z" fill="var(--color-mark)" />
      </svg>
      <span>Writ</span>
    </span>
  );
}
