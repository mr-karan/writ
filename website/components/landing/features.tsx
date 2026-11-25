import { features } from '@/lib/landing';

function FeatureArt({ id }: { id: string }) {
  switch (id) {
    case 'keys':
      return (
        <svg viewBox="0 0 280 96" className="feat-svg" fill="none" aria-hidden="true">
          <rect x="16" y="26" width="84" height="44" rx="8" stroke="var(--color-rule-strong)" strokeWidth="1.4" strokeDasharray="4 3" />
          <text x="58" y="52" textAnchor="middle" fill="var(--color-ink-2)" fontSize="11.5" fontFamily="var(--font-mono)">
            OIDC
          </text>
          <line x1="100" y1="48" x2="140" y2="48" stroke="var(--color-ink-3)" strokeWidth="1.4" strokeDasharray="3 3" />
          <line x1="140" y1="14" x2="140" y2="82" stroke="var(--color-mark)" strokeWidth="2" />
          <line x1="140" y1="48" x2="174" y2="48" stroke="var(--color-accent)" strokeWidth="1.5" />
          <path d="m169 43 6 5-6 5" stroke="var(--color-accent)" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
          <rect x="176" y="26" width="88" height="44" rx="8" fill="var(--color-accent-soft)" stroke="var(--color-accent)" strokeWidth="1.4" />
          <text x="220" y="52" textAnchor="middle" fill="var(--color-accent)" fontSize="11.5" fontWeight="600" fontFamily="var(--font-mono)">
            AWS STS
          </text>
        </svg>
      );
    case 'policy':
      return (
        <svg viewBox="0 0 280 96" className="feat-svg" fill="none" aria-hidden="true">
          <line x1="20" y1="26" x2="128" y2="26" stroke="var(--color-ok)" strokeWidth="1.6" />
          <line x1="20" y1="48" x2="128" y2="48" stroke="var(--color-warn)" strokeWidth="1.6" />
          <line x1="20" y1="70" x2="128" y2="70" stroke="var(--color-bad)" strokeWidth="1.6" />
          <circle cx="24" cy="26" r="3" fill="var(--color-ok)" />
          <circle cx="24" cy="48" r="3" fill="var(--color-warn)" />
          <circle cx="24" cy="70" r="3" fill="var(--color-bad)" />
          <path d="M148 16 174 26v21c0 16-11 29-26 35-15-6-26-19-26-35V26l26-10Z" fill="var(--color-surface-2)" stroke="var(--color-rule-strong)" strokeWidth="1.4" />
          <text x="148" y="52" textAnchor="middle" fill="var(--color-ink)" fontSize="10.5" fontWeight="600" fontFamily="var(--font-mono)">
            Rego
          </text>
          <line x1="176" y1="26" x2="256" y2="26" stroke="var(--color-ok)" strokeWidth="1.6" />
          <line x1="176" y1="48" x2="256" y2="48" stroke="var(--color-warn)" strokeWidth="1.6" />
          <path d="m202 64 12 12m0-12-12 12" stroke="var(--color-bad)" strokeWidth="1.8" strokeLinecap="round" />
          <text x="224" y="74" fill="var(--color-bad)" fontSize="11" fontWeight="600" fontFamily="var(--font-mono)">
            deny
          </text>
        </svg>
      );
    case 'secrets':
      return (
        <svg viewBox="0 0 280 96" className="feat-svg" fill="none" aria-hidden="true">
          <rect x="20" y="14" width="240" height="68" rx="8" fill="var(--color-bg)" stroke="var(--color-rule)" strokeWidth="1.2" />
          <text x="34" y="34" fill="var(--color-ink-3)" fontSize="11" fontFamily="var(--font-mono)">
            region   = &quot;eu-west-1&quot;
          </text>
          <text x="34" y="54" fill="var(--color-ink)" fontSize="11" fontFamily="var(--font-mono)">
            secret   =
          </text>
          <rect x="114" y="42" width="104" height="18" rx="4" fill="var(--color-warn-soft)" stroke="var(--color-gild)" strokeWidth="1" />
          <text x="166" y="54" textAnchor="middle" fill="var(--color-gild)" fontSize="10.5" fontWeight="600" fontFamily="var(--font-mono)">
            (sensitive)
          </text>
          <text x="34" y="73" fill="var(--color-ink-3)" fontSize="11" fontFamily="var(--font-mono)">
            version  = 14
          </text>
        </svg>
      );
    case 'drift':
      return (
        <svg viewBox="0 0 280 96" className="feat-svg" fill="none" aria-hidden="true">
          <text x="20" y="34" fill="var(--color-ink-3)" fontSize="10.5" fontFamily="var(--font-mono)">
            state
          </text>
          <text x="20" y="68" fill="var(--color-ink-3)" fontSize="10.5" fontFamily="var(--font-mono)">
            cloud
          </text>
          <line x1="68" y1="30" x2="256" y2="30" stroke="var(--color-rule-strong)" strokeWidth="1.6" />
          <path d="M68 64h84c14 0 18-16 32-16h72" stroke="var(--color-gild)" strokeWidth="1.8" />
          <circle cx="96" cy="30" r="4" fill="var(--color-ok)" />
          <circle cx="96" cy="64" r="4" fill="var(--color-ok)" />
          <circle cx="152" cy="30" r="4" fill="var(--color-ok)" />
          <circle cx="152" cy="64" r="4" fill="var(--color-ok)" />
          <circle cx="216" cy="48" r="6" fill="var(--color-warn-soft)" stroke="var(--color-gild)" strokeWidth="1.8" />
          <circle cx="216" cy="48" r="2.2" fill="var(--color-gild)" />
        </svg>
      );
    case 'sandbox':
      return (
        <svg viewBox="0 0 280 96" className="feat-svg" fill="none" aria-hidden="true">
          <rect x="20" y="12" width="166" height="72" rx="8" stroke="var(--color-rule-strong)" strokeWidth="1.3" strokeDasharray="4 3" />
          <text x="32" y="27" fill="var(--color-ink-3)" fontSize="10" fontFamily="var(--font-mono)">
            Linux ns · cgroup
          </text>
          <rect x="34" y="36" width="138" height="38" rx="6" fill="var(--color-surface-2)" stroke="var(--color-ink-3)" strokeWidth="1.2" />
          <text x="103" y="59" textAnchor="middle" fill="var(--color-ink)" fontSize="11" fontWeight="600" fontFamily="var(--font-mono)">
            uid 61001 · tofu
          </text>
          <line x1="186" y1="55" x2="254" y2="55" stroke="var(--color-accent)" strokeWidth="1.5" />
          <path d="m249 50 6 5-6 5" stroke="var(--color-accent)" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
          <text x="220" y="45" textAnchor="middle" fill="var(--color-accent)" fontSize="10" fontFamily="var(--font-mono)">
            egress
          </text>
        </svg>
      );
    default:
      return (
        <svg viewBox="0 0 280 96" className="feat-svg" fill="none" aria-hidden="true">
          <rect x="18" y="26" width="76" height="44" rx="8" fill="var(--color-surface-2)" stroke="var(--color-rule-strong)" strokeWidth="1.3" />
          <text x="56" y="52" textAnchor="middle" fill="var(--color-ink)" fontSize="11" fontWeight="600" fontFamily="var(--font-mono)">
            agent
          </text>
          <line x1="94" y1="38" x2="174" y2="38" stroke="var(--color-ok)" strokeWidth="1.5" />
          <path d="m169 34 5 4-5 4" stroke="var(--color-ok)" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
          <text x="134" y="31" textAnchor="middle" fill="var(--color-ok)" fontSize="10" fontFamily="var(--font-mono)">
            plan
          </text>
          <line x1="94" y1="60" x2="174" y2="60" stroke="var(--color-rule-strong)" strokeWidth="1.4" strokeDasharray="3 3" />
          <path d="m130 55 9 10m0-10-9 10" stroke="var(--color-bad)" strokeWidth="1.6" strokeLinecap="round" />
          <text x="134" y="78" textAnchor="middle" fill="var(--color-ink-3)" fontSize="10" fontFamily="var(--font-mono)">
            no apply
          </text>
          <rect x="178" y="26" width="84" height="44" rx="8" fill="var(--color-bg)" stroke="var(--color-rule-strong)" strokeWidth="1.3" />
          <text x="220" y="52" textAnchor="middle" fill="var(--color-ink-2)" fontSize="11" fontFamily="var(--font-mono)">
            writ mcp
          </text>
        </svg>
      );
  }
}

export function Features() {
  return (
    <section id="features" className="sec" aria-labelledby="h-features">
      <div className="wrap">
        <div className="sec-center">
          <h2 id="h-features">{features.title}</h2>
        </div>

        <div className="feat-grid">
          {features.items.map((item) => (
            <article key={item.id} className="feat-card">
              <div className="feat-art">
                <FeatureArt id={item.id} />
              </div>
              <h3>{item.title}</h3>
              <p>{item.text}</p>
            </article>
          ))}
        </div>
      </div>
    </section>
  );
}
