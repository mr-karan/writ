import Link from 'next/link';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { Transcript } from '@/lib/code';
import { tryIt } from '@/lib/landing';
import { site } from '@/lib/site';
import { CopyButton } from './copy-button';
import { Term } from './term';
import { SlabRule } from './voxel';

/** The page ends on the commands: install, start the dev control plane, plan, and apply. */
export function TryIt() {
  const commands = [...landing.build.split('\n'), 'writ serve --dev'];
  return (
    <section id="try" aria-labelledby="h-try">
      <div className="wrap">
        <SlabRule family="m" seed={7} />
        <div className="try-grid">
          <div>
            <h2 id="h-try">{tryIt.title}</h2>
            <p className="lede">
              <Inline text={tryIt.lede} />
            </p>
            <ul className="need">
              {tryIt.needs.map((need) => (
                <li key={need}>
                  <Inline text={need} />
                </li>
              ))}
            </ul>
            <div className="cta">
              <Link className="btn btn-primary" href="/docs/quick-start">
                Quick start
              </Link>
              <a className="btn btn-ghost" href={site.repo}>
                GitHub
              </a>
            </div>
          </div>
          <div className="stack-gap">
            <Term title={tryIt.build} action={<CopyButton text={commands.join('\n')} />}>
              <Transcript text={commands.map((c) => `$ ${c}`).join('\n')} label={tryIt.build} />
            </Term>
            <Term title={tryIt.run}>
              <Transcript text={landing.run} wrap label={tryIt.run} />
            </Term>
            <p className="caption">
              <Inline text={tryIt.caption} />
            </p>
          </div>
        </div>
      </div>
    </section>
  );
}
