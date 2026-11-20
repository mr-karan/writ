import Link from 'next/link';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { compare, trust, whenToUse } from '@/lib/landing';
import { Caret, Limit } from './limit';
import { SlabRule } from './voxel';

/** Honest "when to use Writ", Writ against two paid control planes, then limits and what you can check. */
export function Compare() {
  return (
    <section id="compare" className="compare" aria-labelledby="h-compare">
      <div className="wrap">
        <SlabRule family="i" seed={5} />
        <div className="sec-head">
          <h2 id="h-when">{whenToUse.title}</h2>
          <p className="lede">{whenToUse.lede}</p>
        </div>

        <div className="when-grid">
          <div className="when-col when-writ">
            <h3>{whenToUse.writ.title}</h3>
            <ul>
              {whenToUse.writ.items.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          </div>
          <div className="when-col">
            <h3>{whenToUse.plainCi.title}</h3>
            <ul>
              {whenToUse.plainCi.items.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          </div>
        </div>

        <div className="sec-head compare-subhead">
          <h2 id="h-compare">{compare.title}</h2>
          <p className="lede">{compare.lede}</p>
        </div>

        <table className="cmp">
          <thead>
            <tr>
              <td />
              {compare.columns.map((c) => (
                <th scope="col" key={c}>
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {compare.rows.map(([label, ...cells]) => (
              <tr key={label}>
                <th scope="row">{label}</th>
                {cells.map((cell, i) => (
                  <td key={compare.columns[i]} data-col={compare.columns[i]}>
                    <span>
                      <Inline text={cell} />
                    </span>
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
        <Limit>{compare.missing}</Limit>
        <p className="caption">
          <Inline text={compare.caption} />
        </p>
        <Link className="more" href={compare.gitlab.href}>
          <Inline text={compare.gitlab.text} />
        </Link>

        <div className="two">
          <div className="notyet" id="notyet">
            <h3>{trust.notYet.title}</h3>
            <ul>
              {landing.limits.map((item) => (
                <li key={item.lead}>
                  <Caret />
                  <span>
                    <b>{item.lead}</b> <Inline text={item.text} />
                  </span>
                </li>
              ))}
            </ul>
            <Link className="more" href={trust.notYet.link.href}>
              {trust.notYet.link.text}
            </Link>
          </div>
          <div className="check">
            <h3>{trust.title}</h3>
            <ul>
              {trust.items.map((item) => (
                <li key={item.title}>
                  <b>{item.title}</b> <Inline text={item.text} /> <Link href={item.link.href}>{item.link.text}</Link>
                </li>
              ))}
            </ul>
          </div>
        </div>
      </div>
    </section>
  );
}
