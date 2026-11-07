import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { BASE, GUIDE_PAGES, REPO_URL, SITE, slugOf } from './scripts/sync-docs.mjs';

export default defineConfig({
  site: SITE,
  base: BASE,
  trailingSlash: 'always',
  integrations: [
    starlight({
      title: 'CodeFabric',
      description: 'Run coding agents across a pool of your own machines.',
      components: { Search: './src/components/Search.astro' },
      customCss: ['./src/styles/fonts.css', './src/styles/theme.css'],
      expressiveCode: { defaultProps: { wrap: true, preserveIndent: true }, styleOverrides: { borderRadius: '0', codeFontFamily: 'var(--sl-font-mono)' } },
      social: [{ icon: 'github', label: 'GitHub', href: REPO_URL }],
      sidebar: [{ label: 'Guide', items: GUIDE_PAGES.map((file) => ({ slug: slugOf(file) })) }],
    }),
  ],
});
