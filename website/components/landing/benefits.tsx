import Link from 'next/link';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { Transcript } from '@/lib/code';
import { benefits } from '@/lib/landing';
import { Shot } from './shot';
import { Term } from './term';

function More({ link }: { link: { text: string; href: string } }) {
  return (
    <Link className="more" href={link.href}>
      {link.text}
    </Link>
  );
}

export function Benefits() {
  const { keys, secrets, drift, policy, agents, also } = benefits;
  return (
    <section id="benefits" aria-labelledby="h-benefits">
      <div className="wrap">
        <div className="sec-head">
          <h2 id="h-benefits">{benefits.title}</h2>
        </div>
        <div className="bento">
          <article className="card b-wide">
            <h3>{keys.title}</h3>
            <p>{keys.text}</p>
            <p className="limit">{keys.limit}</p>
            <More link={keys.link} />
            <div className="visual">
              <table className="holds">
                <thead>
                  <tr>
                    <th scope="col">Who</th>
                    <th scope="col">Cloud credentials</th>
                    <th scope="col">Your state</th>
                  </tr>
                </thead>
                <tbody>
                  {keys.holds.map((row) => (
                    <tr key={row.who}>
                      <th scope="row">{row.who}</th>
                      <td className={row.yes ? 'yes' : 'no'}>{row.creds}</td>
                      <td className={row.yes ? 'yes' : 'no'}>{row.state}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </article>

          <article className="card b-wide">
            <h3>{secrets.title}</h3>
            <p>{secrets.text}</p>
            <p className="limit">{secrets.limit}</p>
            <More link={secrets.link} />
            <div className="visual">
              <div className="crop" tabIndex={0} role="region" aria-label="A replaced resource in a plan, screenshot">
                <Shot name="secret" alt={secrets.alt} />
              </div>
            </div>
          </article>

          <article className="card b-two">
            <h3>{drift.title}</h3>
            <p>{drift.text}</p>
            <p className="limit">{drift.limit}</p>
            <More link={drift.link} />
            <div className="visual">
              <div className="crop" tabIndex={0} role="region" aria-label="A stack's drift check, screenshot">
                <Shot name="drift" alt={drift.alt} />
              </div>
            </div>
          </article>

          <article className="card b-third">
            <h3>{policy.title}</h3>
            <p>{policy.text}</p>
            <p className="limit">
              <Inline text={policy.limit} />
            </p>
            <More link={policy.link} />
            <div className="visual">
              <Term small>
                <Transcript text={landing.policyDeny} wrap label="Output of writ plan on a commit that a policy denies" />
              </Term>
              <p className="caption">
                <Inline text={policy.caption} />
              </p>
            </div>
          </article>

          <article className="card b-full">
            <div>
              <h3>{agents.title}</h3>
              <p>
                <Inline text={agents.text} />
              </p>
              <More link={agents.link} />
            </div>
            <div className="visual">
              <Term small>
                <Transcript text={landing.mcpAdd} wrap label="Output of claude mcp add for writ mcp" />
              </Term>
              <p className="caption">{agents.caption}</p>
            </div>
          </article>
        </div>

        <div className="also">
          {also.map((item) => (
            <div key={item.title}>
              <b>{item.title}</b>
              <Inline text={item.text} />
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
