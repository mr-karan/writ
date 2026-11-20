import Link from 'next/link';
import { Inline } from '@/components/inline';
import { security } from '@/lib/landing';
import { Limit } from './limit';
import { SlabRule } from './voxel';

export function Security() {
  return (
    <section id="security" className="security" aria-labelledby="h-security">
      <div className="wrap">
        <SlabRule family="b" seed={6} />
        <div className="sec-head">
          <h2 id="h-security">{security.title}</h2>
          <p className="lede">
            <Inline text={security.lede} />
          </p>
        </div>

        <ol className="sec-layers">
          {security.layers.map((layer) => (
            <li key={layer.num}>
              <span className="sec-layer-num" aria-hidden="true">
                {layer.num}
              </span>
              <div>
                <h3>{layer.title}</h3>
                <p>
                  <Inline text={layer.text} />
                </p>
              </div>
            </li>
          ))}
        </ol>

        <Limit>
          <Inline text={security.limit} />
        </Limit>
        <p className="sec-more">
          <Link className="more" href={security.link.href}>
            {security.link.text}
          </Link>
        </p>
      </div>
    </section>
  );
}
