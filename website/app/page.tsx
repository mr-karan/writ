import { Features } from '@/components/landing/features';
import { Footer } from '@/components/landing/footer';
import { Header } from '@/components/landing/header';
import { Hero } from '@/components/landing/hero';
import { Showcase } from '@/components/landing/showcase';
import { TryIt } from '@/components/landing/try-it';
import { Workflow } from '@/components/landing/workflow';

export default function Home() {
  return (
    <div className="landing">
      <Header />
      <main id="main-content">
        <Hero />
        <Workflow />
        <Features />
        <Showcase />
        <TryIt />
      </main>
      <Footer />
    </div>
  );
}
