# apps/web — Next.js

**Status:** 🟡 Scaffold.

Marketing site + admin console. **Self-hosted from day one** — no Vercel-only features (no `next/og` edge tricks, no Vercel KV — use Valkey).

## Rules

- AG-UI 1.0 is the agent↔client contract (`POST /ag-ui/run` → typed SSE event stream).
- All UI strings go through the i18n packs (`locales/`) — Hindi first, English second, more to follow.
- Brand strings come from `brand.config.json` — never hardcode the brand name.
