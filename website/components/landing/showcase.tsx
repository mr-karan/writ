import Link from 'next/link';
import landing from '@/content/landing.json';
import { CodeBlock, Transcript } from '@/lib/code';
import { showcase } from '@/lib/landing';
import { Frame, Shot, shotPaths } from './shot';
import { Term } from './term';

export function Showcase() {
  return (
    <div className="showcases">
      <section className="sec" aria-labelledby="h-review">
        <div className="wrap">
          <div className="sec-center">
            <h2 id="h-review">{showcase.review.title}</h2>
            <p className="sec-body">{showcase.review.text}</p>
            <Link className="sec-link" href={showcase.review.link.href}>
              {showcase.review.link.text} <span aria-hidden="true">→</span>
            </Link>
          </div>
          <Frame path={shotPaths.secret}>
            <div className="crop crop-secret" tabIndex={0} role="region" aria-label="Masked secret in a plan diff, screenshot">
              <Shot name="secret" mobileName="plan" alt={showcase.review.alt} />
            </div>
          </Frame>
        </div>
      </section>

      <section id="policy" className="sec" aria-labelledby="h-policy">
        <div className="wrap split split-visual-right">
          <div className="split-copy">
            <h2 id="h-policy">{showcase.policy.title}</h2>
            <p>{showcase.policy.text}</p>
            <Link className="sec-link" href={showcase.policy.link.href}>
              {showcase.policy.link.text} <span aria-hidden="true">→</span>
            </Link>
          </div>
          <div className="split-visual stack-gap">
            <Term title={showcase.policy.regoTitle} small>
              <CodeBlock
                code={landing.policyRego.split('\n\nwarn_file_permission')[0]}
                lang="rego"
                wrap
                label="Rego policy rule in examples/policy/tls.rego"
              />
            </Term>
            <Term title={showcase.policy.denyTitle} small>
              <Transcript text={landing.policyDeny} wrap label="Output of writ plan --remote when a policy denies" />
            </Term>
          </div>
        </div>
      </section>

      <section className="sec" aria-labelledby="h-drift">
        <div className="wrap">
          <div className="sec-center">
            <h2 id="h-drift">{showcase.drift.title}</h2>
            <p className="sec-body">{showcase.drift.text}</p>
            <div className="link-row link-row-center">
              {showcase.drift.links.map((link) => (
                <Link key={link.href} className="sec-link" href={link.href}>
                  {link.text} <span aria-hidden="true">→</span>
                </Link>
              ))}
            </div>
          </div>
          <Frame path={shotPaths.drift}>
            <div className="crop crop-drift" tabIndex={0} role="region" aria-label="Drift table in the control UI, screenshot">
              <Shot name="drift" mobileName="stack" alt={showcase.drift.alt} />
            </div>
          </Frame>
        </div>
      </section>
    </div>
  );
}
