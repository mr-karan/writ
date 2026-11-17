import Link from 'next/link';
import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { compare, trust } from '@/lib/landing';
import { Caret, Limit } from './limit';
import { SlabRule } from './voxel';

/** Writ against two paid control planes, then what Writ lacks and what a reader can check. */
export function Compare() {
  return (
    <section id="compare" className="compare" aria-labelledby="h-compare">
      <div className="wrap">
        <SlabRule family="i" seed={5} />
        <div className="sec-head">
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
                    {cell}
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
