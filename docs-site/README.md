<!-- SPDX-License-Identifier: MIT -->

# Sutra developer docs site (Docusaurus skeleton)

**Licensing split, deliberate:** the *skeleton* (config, theme plumbing,
build scripts, `package.json`) is **MIT** — reuse it anywhere. The *guide
content* under `docs/` carries the repo's `AGPL-3.0-or-later` SPDX header,
like every other doc in `../docs/`.

## Run it

```bash
cd docs-site
npm install
npm start        # dev server at http://localhost:3000
npm run build    # static output in build/
```

## Structure

- `docusaurus.config.js` — site config; update `url`/`baseUrl` before deploy
- `sidebars.js` — the Guides sidebar
- `docs/` — guide content (AGPL):
  - `intro.md` — what the platform is
  - `onboarding.md` — **the** developer onboarding guide: app keys,
    first API call, webhooks, mini-app submission checklist
  - `webhooks.md` / `mini-apps.md` — surface summaries; the deep contracts
    stay in the repo root (`../docs/webhooks.md`, `../docs/mini-apps.md`)
    so they can't drift between two copies
- `src/pages/index.js` — the landing page

The REST reference is `../docs/openapi.yaml` — the single source of truth,
coverage-gated in CI (`scripts/check_openapi_coverage.py`). When this site
goes live, add a Redoc/Swagger-UI page rendering it.
