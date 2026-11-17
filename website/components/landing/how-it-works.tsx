import { Inline } from '@/components/inline';
import landing from '@/content/landing.json';
import { Transcript } from '@/lib/code';
import { how } from '@/lib/landing';
import { Flow } from './flow';
import { Limit } from './limit';
import { Term } from './term';

export function HowItWorks() {
  const texts = [landing.plan, `${landing.denied}\n\n${landing.approved}`, landing.apply];
  return (
    <section id="how" aria-labelledby="h-how">
      <div className="wrap">
        <div className="sec-head">
          <h2 id="h-how">{how.title}</h2>
          <p className="lede">{how.lede}</p>
        </div>
        <Flow
          stations={how.steps.map((st) => ({
            name: st.name,
            idle: <span className={`pill ${st.idle.tone}`}>{st.idle.text}</span>,
            live: <span className={`pill ${st.live.tone}`}>{st.live.text}</span>,
            text: <Inline text={st.text} />,
          }))}
          outputs={how.steps.map((st, i) => (
            <Term key={st.name} title={st.output}>
              <Transcript text={texts[i]} wrap={i === 1} label={st.output} prompts={i === 1 ? ['alice', 'bob'] : []} />
            </Term>
          ))}
          caption={<Inline text={how.caption} />}
        >
          <Limit>{how.refusal}</Limit>
        </Flow>
      </div>
    </section>
  );
}
