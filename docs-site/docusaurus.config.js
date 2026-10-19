// SPDX-License-Identifier: MIT
// @ts-check
// Sutra developer docs site (Docusaurus). This skeleton (config, theme
// plumbing, build scripts) is MIT-licensed so anyone can reuse it; the
// guide content under docs/ carries the repo's AGPL-3.0-or-later header
// like every other doc in ../docs/.

const { themes } = require("prism-react-renderer");

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: "Sutra Developers",
  tagline: "Build mini-apps, webhooks, and API clients on Sutra",
  favicon: "img/favicon.ico",

  // Update these before the first real deploy (GitHub Pages or your host).
  url: "https://developers.sutra.example",
  baseUrl: "/",

  organizationName: "mintzocloud-coder",
  projectName: "sutra",

  onBrokenLinks: "throw",
  onBrokenMarkdownLinks: "throw",

  i18n: { defaultLocale: "en", locales: ["en"] },

  presets: [
    [
      "classic",
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          sidebarPath: require.resolve("./sidebars.js"),
          routeBasePath: "/",
          // SSOT note: the API reference is docs/openapi.yaml in the repo
          // root, linted + coverage-gated in CI (see docs/dev-platform.md).
          // When this site goes live, add a redoc/swagger-ui page for it here.
        },
        blog: false,
        theme: { customCss: require.resolve("./src/css/custom.css") },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      navbar: {
        title: "Sutra Developers",
        items: [
          {
            type: "docSidebar",
            sidebarId: "guidesSidebar",
            position: "left",
            label: "Guides",
          },
          {
            href: "https://github.com/mintzocloud-coder/sutra/blob/main/docs/openapi.yaml",
            label: "OpenAPI spec",
            position: "right",
          },
          {
            href: "https://github.com/mintzocloud-coder/sutra",
            label: "GitHub",
            position: "right",
          },
        ],
      },
      footer: {
        style: "dark",
        links: [
          {
            title: "Platform",
            items: [
              { label: "Onboarding", to: "/onboarding" },
              { label: "Webhooks", to: "/webhooks" },
              { label: "Mini-apps", to: "/mini-apps" },
            ],
          },
          {
            title: "Repo",
            items: [
              {
                label: "openapi.yaml (source of truth)",
                href: "https://github.com/mintzocloud-coder/sutra/blob/main/docs/openapi.yaml",
              },
              {
                label: "Production runbook",
                href: "https://github.com/mintzocloud-coder/sutra/blob/main/docs/production.md",
              },
            ],
          },
        ],
        copyright: `Sutra developer docs skeleton is MIT-licensed; guide content is AGPL-3.0-or-later.`,
      },
      prism: { theme: themes.github, darkTheme: themes.dracula },
    }),
};

module.exports = config;
