import Link from 'next/link';
import landing from '@/content/landing.json';
import { Transcript } from '@/lib/code';
import { tryIt } from '@/lib/landing';
import { CopyButton } from './copy-button';
import { Limit } from './limit';
import { Term } from './term';

export function TryIt() {
  const commands = [...landing.build.split('\n'), 'writ serve --dev'];

  return (
    <section id="try" className="sec sec-try" aria-labelledby="h-try">
      <div className="wrap">
        <div className="try-band">
          <div className="try-copy">
            <h2 id="h-try">{tryIt.title}</h2>
            <p>{tryIt.text}</p>
            <div className="cta">
              <Link className="btn btn-primary" href="/docs/quick-start">
                Quick start
              </Link>
              <Link className="btn btn-ghost" href="/docs/compare">
                Compare
              </Link>
            </div>
            <Limit>
              {tryIt.limit}{' '}
              <Link href={tryIt.limitLink.href}>{tryIt.limitLink.text}</Link>
            </Limit>
          </div>
          <div className="try-term">
            <Term title={tryIt.terminalTitle} action={<CopyButton text={commands.join('\n')} />}>
              <Transcript text={commands.map((c) => `$ ${c}`).join('\n')} wrap label={tryIt.terminalTitle} />
            </Term>
          </div>
        </div>
      </div>
    </section>
  );
}
