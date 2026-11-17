'use client';

import { useEffect, useRef, useState, type ReactNode } from 'react';

interface Station {
  name: string;
  idle: ReactNode;
  live: ReactNode;
  text: ReactNode;
}

/**
 * The three steps of a change, with each step's real output beside them.
 * The server renders the finished figure and every output, so a page
 * without script shows all of it. In view, the line fills from step to step
 * and each output takes its turn. A click on a step stops that and shows
 * its output. With reduced motion the figure stays finished.
 */
export function Flow({ stations, outputs, caption, children }: { stations: Station[]; outputs: ReactNode[]; caption: ReactNode; children?: ReactNode }) {
  const ref = useRef<HTMLOListElement>(null);
  const timer = useRef<ReturnType<typeof setInterval> | undefined>(undefined);
  // step 0 is the finished figure: every station done.
  const [step, setStep] = useState(0);
  const [shown, setShown] = useState<number | null>(null);

  useEffect(() => {
    if (matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setShown(1);
      return;
    }
    setStep(1);
    setShown(1);
    const el = ref.current;
    if (!el) return;
    const seen = new IntersectionObserver(
      ([entry]) => {
        if (!entry.isIntersecting) return;
        seen.disconnect();
        timer.current = setInterval(() => {
          setStep((s) => {
            const n = s === 3 ? 1 : s + 1;
            setShown(n);
            return n;
          });
        }, 3200);
      },
      { threshold: 0.5 },
    );
    seen.observe(el);
    return () => {
      seen.disconnect();
      clearInterval(timer.current);
    };
  }, []);

  const pick = (n: number) => {
    clearInterval(timer.current);
    setStep(n);
    setShown(n);
  };

  return (
    <div className="how-grid">
      <div>
        <ol className="flow" ref={ref}>
          {stations.map((st, i) => {
            const n = i + 1;
            const state = step === 0 || n < step ? 'done' : n === step ? 'active' : '';
            return (
              <li key={st.name} className={`station ${state}`}>
                <button type="button" aria-controls={`out-${n}`} aria-pressed={shown === n} onClick={() => pick(n)}>
                  <span className="num">{n}</span>
                  <span className="st-name">
                    {st.name} {state ? st.live : st.idle}
                  </span>
                  <span className="st-text">{st.text}</span>
                </button>
              </li>
            );
          })}
        </ol>
        {children}
      </div>
      <div className="how-out">
        {outputs.map((out, i) => (
          <div key={i} id={`out-${i + 1}`} className="out" hidden={shown !== null && shown !== i + 1}>
            {out}
          </div>
        ))}
        <p className="caption">{caption}</p>
      </div>
    </div>
  );
}
