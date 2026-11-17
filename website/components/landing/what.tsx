import { Inline } from '@/components/inline';
import { what } from '@/lib/landing';
import { Icon } from './icons';

/** What Writ is: who talks to it, what it does, and what it alone can reach. */
export function What() {
  const { diagram } = what;
  return (
    <section id="what" className="what" aria-labelledby="h-what">
      <div className="wrap">
        <div className="sec-head center">
          <h2 id="h-what">{what.title}</h2>
          <p className="lede">
            <Inline text={what.lede} />
          </p>
        </div>

        <div className="map" role="img" aria-label={diagram.alt}>
          <div className="map-side">
            {diagram.left.map((node) => (
              <div className="node" key={node.name}>
                <b>{node.name}</b>
                <span>{node.text}</span>
              </div>
            ))}
          </div>
          <div className="map-arrow" aria-hidden="true" />
          <div className="node node-writ">
            <b>{diagram.center.name}</b>
            <ul>
              {diagram.center.items.map((item) => (
                <li key={item}>
                  <Icon name="check" size={15} />
                  {item}
                </li>
              ))}
            </ul>
          </div>
          <div className="map-arrow" aria-hidden="true" />
          <div className="map-side">
            <div className="node">
              <b>{diagram.right.name}</b>
              <span>{diagram.right.text}</span>
            </div>
          </div>
        </div>

        <h3 className="sub">{what.without}</h3>
        <div className="cards3">
          {what.cards.map((card) => (
            <div className="card" key={card.title}>
              <div className="ico">
                <Icon name={card.icon} />
              </div>
              <h3>{card.title}</h3>
              <p>
                <Inline text={card.text} />
              </p>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
