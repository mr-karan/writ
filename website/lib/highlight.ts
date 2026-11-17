// The grammar for terminal transcripts and the one syntax theme. Fumadocs
// highlights the docs with them when the site builds, and the landing page
// uses the same two, so code looks the same everywhere.
//
// The theme's colors are CSS variables (--syn-*, set in app/global.css), so
// light and dark share one theme and the palette lives in one place.

import type { LanguageRegistration, ThemeRegistration } from 'shiki';

// A terminal transcript: "$ command" lines and the output of writ and tofu.
export const sessionLanguage: LanguageRegistration = {
  name: 'session',
  scopeName: 'text.session.writ',
  repository: {},
  patterns: [
    {
      match: '^(\\$)\\s+(.*)$',
      captures: { 1: { name: 'punctuation.prompt.session' }, 2: { name: 'entity.name.session' } },
    },
    { name: 'markup.changed.session', match: '^warning(\\[[a-z-]+\\])?' },
    { name: 'markup.inserted.session', match: '^ok:' },
    { name: 'markup.changed.session', match: '^(\\+/-|-/\\+|~)(?=\\s)' },
    { name: 'markup.inserted.session', match: '^\\+(?=\\s)' },
    { name: 'markup.deleted.session', match: '^[-!](?=\\s)' },
    { name: 'keyword.session', match: '^>(?=\\s)' },
    { name: 'markup.bold.session', match: '^Plan:.*$' },
    { name: 'markup.bold.session', match: '^Policy:.*$' },
    { name: 'markup.deleted.session', match: '^\\s+deny:' },
    { name: 'markup.changed.session', match: '^\\s+warn:' },
    { name: 'markup.bold.session', match: '^(Applied|Imported) .*$' },
  ],
};

const tokenColors: ThemeRegistration['tokenColors'] = [
  { scope: ['comment', 'punctuation.prompt'], settings: { foreground: 'var(--syn-comment)' } },
  { scope: ['keyword', 'storage'], settings: { foreground: 'var(--syn-keyword)' } },
  { scope: ['keyword.operator', 'punctuation'], settings: { foreground: 'var(--syn-punct)' } },
  { scope: ['string', 'string punctuation'], settings: { foreground: 'var(--syn-string)' } },
  { scope: ['meta.interpolation', 'meta.embedded'], settings: { foreground: 'var(--syn-fg)' } },
  { scope: ['punctuation.section.interpolation', 'constant.character.escape'], settings: { foreground: 'var(--syn-number)' } },
  { scope: ['constant.numeric', 'constant.language', 'constant'], settings: { foreground: 'var(--syn-number)' } },
  { scope: ['entity.name.type', 'support.type', 'support.class', 'entity.name.tag'], settings: { foreground: 'var(--syn-type)' } },
  { scope: ['support.function', 'entity.name.function'], settings: { foreground: 'var(--syn-function)' } },
  { scope: ['entity.name', 'entity.name.session'], settings: { foreground: 'var(--syn-name)' } },
  { scope: ['variable.other.property', 'support.type.property-name', 'meta.object-literal.key'], settings: { foreground: 'var(--syn-fg)' } },
  { scope: ['markup.inserted'], settings: { foreground: 'var(--syn-add)' } },
  { scope: ['markup.deleted', 'invalid'], settings: { foreground: 'var(--syn-remove)' } },
  { scope: ['markup.changed'], settings: { foreground: 'var(--syn-change)' } },
  { scope: ['markup.bold'], settings: { foreground: 'var(--syn-name)', fontStyle: 'bold' } },
  { scope: ['markup.underline.link'], settings: { foreground: 'var(--syn-comment)' } },
];

function theme(name: string, type: 'light' | 'dark'): ThemeRegistration {
  return {
    name,
    type,
    colors: { 'editor.foreground': 'var(--syn-fg)', 'editor.background': 'var(--syn-bg)' },
    tokenColors,
  };
}

export const writThemes = { light: theme('writ-light', 'light'), dark: theme('writ-dark', 'dark') };
