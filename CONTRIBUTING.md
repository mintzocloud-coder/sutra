# Contributing to Sutra

Welcome, builder. 🇮🇳 Sutra is India's AI-native super-app — and **10% of the company is reserved for the people who build it.** This document explains exactly how you earn your slice. No hype, no fine print surprises.

---

## The deal, in one paragraph

Sutra's equity map is public: **Founder 70% · Contributors 10% · Investors 20%.** The contributor pool is real and fixed. You earn **credits** for merged work → top contributors are invited into **formal roles** (advisor/contractor/team) → formal roles receive **sweat equity / ESOPs** from the pool, with vesting. Small slices, real and written down — materializing as the company runs. The company is incorporating now; the pool formalizes with it.

---

## 1. Ways to contribute

- **Code** — Go APIs, Elixir realtime core, Python AI, Next.js web, KMP mobile. `good-first-issue` labels mark the on-ramps. (Elixir core is deliberately narrow — serious contributors only.)
- **Design & UX** — flows, regional-language UX, accessibility, low-bandwidth experiences.
- **Docs & translations** — Hindi, Gujarati, Tamil, Bengali, Telugu… India speaks many languages; Sutra should too.
- **Testing** — especially low-end Android devices and slow networks.
- **Ideas** — well-argued RFCs in Discussions. Best ideas get specced and bountied.

## 2. Finding work & claiming it — the no-conflict rule

**Where to look:** [open issues with no assignee](https://github.com/mintzocloud-coder/sutra/issues?q=is%3Aissue+is%3Aopen+no%3Aassignee) — **no assignee = available**, that's the whole list. (Filter further by label: `good first issue`, `backend`, `documentation`.)

**How to claim (4 steps):**
1. Pick an issue from **Available** that has **no assignee**.
2. Comment on it: "I want to work on this" + 2-3 lines on your approach.
3. A maintainer assigns it to you within 24 hours. **Only start after you're assigned.**
4. Open your PR with "Closes #N" in the description.

**The no-conflict rule:** one issue = **one assignee**. If an issue already shows an assignee, it's taken — pick another. Never start work on an assigned issue without talking to the assignee first.

**Stale claims:** no update for **7 days** → the issue is unassigned and returns to Available. (Bountied issues: same rule — first credible claim gets 7 days.)

**Why conflicts are rare by design:** the monorepo is split by service (`services/chat`, `services/api`, `services/ai`, `apps/*`) and **no service writes another service's tables** (see ARCHITECTURE.md). Big work is split into small issues, so two people almost never need the same file.

## 3. Bounties — cash now (INR)

Selected issues carry a **₹ bounty**. The amount is written on the issue — no negotiation after merge. Claim by commenting; first credible claim gets 7 days, then it reopens. Paid within 14 days of merge via UPI/bank transfer. *(Initial bounty budget to be announced.)*

## 4. Credits & leaderboard — reputation that compounds

| Contribution | Credits |
|---|---|
| Merged PR (code) | 10–100 by size & difficulty |
| Bounty completion | 2× base credits |
| Accepted RFC / design | 25 |
| Translation milestone (per language pack) | 30 |
| Critical bug report (confirmed) | 15 |

- **Public leaderboard**, updated on every merge. No hidden scoring.
- Credits never expire, tied to your GitHub identity.
- Credits are **recognition + your path into the 10% pool** — they are not shares by themselves. (Indian law doesn't allow "PR = shares." The pool flows through formal roles — that's the compliant road, written down here instead of whispered.)

## 5. The equity path — step by step

1. **Contribute** → earn credits on the public leaderboard.
2. **Get noticed** → consistent top contributors are invited to a formal role (technical advisor, contractor, or team member) — **a future job is genuinely possible** for the best.
3. **Earn equity** → formal roles receive sweat equity / ESOPs from the 10% contributor pool, with standard vesting and board approval.

We will never announce equity before the paperwork exists. The map is public; the instruments follow incorporation.

## 6. License & CLA — read this before your first PR

- Public code is **AGPLv3**. If you run a modified version as a service, you publish your changes. That's the deal.
- Before your first merged PR, sign the **CLA** ([CLA.md](./CLA.md)): you keep ownership of your code; you give the project a permanent license including relicensing rights — so the project can offer commercial licenses later without chasing every contributor. No CLA, no merge. Two minutes.

## 7. Ground rules

- **Be kind, be honest.** Review code, not people.
- **No AI-slop PRs.** AI assistance welcome; unreviewed mass-generated PRs get closed.
- **Self-hostable or it doesn't ship.** Every dependency must run on a contributor's laptop (`docker compose up`). Proprietary managed services are rejected in review.
- **Security issues** → private channel, never public issues (see security policy).
- **Brand, UX specs, production configs are private.** Don't ask for them in issues.

## 8. What NOT to expect

- ❌ Shares for PRs directly. (The pool flows through formal roles — section 4.)
- ❌ Payment for every contribution. Only bountied issues pay cash.
- ❌ 24-hour reviews. We're building the reviewer bench — that's why you're here.

---

**Ready?** Star the repo, pick a `good-first-issue`, say hello in Discussions. India's internet is being rebuilt — help lay a brick, and own a piece of the wall. 🧱

*Nothing here is an offer of securities. The contributor pool formalizes at incorporation with proper legal process.*
