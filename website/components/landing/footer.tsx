import Link from 'next/link';
import { Wordmark } from '@/components/wordmark';
import { repoUrl, site } from '@/lib/site';

export function Footer() {
  return (
    <footer className="foot-band">
      <div className="wrap foot">
        <div className="foot-left">
          <Link href="/" aria-label="Writ, home" className="brand">
            <Wordmark />
          </Link>
          <p>Apache 2.0 open source. No analytics or cookies.</p>
        </div>
        <nav aria-label="Footer">
          <Link href="/docs">Docs</Link>
          <Link href="/docs/quick-start">Quick start</Link>
          <Link href="/docs/security-model">Security model</Link>
          <Link href="/docs/run-isolation">Run isolation</Link>
          <Link href="/docs/policy-cookbook">Policy cookbook</Link>
          <Link href="/docs/status">Supported platforms</Link>
          <Link href="/docs/compare">Comparison</Link>
          <a href={repoUrl('SECURITY.md')}>Security policy</a>
          <a href={site.repo}>GitHub</a>
          <a href="/llms.txt">llms.txt</a>
        </nav>
      </div>
    </footer>
  );
}
