import Link from 'next/link';
import { Wordmark } from '@/components/wordmark';

export default function NotFound() {
  return (
    <main id="main-content" className="landing flex flex-1 flex-col">
      <div className="wrap flex flex-1 flex-col justify-center py-24">
        <Link href="/" aria-label="Writ, home" className="brand mb-10 self-start py-2">
          <Wordmark />
        </Link>
        <h1>This page is not in the plan.</h1>
        <p className="lede mt-6 max-w-[36rem]">Nothing lives at this address. The docs have a search, and the home page has the short version.</p>
        <div className="mt-8 flex flex-wrap gap-3">
          <Link href="/docs" className="btn btn-primary">
            Open the docs
          </Link>
          <Link href="/" className="btn btn-ghost">
            Go home
          </Link>
        </div>
      </div>
    </main>
  );
}
