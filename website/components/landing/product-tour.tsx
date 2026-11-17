import { Inline } from '@/components/inline';
import { tour } from '@/lib/landing';
import { Frame, Shot, shotDate, shotPaths } from './shot';

/** Four screenshots of the control UI behind tabs: radio inputs and CSS, so it works without script. */
export function ProductTour() {
  return (
    <section id="product" className="tour" aria-labelledby="h-product">
      <div className="wrap center">
        <div className="sec-head center">
          <h2 id="h-product">{tour.title}</h2>
          <p className="lede">
            <Inline text={tour.lede} />
          </p>
        </div>
        <div className="tabs" role="radiogroup" aria-label="Control UI pages">
          {tour.tabs.map((tab, i) => (
            <span key={tab.id}>
              <input type="radio" name="tour" id={`tour-${tab.id}`} defaultChecked={i === 0} />
              <label htmlFor={`tour-${tab.id}`}>{tab.label}</label>
            </span>
          ))}
        </div>
        {tour.tabs.map((tab) => (
          <div key={tab.id} className={`tour-panel tour-${tab.id}`}>
            <p className="tour-cap">{tab.caption}</p>
            <Frame path={shotPaths[tab.id]}>
              <div className="view" tabIndex={0} role="region" aria-label={`${tab.label}, screenshot`}>
                <Shot name={tab.id} alt={tab.alt} />
              </div>
            </Frame>
          </div>
        ))}
        <p className="limit">
          <Inline text={tour.limit} />
        </p>
        <p className="caption">Screenshots of the control UI against a dev control plane with demo stacks, {shotDate}.</p>
      </div>
    </section>
  );
}
