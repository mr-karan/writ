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
          <a href="#what">What it is</a>
          <a href="#how">How it works</a>
          <a href="#product">Product</a>
          <a href="#compare">Compare</a>
          <Link href="/docs">Docs</Link>
        </nav>
        <div className="nav-right">
          <HeaderControls />
          <a className="btn btn-ghost btn-sm gh" href={site.repo}>
            GitHub
          </a>
          <a className="btn btn-primary btn-sm" href="#try">
            Get started
          </a>
        </div>
      </div>
    </header>
  );
}
