import Link from 'next/link';
import { hero, tour } from '@/lib/landing';
import { Icon } from './icons';
import { Frame, Shot, shotPaths } from './shot';

export function Hero() {
  return (
    <>
      <section className="hero" aria-labelledby="h-hero">
        <div className="wrap">
          <p className="chip">
            <b>{hero.chip.tag}</b> {hero.chip.text}
          </p>
          <h1 id="h-hero">{hero.title}</h1>
          <p className="lede">{hero.lede}</p>
          <div className="cta">
            <a className="btn btn-primary" href="#try">
              Get started
            </a>
            <Link className="btn btn-ghost" href="/docs">
              Read the docs
            </Link>
          </div>
          <p className="v0">
            {hero.note} <a href="#notyet">What Writ isn&rsquo;t, yet</a>
          </p>
          <div className="hero-shot">
            <Frame path={shotPaths.plan}>
              <div className="view">
                <Shot name="hero" alt={tour.tabs[1].alt} eager />
              </div>
            </Frame>
          </div>
        </div>
      </section>
      <div className="factbar">
        <div className="wrap">
          <ul className="facts" aria-label="Facts">
            {hero.facts.map((fact) => (
              <li key={fact}>
                <Icon name="check" size={15} />
                {fact}
              </li>
            ))}
          </ul>
        </div>
      </div>
    </>
  );
}
