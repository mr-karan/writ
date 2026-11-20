import { defineConfig, defineDocs } from 'fumadocs-mdx/config';
import { regoLanguage, sessionLanguage, writThemes } from './lib/highlight';

export const docs = defineDocs({
  dir: 'content/docs',
});

export default defineConfig({
  mdxOptions: {
    rehypeCodeOptions: {
      themes: writThemes,
      langs: [sessionLanguage, regoLanguage, 'json', 'go', 'hcl', 'sh', 'diff', 'yaml'],
      // The docs call a transcript `console`; `session` is the grammar for it.
      langAlias: { console: 'session' },
      fallbackLanguage: 'text',
    },
  },
});
