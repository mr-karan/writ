import Link from 'next/link';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { Transcript } from '@/lib/code';
import { close, questions, tryIt } from '@/lib/landing';
import { site } from '@/lib/site';
import { CopyButton } from './copy-button';
import { Icon } from './icons';
import { Term } from './term';

export function TryIt() {
  const commands = [...landing.build.split('\n'), 'writ serve --dev'];
  return (
    <section id="try" aria-labelledby="h-try">
      <div className="wrap">
        <div className="try-grid">
          <div>
            <h2 id="h-try">{tryIt.title}</h2>
            <p className="lede">
              <Inline text={tryIt.lede} />
            </p>
            <ul className="need">
              {tryIt.needs.map((need) => (
                <li key={need}>
                  <Icon name="check" size={15} />
                  <span>
                    <Inline text={need} />
                  </span>
                </li>
              ))}
            </ul>
            <div className="cta start">
              <Link className="btn btn-primary" href="/docs/quick-start">
                Follow the quick start
              </Link>
              <Link className="btn btn-ghost" href="/docs/quick-start#set-up-writ-for-a-team">
                Set it up for a team
              </Link>
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

        <div className="faq">
          <h2>Questions</h2>
          <div>
            {questions.map(({ q, a }) => (
              <details key={q}>
                <summary>{q}</summary>
                <p>
                  <Inline text={a} />
                </p>
              </details>
            ))}
          </div>
        </div>

        <div className="final">
          <h2>{close.title}</h2>
          <p>{close.text}</p>
          <div className="cta">
            <Link className="btn btn-primary" href="/docs/quick-start">
              Follow the quick start
            </Link>
            <a className="btn btn-ghost" href={site.repo}>
              View on GitHub
            </a>
          </div>
        </div>
      </div>
    </section>
  );
}
