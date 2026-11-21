import Link from 'next/link';
import { hero } from '@/lib/landing';
import { site } from '@/lib/site';
import { Shot, shotPaths } from './shot';

export function Hero() {
  return (
    <section className="hero" aria-labelledby="h-hero">
      <div className="wrap hero-head">
        <h1 id="h-hero">
          {hero.title.plain} <em>{hero.title.accent}</em>
        </h1>
        <p className="hero-sub">{hero.subhead}</p>
        <div className="hero-cta">
          <Link className="btn btn-primary" href="/docs/quick-start">
            Quick start
          </Link>
          <a className="btn btn-ghost" href={site.repo}>
            <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="currentColor">
              <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8" />
            </svg>
            GitHub
          </a>
        </div>
      </div>

      <div className="wrap hero-stage">
        <div className="product-window">
          <div className="window-bar">
            <div className="window-tabs" role="radiogroup" aria-label="Control UI views">
              {hero.tabs.map((tab, i) => (
                <span key={tab.id}>
                  <input type="radio" name="hero-ui" id={`hero-ui-${tab.id}`} defaultChecked={i === 0} />
                  <label htmlFor={`hero-ui-${tab.id}`}>{tab.label}</label>
                </span>
              ))}
            </div>
            <div className="window-urls" aria-hidden="true">
              {hero.tabs.map((tab) => (
                <span key={tab.id} className={`url window-url-${tab.id}`}>
                  writ.example.com{shotPaths[tab.id]}
                </span>
              ))}
            </div>
          </div>

          {hero.tabs.map((tab, i) => (
            <div key={tab.id} className={`window-panel window-panel-${tab.id}`}>
              <div className="view" tabIndex={0} role="region" aria-label={`${tab.label}, Writ control UI screenshot`}>
                <Shot name={tab.id} alt={tab.alt} eager={i === 0} />
              </div>
            </div>
          ))}
        </div>

        <dl className="proof-strip">
          {hero.proof.map((item) => (
            <div key={item.label}>
              <dt>{item.label}</dt>
              <dd>{item.detail}</dd>
            </div>
          ))}
        </dl>
      </div>
    </section>
  );
}
