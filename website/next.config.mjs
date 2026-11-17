import { createMDX } from 'fumadocs-mdx/next';

/** @type {import('next').NextConfig} */
const config = {
  // A static export: plain files that any web server or object store can host.
  output: 'export',
  trailingSlash: true,
  reactStrictMode: true,
  images: { unoptimized: true },
  // `next dev` would otherwise write an AGENTS.md into this directory.
  agentRules: false,
};

export default createMDX()(config);
