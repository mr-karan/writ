import Link from 'next/link';
import { repoUrl, site } from '@/lib/site';
import { Voxel } from './voxel';

export function Footer() {
  return (
    <footer className="foot-band">
      <div className="foot-art" aria-hidden="true">
        <span className="foot-mark">Writ</span>
        <Voxel name="foot" />
      </div>
      <div className="wrap foot">
        <p>Writ is open source under the Apache 2.0 license. This site has no analytics and no cookies.</p>
        <nav aria-label="Footer">
          <Link href="/docs">Docs</Link>
          <Link href="/docs/quick-start">Quick start</Link>
          <Link href="/docs/security-model">Security model</Link>
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
