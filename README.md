# Sutra — India's AI-native super-app

> **Codename: Sutra** (सूत्र — "the thread that connects"). The public brand name is filled in at company registration via [`brand.config.json`](./brand.config.json) — one place, everywhere.

> **One app for how India lives online.** Social feeds, short video, livestreams, messaging & calls, shopping, creator earnings, mini-apps, and your own personal AI — under one identity, built AI-first from day one.

**Status:** 🟡 Rebuilding from scratch (tech v2) · 🟢 Contributors welcome · License: **AGPLv3** · 🟡 Company incorporation in progress

[![Hacktoberfest 2026](https://img.shields.io/badge/🎃_Hacktoberfest-2026-orange?style=for-the-badge)](https://github.com/mintzocloud-coder/sutra/labels/hacktoberfest) — we participate! Grab a [`hacktoberfest`](https://github.com/mintzocloud-coder/sutra/labels/hacktoberfest) issue, newcomers start with [`good first issue`](https://github.com/mintzocloud-coder/sutra/labels/good%20first%20issue).

---

## The equity map — public from day one

Most startups hide their cap table. We publish ours. If you build Sutra or back Sutra, you know exactly where you stand:

| Pool | Share | Who |
|---|---|---|
| **Founder** | **70%** | Harshal Vanpariya |
| **Contributors** | **10%** | Open-source contributors, earned via credits → formal roles |
| **Investors** | **20%** | Early backers (private placement — request the investor book) |
| Infra partners | *carved from founder pool* | Cloud/infra partners (terms per partnership, TBD) |

This is the **map**, not a securities offer — the legal instruments (sweat equity/ESOPs, private placement) execute as the company incorporates. But the pie is fixed and visible from day one. *Jo dikhta hai, vahi bikta hai.*

## Founder

**Sutra is being built by Harshal Vanpariya** — founder, in the open, under his own name. No anonymous team, no faceless brand. When the founder puts his name on it, trust isn't a claim — it's skin in the game.

## Founding Creators — first 10,000

The first **10,000 creators** get something no later creator will:

- **A personal referral code** (first-come — fill the form, we send your code personally)
- **Invite up to 30 creators** with your code
- **Earn 2% lifetime** on every invited creator's income — **paid by the platform, not cut from the creator.** If your invitee earns ₹1,00,000/month, you get ₹2,000/month. Forever.

Bring big creators. Your 30 slots are precious — use them well.

## Creator income — 7 streams, all visible

1. **Brand deals** — direct sponsorships via the creator marketplace, on your rate card
2. **Affiliate** — tag products in Reels, earn on every sale
3. **Live gifts** — Sutra Coins from fans on livestreams
4. **Ad revenue share** — **55% to you**, YouTube-style. Public, fixed, no fine print
5. **Private groups & exclusive content** — paid memberships; your community, your price — **70% to you**
6. **Shoppable content** — your Reel is your storefront
7. **Referral 2% lifetime** — on invited creators' income, paid by the platform

## Tech — v2, rebuilt from scratch

| Layer | Choice |
|---|---|
| Realtime core (chat, presence, live orchestration) | **Elixir / Erlang OTP** — narrow, deliberate; rare talent = harder to copy |
| APIs (marketplace, orders, mini-app runtime) | **Go** |
| AI (recsys, agents, creator tools, moderation) | **Python** |
| Web + admin | **Next.js** (self-hosted) |
| Mobile | **Kotlin Multiplatform** (shared logic + native UI) |
| Database | **PostgreSQL 18 + pgvector** · Cache: **Valkey** |
| AI agent UI | **AG-UI** (protocol, not vendor lock-in) |

**Rule:** no readymade engines for the **core product** — the commerce engine, social, messaging, mini-app runtime are all built custom. Readymade open-source is allowed only for plumbing (auth, video SFU, search) and only if a contributor can run it on a laptop.

## Contribute — code, bounties, equity

### 🎃 Hacktoberfest 2026 — we participate!

October is the best month to join Sutra. Issues labeled [`hacktoberfest`](https://github.com/mintzocloud-coder/sutra/labels/hacktoberfest) are Hacktoberfest-eligible; if you're new, start with [`good first issue`](https://github.com/mintzocloud-coder/sutra/labels/good%20first%20issue). Comment to claim → get assigned within 24h → open your PR. Merged PRs count for Hacktoberfest **and** earn contributor credits toward the 10% equity pool.

Read **[CONTRIBUTING.md](./CONTRIBUTING.md)**. The short version:

- **Bounties** — real INR for scoped issues
- **Public leaderboard** — every merged PR counts, transparently
- **10% contributor equity pool** — credits earn your way toward a formal role, which is where sweat equity / ESOPs live. Small slices, real and written down.
- **CLA** — one signature before your first merge ([CLA.md](./CLA.md)), so the project can dual-license forever

## License

Public code is licensed under **AGPLv3** ([LICENSE](./LICENSE)) — anyone who runs a modified version as a service must publish their changes. A commercial license is available for those who need it. Brand, UX specs, and production configs stay private.

## Roadmap (engineering waves)

> All 30 modules in scope. Waves are integration order, not scope cuts.

- **Wave 1 — Foundation:** identity, social feeds, short video, messaging (Elixir core), design system
- **Wave 2 — AI core:** personal AI, creator studio, language bridge, recommendations
- **Wave 3 — Commerce:** custom marketplace engine, seller center, UPI-first payments, logistics
- **Wave 4 — Ecosystem:** mini-app runtime, developer platform, creator marketplace, live shopping
- **Wave 5 — Business:** business AI, integrations, advertising, subscriptions

## Contact

- Code & discussions: right here on GitHub
- Founding creator form: *(link to be announced)*
- Investors: request the investor book *(contact to be announced)*
- Security reports: *(policy to be announced)*

---

*Sutra is the development codename of an independent project by Harshal Vanpariya. Design and engineering in progress; figures are planning targets, not promises. Nothing here is an offer of securities.*
