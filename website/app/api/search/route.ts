import { createFromSource } from 'fumadocs-core/search/server';
import { source } from '@/lib/source';

// The search index is one static file, built with the site. Queries run in
// the browser, so the export needs no server.
export const revalidate = false;
export const { staticGET: GET } = createFromSource(source);
