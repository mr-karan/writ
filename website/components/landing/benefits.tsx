import Link from 'next/link';
import type { ReactNode } from 'react';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { Transcript } from '@/lib/code';
import { benefits } from '@/lib/landing';
import { Limit } from './limit';
import { Shot } from './shot';
import { Term } from './term';

/** One claim: what it is, its limit, where the docs go on, and the evidence beside it. */
function Row({ title, text, limit, link, children }: { title: string; text: string; limit?: string; link: { text: string; href: string }; children: ReactNode }) {
  return (
    <article className="row">
      <div className="row-text">
        <h3>{title}</h3>
        <p>
          <Inline text={text} />
        </p>
        {limit && (
          <Limit>
            <Inline text={limit} />
          </Limit>
        )}
        <Link className="more" href={link.href}>
          {link.text}
        </Link>
      </div>
      <div className="row-visual">{children}</div>
    </article>
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

        <Row {...keys}>
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
        </Row>

        <Row {...secrets}>
          <div className="crop" tabIndex={0} role="region" aria-label="A replaced resource in a plan, screenshot">
            <Shot name="secret" alt={secrets.alt} />
          </div>
        </Row>

        <Row {...drift}>
          <div className="crop" tabIndex={0} role="region" aria-label="A stack's drift check, screenshot">
            <Shot name="drift" alt={drift.alt} />
          </div>
        </Row>

        <Row {...policy}>
          <Term small>
            <Transcript text={landing.policyDeny} wrap label="Output of writ plan on a commit that a policy denies" />
          </Term>
          <p className="caption">
            <Inline text={policy.caption} />
          </p>
        </Row>

        <Row {...agents}>
          <Term small>
            <Transcript text={landing.mcpAdd} wrap label="Output of claude mcp add for writ mcp" />
          </Term>
          <p className="caption">{agents.caption}</p>
        </Row>

        <div className="also">
          {also.map((item) => (
            <div key={item.title}>
              <h3>{item.title}</h3>
              <p>
                <Inline text={item.text} />
              </p>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
