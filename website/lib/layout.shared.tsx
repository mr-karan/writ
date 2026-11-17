import type { BaseLayoutProps } from 'fumadocs-ui/layouts/shared';
import { Wordmark } from '@/components/wordmark';
import { site } from '@/lib/site';

export function baseOptions(): BaseLayoutProps {
  return {
    nav: { title: <Wordmark className="py-1.5" />, url: '/' },
    githubUrl: site.repo,
  };
}
