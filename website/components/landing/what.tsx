import { Inline } from '@/components/inline';
import { what } from '@/lib/landing';

/** What Writ is: who talks to it, what it does, and what it alone can reach. */
export function What() {
  const { diagram } = what;
  return (
    <section id="what" aria-labelledby="h-what">
      <div className="wrap">
        <div className="sec-head">
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
                <li key={item}>{item}</li>
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
        <div className="cols3">
          {what.cards.map((card) => (
            <div key={card.title}>
              <h4>{card.title}</h4>
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
