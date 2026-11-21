import { features } from '@/lib/landing';

function FeatureIcon({ id }: { id: string }) {
  switch (id) {
    case 'keys':
      return (
        <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="7" cy="10" r="3.5" />
          <path d="M10.5 10H18m-2.5 0v2.5M13 10v2" />
        </svg>
      );
    case 'policy':
      return (
        <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M10 2.5 16.5 5v4.5c0 3.7-2.6 6.7-6.5 8-3.9-1.3-6.5-4.3-6.5-8V5L10 2.5Z" />
          <path d="m7.3 10.1 1.9 1.9 3.6-3.8" />
        </svg>
      );
    case 'secrets':
      return (
        <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M3 3l14 14M8.2 8.2a2.5 2.5 0 0 0 3.6 3.6M5.2 6.6C3.7 7.8 2.8 9.2 2.5 10c1.1 2.7 3.9 5 7.5 5 1.3 0 2.5-.3 3.6-.8M14.8 13.4c1.5-1.2 2.4-2.6 2.7-3.4-1.1-2.7-3.9-5-7.5-5-.8 0-1.6.1-2.3.4" />
        </svg>
      );
    case 'drift':
      return (
        <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M2.5 10h3.2l2-5 3.2 10 2.2-5h4.4" />
        </svg>
      );
    case 'sandbox':
      return (
        <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M10 2.5 16.5 6v8L10 17.5 3.5 14V6L10 2.5Z" />
          <path d="M10 10 16.5 6M10 10 3.5 6M10 10v7.5" />
        </svg>
      );
    default:
      return (
        <svg viewBox="0 0 20 20" width="20" height="20" fill="none" stroke="currentColor" strokeWidth="1.65" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <rect x="2.5" y="4" width="15" height="12" rx="2" />
          <path d="m6 8.5 2.5 2-2.5 2M10.5 12.5H14" />
        </svg>
      );
  }
}

export function Features() {
  return (
    <section id="features" className="sec" aria-labelledby="h-features">
      <div className="wrap">
        <div className="sec-bar">
          <h2 id="h-features">{features.title}</h2>
        </div>

        <div className="feat-grid">
          {features.items.map((item) => (
            <article key={item.id} className="feat-card">
              <span className="feat-icon">
                <FeatureIcon id={item.id} />
              </span>
              <h3>{item.title}</h3>
              <p>{item.text}</p>
            </article>
          ))}
        </div>
      </div>
    </section>
  );
}
