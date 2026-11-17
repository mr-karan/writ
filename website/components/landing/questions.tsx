import { Inline } from '@/components/inline';
import { questions } from '@/lib/landing';

export function Questions() {
  return (
    <section id="questions" aria-labelledby="h-questions">
      <div className="wrap faq">
        <h2 id="h-questions">Questions</h2>
        <div>
          {questions.map(({ q, a }) => (
            <details key={q}>
              <summary>{q}</summary>
              <p>
                <Inline text={a} />
              </p>
            </details>
          ))}
        </div>
      </div>
    </section>
  );
}
