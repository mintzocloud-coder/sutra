# SPDX-License-Identifier: AGPL-3.0-or-later

# Language bridge (E4)

`POST /v1/ai/translate` translates short user-facing text (post bodies,
comments) between English and Indic languages. The Go API proxies it to
the Python AI service's `POST /translate` (see `services/ai/app/translate.py`).

## Why this exists

Sutra is India-first. A Hindi post should be readable by a Tamil user
and vice versa — without either side leaving the feed. Machine
translation is the cheapest way to collapse that barrier, and caching
makes it nearly free at feed scale: the same viral post is translated
once per target language, not once per viewer.

## Request / response

```
POST /v1/ai/translate
Authorization: Bearer <user JWT>

{"text": "Hello, how are you?", "source": "auto", "target": "hi"}
```

```json
{
  "text": "नमस्ते, आप कैसे हैं?",
  "source": "en",
  "target": "hi",
  "cached": false,
  "model": "indictrans2"
}
```

- `source: "auto"` resolves via Unicode script detection (ASCII → `en`,
  Devanagari → `hi`, Tamil script → `ta`, …). Mixed-script or
  undetectable text returns 422 `invalid_language` — pass `source`
  explicitly instead of letting the server guess.
- `cached: true` means the Valkey entry served this request; no model
  call happened. Clients may show a subtle "translated" affordance from
  this flag.
- Translating into the same language is a validated no-op (still
  rate-limited, never touches the model).

## Backend ladder

| `TRANSLATE_BACKEND` | Behavior |
|---|---|
| `indictrans2` (default) | Real AI4Bharat IndicTrans2 checkpoints, loaded lazily per direction. The heavy stack (torch, transformers, IndicTransToolkit) imports lazily; when it is absent the endpoint answers 503 `translate_unavailable` — fail-closed, never a silent stub. |
| `stub` | Deterministic test double returning `[tgt] text`. Smoke/CI only. |

An unknown backend value refuses service boot (fail-closed).

### Checkpoints (verified on HuggingFace 2026-10-18)

| Direction | dist (default) | 1B (`TRANSLATE_MODEL_SIZE=1B`) |
|---|---|---|
| en → indic | `ai4bharat/indictrans2-en-indic-dist-200M` (MIT) | `ai4bharat/indictrans2-en-indic-1B` |
| indic → en | `ai4bharat/indictrans2-indic-en-dist-200M` (MIT) | `ai4bharat/indictrans2-indic-en-1B` |
| indic → indic | `ai4bharat/indictrans2-indic-indic-dist-320M` | `ai4bharat/indictrans2-indic-indic-1B` |

License note: the two dist-200M checkpoints verified above carry an
explicit MIT license on their model cards. The indic→indic dist-320M
card has **no machine-readable license field** — confirm its terms
before relying on it in production (the code routes indic→indic
through it, flagged here deliberately rather than hidden).

Models download once into `TRANSLATE_MODEL_DIR`
(`/srv/ai/models/translate`); mount a persistent volume there so
redeploys don't re-download ~800MB.

### Supported languages

`hi gu ta bn te kn ml mr pa or as ur ne en` (ISO 639-1 → FLORES-200
codes in `app/translate.py`). Devanagari auto-detection defaults to
`hi` (also covers mr/ne text — pass `source` explicitly when it
matters); Bengali script defaults to `bn` (also `as`).

## Caching

Valkey key: `sutra:translate:v1:<backend>:<sha256(src␀tgt␀text)[:48]>`,
TTL `TRANSLATE_CACHE_TTL` (default 30 days). The backend name is part
of the key so a stub/dev cache can never poison real translations.

Translations are deterministic per (backend, src, tgt, text), so long
TTLs are safe and there is no invalidation path by design.

## Guards

- Validation first: empty → 422 `empty_text`; over
  `TRANSLATE_MAX_INPUT_CHARS` (default 4000) → 422 `text_too_long`;
  unknown language → 422 `invalid_language`.
- Per-user fixed-window rate limit in Valkey (`TRANSLATE_RPM`, default
  30/min) → 429 `translate_rate_limited` + `Retry-After`. The check runs
  before the cache lookup, so the endpoint can't be used as an
  unbounded oracle even on cache hits.
- Missing model stack → 503 `translate_unavailable`; inference failure
  → 502 `translate_failed`.

## Failure modes

| Condition | Response |
|---|---|
| No JWT / bad service token | 401 (proxy / AI service) |
| Empty / too-long / unknown language | 422 envelope |
| Rate limit hit | 429 + Retry-After |
| AI service down or unconfigured | 503 `service_unavailable` (proxy) |
| IndicTrans2 stack not installed | 503 `translate_unavailable` (never a silent stub) |
| Model inference error | 502 `translate_failed` |

## Feed integration (E4 part 2)

`GET /v1/feed/home?translate=xx` is IMPLEMENTED (Go,
`internal/feed/translate.go`): the API translates item bodies
best-effort through this endpoint and never blocks the feed read on
it. Items carry `translated_text` / `translated_from` (omitempty —
absent when untranslated); an unsupported target is 400
`invalid_translate_target`. Translation work is bounded per page
(`translatePageBudget`, 6s); per-item calls carry the caller's verified
subject as `X-Sutra-Subject` so the Python per-user rate limit applies.
An empty `AI_SERVICE_TOKEN` disables the decorator with a loud WARN at
boot — `?translate=` then no-ops.

## Out of scope (Wave 2+)

- Real-time chat message translation (needs the websocket path; the
  endpoint is request-scoped today).
- Translation quality evals (E6's eval harness covers chat prompts;
  translation goldens are a separate set).
- User-level language preference storage (profile field; feed then
  passes it as `target` automatically).
