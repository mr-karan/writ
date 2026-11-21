import Link from 'next/link';
import { HeaderControls } from '@/components/header-controls';
import { Wordmark } from '@/components/wordmark';
import { site } from '@/lib/site';

export function Header() {
  return (
    <header className="nav">
      <div className="wrap nav-in">
        <Link href="/" aria-label="Writ, home" className="brand">
          <Wordmark />
        </Link>
        <nav aria-label="Sections" className="nav-links">
          <a href="#how">How it works</a>
          <a href="#features">Features</a>
          <a href="#policy">Policy</a>
          <Link href="/docs">Docs</Link>
          <a href={site.repo}>GitHub</a>
        </nav>
        <div className="nav-right">
          <HeaderControls />
          <Link className="btn btn-primary btn-sm" href="/docs/quick-start">
            Quick start
          </Link>
        </div>
      </div>
    </header>
  );
}
