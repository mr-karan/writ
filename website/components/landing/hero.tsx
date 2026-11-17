import Link from 'next/link';
import { hero, tour } from '@/lib/landing';
import { Frame, Shot, shotPaths } from './shot';

export function Hero() {
  return (
    <section className="hero" aria-labelledby="h-hero">
      <div className="wrap">
        <h1 id="h-hero">{hero.title}</h1>
        <div className="hero-grid">
          <div>
            <p className="lede">{hero.lede}</p>
            <div className="cta">
              <a className="btn btn-primary" href="#try">
                Get started
              </a>
              <Link className="btn btn-ghost" href="/docs">
                Read the docs
              </Link>
            </div>
          </div>
          <dl className="facts">
            {hero.facts.map((fact, i) => (
              <div key={fact.label}>
                <dt>{fact.label}</dt>
                <dd>
                  {fact.text}
                  {i === hero.facts.length - 1 && (
                    <>
                      {' '}
                      <a href={hero.notYet.href}>{hero.notYet.text}</a>
                    </>
                  )}
                </dd>
              </div>
            ))}
          </dl>
        </div>
        <div className="hero-shot">
          <Frame path={shotPaths.plan}>
            <div className="view">
              <Shot name="hero" alt={tour.tabs[1].alt} eager />
            </div>
          </Frame>
        </div>
      </div>
    </section>
  );
}
