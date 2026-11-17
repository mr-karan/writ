import { DocsBody, DocsPage, DocsTitle } from 'fumadocs-ui/layouts/docs/page';
import type { Metadata } from 'next';
import { notFound } from 'next/navigation';
import { getMDXComponents } from '@/components/mdx';
import { pages } from '@/content.manifest';
import { repoUrl } from '@/lib/site';
import { source } from '@/lib/source';

interface Props {
  params: Promise<{ slug?: string[] }>;
}

/** The repository files a page was cut from, in the order they appear on it. */
function sourcesOf(slug: string[]): string[] {
  const key = slug.join('/');
  const page = pages.find((p) => p.slug === key || p.slug === (key === '' ? 'index' : `${key}/index`));
  const files = page?.parts.flatMap((part) => ('file' in part ? [part.file] : [])) ?? [];
  return [...new Set(files)];
}

export default async function Page({ params }: Props) {
  const { slug = [] } = await params;
  const page = source.getPage(slug);
  if (!page) notFound();

  const MDX = page.data.body;
  const files = sourcesOf(slug);
  const raw = `/raw/${slug.length === 0 ? 'index' : slug.join('/')}.md`;

  return (
    <DocsPage toc={page.data.toc}>
      <DocsTitle id="main-content" tabIndex={-1} className="scroll-mt-24 outline-none">{page.data.title}</DocsTitle>
      <DocsBody>
        <MDX components={getMDXComponents()} />
      </DocsBody>
      <p className="mt-10 border-t border-rule pt-4 text-sm text-pretty text-ink-2">
        This page is built from{' '}
        {files.map((file, i) => (
          <span key={file}>
            {i > 0 && (i === files.length - 1 ? ' and ' : ', ')}
            <a className="underline decoration-rule-strong underline-offset-4 hover:text-ink" href={repoUrl(file)}>
              <code>{file}</code>
            </a>
          </span>
        ))}{' '}
        in the repository. It is also served as{' '}
        <a className="underline decoration-rule-strong underline-offset-4 hover:text-ink" href={raw}>
          plain Markdown
        </a>
        .
      </p>
    </DocsPage>
  );
}

export function generateStaticParams() {
  return source.generateParams();
}

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { slug = [] } = await params;
  const page = source.getPage(slug);
  if (!page) notFound();
  return { title: page.data.title, description: page.data.description };
}
