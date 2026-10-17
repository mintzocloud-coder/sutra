# ADR-003: AGPLv3 + CLA from day one

**Date:** 2026-10-17 · **Status:** Accepted · **Decider:** Harshal Vanpariya

## Decision

Public code is licensed **AGPLv3** (single public license), with a **Harmony-style CLA** signed before the first external PR merges, and a quietly published **commercial dual-license** option.

## Rationale

- AGPLv3 is the strongest genuinely open-source (OSI-approved) license; for a super-app the service IS the product, so §13 network copyleft covers most of it.
- SSPL/BSL rejected: not OSI-approved ("fauxpen source") — every switch produced a funded fork (MongoDB→DocumentDB, Elastic→OpenSearch, Redis→Valkey in 30 days).
- No license stops clean-room clones — the real moat is network/data/brand/execution. AGPLv3 maximizes community trust while the moat compounds.

## Consequences

- Contributors: sign the CLA once (`CLA.md`). No CLA, no merge.
- Dependencies: AGPLv3-compatible only. GPLv2-only code is forbidden; BSL/source-available rejected in review.
- Company (Pvt Ltd) owns 100% copyright from day one — founder IP assignment at incorporation.
