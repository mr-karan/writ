import Link from 'next/link';
import { hero } from '@/lib/landing';
import { Voxel } from './voxel';

export function Hero() {
  return (
    <section className="hero" aria-labelledby="h-hero">
      <div className="wrap hero-text">
        <h1 id="h-hero">
          {hero.title.plain} <em>{hero.title.accent}</em>
        </h1>
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
      <div className="hero-art">
        <Voxel name="hero" label={hero.art} />
      </div>
      <div className="wrap">
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
    </section>
  );
}
