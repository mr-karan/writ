import { Benefits } from '@/components/landing/benefits';
import { Compare } from '@/components/landing/compare';
import { Footer } from '@/components/landing/footer';
import { Header } from '@/components/landing/header';
import { Hero } from '@/components/landing/hero';
import { HowItWorks } from '@/components/landing/how-it-works';
import { PolicySection } from '@/components/landing/policy-section';
import { ProductTour } from '@/components/landing/product-tour';
import { Questions } from '@/components/landing/questions';
import { Security } from '@/components/landing/security';
import { TryIt } from '@/components/landing/try-it';
import { What } from '@/components/landing/what';

export default function Home() {
  return (
    <div className="landing">
      <Header />
      <main id="main-content">
        <Hero />
        <What />
        <HowItWorks />
        <Security />
        <PolicySection />
        <ProductTour />
        <Benefits />
        <Compare />
        <Questions />
        <TryIt />
      </main>
      <Footer />
    </div>
  );
}
