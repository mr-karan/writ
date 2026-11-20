import Link from 'next/link';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { CodeBlock, Transcript } from '@/lib/code';
import { policySection } from '@/lib/landing';
import { Limit } from './limit';
import { Term } from './term';
import { SlabRule } from './voxel';

export function PolicySection() {
  return (
    <section id="policy" className="policy-sec" aria-labelledby="h-policy">
      <div className="wrap">
        <SlabRule family="o" seed={8} />
        <div className="sec-head">
          <h2 id="h-policy">{policySection.title}</h2>
          <p className="lede">
            <Inline text={policySection.lede} />
          </p>
        </div>

        <div className="policy-grid">
          <div className="policy-verdicts">
            <dl className="verdict-list">
              {policySection.verdicts.map((v) => (
                <div key={v.title}>
                  <dt>{v.title}</dt>
                  <dd>
                    <Inline text={v.text} />
                  </dd>
                </div>
              ))}
            </dl>
            <Limit>
              <Inline text={policySection.limit} />
            </Limit>
            <div className="policy-links">
              {policySection.links.map((link) => (
                <Link key={link.href} className="more" href={link.href}>
                  {link.text}
                </Link>
              ))}
            </div>
          </div>

          <div className="stack-gap">
            <div>
              <Term title={policySection.regoTitle} small>
                <CodeBlock code={landing.policyRego} lang="rego" label="Rego policy rule in examples/policy/tls.rego" />
              </Term>
              <p className="caption">
                <Inline text={policySection.regoCaption} />
              </p>
            </div>
            <div>
              <Term title={policySection.outputTitle} small>
                <Transcript text={landing.policyFullPlan} wrap label="Output of writ plan --remote when a policy denies" />
              </Term>
              <p className="caption">
                <Inline text={policySection.outputCaption} />
              </p>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}
