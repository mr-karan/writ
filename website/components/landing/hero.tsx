import Link from 'next/link';
import landing from '@/content/landing.json';
import { hero } from '@/lib/landing';
import { CopyButton } from './copy-button';
import { Shot, shotPaths } from './shot';

export function Hero() {
  const installCmd = landing.build.split('\n')[0];

  return (
    <section className="hero" aria-labelledby="h-hero">
      <div className="wrap hero-head">
        <h1 id="h-hero">{hero.title}</h1>
        <p className="hero-sub">{hero.subhead}</p>
        <div className="hero-actions">
          <Link className="btn btn-primary" href="/docs/quick-start">
            Quick start
          </Link>
          <div className="install-bar" aria-label="Install writ">
            <code className="install-cmd">
              <span className="install-prompt" aria-hidden="true">
                ${' '}
              </span>
              {installCmd}
            </code>
            <CopyButton text={installCmd} className="install-copy" />
          </div>
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
      </div>
    </section>
  );
}
