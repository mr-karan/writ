import { Fragment } from 'react';
import Link from 'next/link';
import { workflow } from '@/lib/landing';

function StageIcon({ id }: { id: string }) {
  switch (id) {
    case 'push':
      return (
        <svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="5.5" cy="5.5" r="2.25" />
          <circle cx="14.5" cy="5.5" r="2.25" />
          <circle cx="5.5" cy="14.5" r="2.25" />
          <path d="M5.5 7.75v4.5M14.5 7.75c0 2.75-2 3.75-9 3.75" />
        </svg>
      );
    case 'plan':
      return (
        <svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <rect x="3.5" y="3" width="13" height="14" rx="2" />
          <path d="M7 7.5h6M7 10.5h6M7 13.5h3.5" />
        </svg>
      );
    case 'policy':
      return (
        <svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M10 2.5 16.5 5v4.5c0 3.7-2.6 6.7-6.5 8-3.9-1.3-6.5-4.3-6.5-8V5L10 2.5Z" />
          <path d="m7.3 10.1 1.9 1.9 3.6-3.8" />
        </svg>
      );
    case 'approve':
      return (
        <svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <circle cx="8" cy="6.5" r="2.75" />
          <path d="M3 16c.6-2.6 2.5-4 5-4s4.4 1.4 5 4M13.5 9.8l1.6 1.6 3.1-3.3" />
        </svg>
      );
    case 'apply':
      return (
        <svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M6.5 4.5 15.5 10l-9 5.5V4.5Z" />
        </svg>
      );
    default:
      return (
        <svg viewBox="0 0 20 20" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
          <path d="M4 5.5h12M4 10h12M4 14.5h8" />
          <circle cx="15.5" cy="14.5" r="1.5" />
        </svg>
      );
  }
}

export function Workflow() {
  const ciStages = workflow.stages.filter((s) => s.zone === 'ci');
  const serverStages = workflow.stages.filter((s) => s.zone === 'server');

  return (
    <section id="how" className="sec" aria-labelledby="h-how">
      <div className="wrap">
        <div className="sec-bar">
          <h2 id="h-how">{workflow.title}</h2>
          <Link className="sec-link" href={workflow.link.href}>
            {workflow.link.text} <span aria-hidden="true">→</span>
          </Link>
        </div>

        <div className="pipe-diagram" role="img" aria-label={workflow.diagramAlt}>
          <div className="pipe-zone pipe-ci">
            <div className="pipe-nodes">
              {ciStages.map((s) => (
                <div key={s.id} className="pipe-node">
                  <span className="pipe-icon">
                    <StageIcon id={s.id} />
                  </span>
                  <span className="pipe-label">{s.label}</span>
                </div>
              ))}
            </div>
            <span className="pipe-zone-tag">{workflow.ciLabel}</span>
          </div>

          <div className="pipe-bridge" aria-hidden="true">
            <svg viewBox="0 0 44 20" width="44" height="20" fill="none">
              <line x1="2" y1="10" x2="36" y2="10" className="pipe-line" />
              <path d="m32 5 6 5-6 5" className="pipe-head" />
            </svg>
          </div>

          <div className="pipe-zone pipe-server">
            <div className="pipe-nodes pipe-server-nodes">
              {serverStages.map((s, idx) => (
                <Fragment key={s.id}>
                  {idx > 0 && (
                    <svg className="pipe-arrow" viewBox="0 0 24 16" width="24" height="16" fill="none" aria-hidden="true">
                      <line x1="2" y1="8" x2="18" y2="8" className="pipe-line" />
                      <path d="m15 4 5 4-5 4" className="pipe-head" />
                    </svg>
                  )}
                  <div className={`pipe-node${s.id === 'approve' ? ' pipe-node-gate' : ''}`}>
                    <span className="pipe-icon">
                      <StageIcon id={s.id} />
                    </span>
                    <span className="pipe-label">{s.label}</span>
                  </div>
                </Fragment>
              ))}
            </div>
            <span className="pipe-zone-tag pipe-zone-tag-server">{workflow.serverLabel}</span>
          </div>
        </div>

        <p className="pipe-note">{workflow.note}</p>
      </div>
    </section>
  );
}
