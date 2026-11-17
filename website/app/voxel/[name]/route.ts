import { pictures, pictureSvg } from '@/lib/voxel/scenes';

// Each picture is one static SVG file, built with the site. The page draws
// it with <use>, so the document doesn't carry it and a browser caches it.
export const dynamic = 'force-static';

export function generateStaticParams() {
  return Object.keys(pictures).map((name) => ({ name: `${name}.svg` }));
}

export async function GET(_request: Request, { params }: { params: Promise<{ name: string }> }) {
  const name = (await params).name.replace(/\.svg$/, '');
  if (!(name in pictures)) return new Response('Not found', { status: 404 });
  return new Response(pictureSvg(pictures[name as keyof typeof pictures]), { headers: { 'content-type': 'image/svg+xml' } });
}
