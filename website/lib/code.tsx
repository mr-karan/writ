// A server component that sets terminal output. It uses the same grammar and
// theme as the docs, so nothing is highlighted in the browser.

import { getHighlighter } from 'fumadocs-core/highlight';
import { sessionLanguage, writThemes } from './highlight';

interface Token {
  content: string;
  color?: string;
  bold: boolean;
}

const plain = 'var(--syn-fg)';
const highlighter = getHighlighter('js', {
  langs: [sessionLanguage],
  themes: [writThemes.light],
});

async function tokenize(code: string, lang: string): Promise<Token[][]> {
  const shiki = await highlighter;
  const lines = shiki.codeToTokensBase(code, { lang: lang as never, theme: writThemes.light.name as never });
  return lines.map((line) =>
    line.map((t) => ({ content: t.content, color: t.color === plain ? undefined : t.color, bold: ((t.fontStyle ?? 0) & 2) !== 0 })),
  );
}

function Tokens({ tokens }: { tokens: Token[] }) {
  return tokens.map((t, i) =>
    t.color || t.bold ? (
      <span key={i} style={{ color: t.color, fontWeight: t.bold ? 650 : undefined }}>
        {t.content}
      </span>
    ) : (
      t.content
    ),
  );
}

export interface TranscriptProps {
  /** A captured transcript: "$ command" lines, then the command's real output. */
  text: string;
  /** Wrap long lines, as a terminal does, where the end of a line carries the message. */
  wrap?: boolean;
  label?: string;
  /** Who ran each "$ " command, in order, shown before its prompt. */
  prompts?: string[];
}

/** Terminal output exactly as writ printed it. */
export async function Transcript({ text, wrap = false, label = 'Terminal output', prompts = [] }: TranscriptProps) {
  const body = text.split('\n');
  const lines = await tokenize(text, 'session');
  let command = 0;

  return (
    <figure className={wrap ? 'transcript is-wrapped' : 'transcript'}>
      <pre tabIndex={0} role="group" aria-label={label}>
        <code>
          {lines.map((tokens, i) => (
            <span key={i} className="tl">
              {body[i].startsWith('$ ') && prompts[command] && <span className="who">{prompts[command++]} </span>}
              <Tokens tokens={tokens} />
            </span>
          ))}
        </code>
      </pre>
    </figure>
  );
}
