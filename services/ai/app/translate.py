# SPDX-License-Identifier: AGPL-3.0-or-later
"""Language bridge (E4): IndicTrans2-powered translation.

POST /translate (served by the Go proxy as /v1/ai/translate) translates
short user-facing text — post bodies, comments — between English and
Indic languages. Every translation is cached in Valkey by
(content-hash, target) so repeated feed reads never re-run the model,
and per-user rate limits keep the (expensive) model calls bounded.

Backend ladder:
  * ``stub``        — deterministic test double. Returns ``[tgt] text``;
                      never a real translation. For smoke/CI only.
  * ``indictrans2`` — the real path: AI4Bharat IndicTrans2 checkpoints
                      (MIT-licensed; verified on HuggingFace 2026-10-18)
                      loaded lazily via transformers + IndicTransToolkit.
                      The heavy stack (torch/transformers) is imported
                      lazily inside the backend so the service boots and
                      unit tests run without it; when the stack is absent
                      the backend fails closed (503), never silently
                      falling back to the stub.

Source language ``auto`` resolves via Unicode script detection
(detect_source_iso) — Devanagari text becomes hi, Tamil script becomes
ta, etc. ASCII-only text becomes en. When no script is recognized the
caller must pass an explicit source (422).

Nothing here imports fastapi or redis at module level: the service
takes an injected cache client, so all of this is unit-testable with
fakes. The only network touches are the cache client's get/setex/incr
and (in the real backend) the one-time model download.
"""
from __future__ import annotations

import hashlib
import os
import threading
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from typing import Protocol


# --- Language table -------------------------------------------------------
# ISO 639-1 codes the API accepts -> IndicTrans2 FLORES-200 codes.
# The plan's headline set (hi/gu/ta/bn/te/en) plus the other scheduled
# languages IndicTrans2 covers. Devanagari is shared by hi/mr/ne and
# Bengali script by bn/as: auto-detection picks the most common one and
# documents the ambiguity — callers who care pass source explicitly.

SUPPORTED_LANGUAGES: dict[str, str] = {
    "hi": "hin_Deva",
    "gu": "guj_Gujr",
    "ta": "tam_Taml",
    "bn": "ben_Beng",
    "te": "tel_Telu",
    "kn": "kan_Knda",
    "ml": "mal_Mlym",
    "mr": "mar_Deva",
    "pa": "pan_Guru",
    "or": "ory_Orya",
    "as": "asm_Beng",
    "ur": "urd_Arab",
    "ne": "nep_Deva",
    "en": "eng_Latn",
}

# Direction checkpoints: (src-group, tgt-group) -> HF repo id.
# Verified live on HuggingFace 2026-10-18; en-indic and indic-en
# dist-200M carry an explicit MIT license. The indic-indic dist-320M
# card has no machine-readable license field — confirm before relying
# on it in production (docs/language-bridge.md).
_CHECKPOINTS_200M = {
    ("eng", "indic"): "ai4bharat/indictrans2-en-indic-dist-200M",
    ("indic", "eng"): "ai4bharat/indictrans2-indic-en-dist-200M",
    ("indic", "indic"): "ai4bharat/indictrans2-indic-indic-dist-320M",
}
_CHECKPOINTS_1B = {
    ("eng", "indic"): "ai4bharat/indictrans2-en-indic-1B",
    ("indic", "eng"): "ai4bharat/indictrans2-indic-en-1B",
    ("indic", "indic"): "ai4bharat/indictrans2-indic-indic-1B",
}

# Unicode script ranges for auto source detection: (start, end, iso).
# Majority script wins; ties resolve to the first-listed range.
_SCRIPT_RANGES = [
    (0x0900, 0x097F, "hi"),   # Devanagari (also mr/ne — defaults to hi)
    (0x0980, 0x09FF, "bn"),   # Bengali (also as — defaults to bn)
    (0x0A00, 0x0A7F, "pa"),   # Gurmukhi
    (0x0A80, 0x0AFF, "gu"),   # Gujarati
    (0x0B00, 0x0B7F, "or"),   # Oriya
    (0x0B80, 0x0BFF, "ta"),   # Tamil
    (0x0C00, 0x0C7F, "te"),   # Telugu
    (0x0C80, 0x0CFF, "kn"),   # Kannada
    (0x0D00, 0x0D7F, "ml"),   # Malayalam
    (0x0600, 0x06FF, "ur"),   # Arabic block (Urdu in our language set)
]


def detect_source_iso(text: str) -> str | None:
    """Best-effort source language from Unicode scripts.

    Pure function: ASCII-only -> "en"; majority Indic/Arabic script ->
    its mapped ISO code; nothing recognized -> None (caller must pass
    an explicit source). Latin-extended / CJK / emoji-only text returns
    None — guessing English for those would mistranslate silently.
    """
    if not text or text.isascii():
        return "en"
    counts: dict[str, int] = {}
    for ch in text:
        cp = ord(ch)
        for start, end, iso in _SCRIPT_RANGES:
            if start <= cp <= end:
                counts[iso] = counts.get(iso, 0) + 1
                break
    if not counts:
        return None
    return max(counts, key=lambda iso: counts[iso])


def _direction_group(flores: str) -> str:
    return "eng" if flores == "eng_Latn" else "indic"


def checkpoint_for(src_flores: str, tgt_flores: str, size: str) -> str:
    """HF repo id for one translation direction."""
    table = _CHECKPOINTS_1B if size == "1B" else _CHECKPOINTS_200M
    return table[(_direction_group(src_flores), _direction_group(tgt_flores))]


# --- Errors ---------------------------------------------------------------

@dataclass
class TranslateError(Exception):
    """Raised for every rejected/failed translation. The HTTP layer maps
    these to the shared JSON error envelope."""

    code: str            # invalid_language | empty_text | text_too_long |
                         # translate_rate_limited | translate_unavailable |
                         # translate_failed
    message: str
    status_code: int = 422
    retry_after: int | None = None


def _seconds_to_next_minute_utc() -> int:
    now = datetime.now(timezone.utc)
    nxt = (now + timedelta(minutes=1)).replace(second=0, microsecond=0)
    return max(1, int((nxt - now).total_seconds()))


# --- Cache ----------------------------------------------------------------

CACHE_PREFIX = "sutra:translate:v1"


def cache_key(backend_name: str, src_iso: str, tgt_iso: str, text: str) -> str:
    """Valkey key for (backend, src, tgt, text).

    The backend name is part of the key so a stub/dev cache can never
    poison real translations (and a future model upgrade can bump the
    segment instead of flushing). The digest is truncated — 192 bits is
    far beyond collision-plausible for a cache key.
    """
    digest = hashlib.sha256(
        f"{src_iso}\x00{tgt_iso}\x00{text}".encode("utf-8")
    ).hexdigest()[:48]
    return f"{CACHE_PREFIX}:{backend_name}:{digest}"


def rate_limit_key(subject: str) -> str:
    """Per-user per-minute window key (UTC)."""
    window = datetime.now(timezone.utc).strftime("%Y%m%d%H%M")
    return f"{CACHE_PREFIX}:rl:{subject}:{window}"


# --- Backends -------------------------------------------------------------

class TranslateBackend(Protocol):
    name: str

    def translate(self, text: str, src_flores: str, tgt_flores: str) -> str:
        """Translate text; raises TranslateError on failure."""


class StubBackend:
    """Deterministic test double. Output is visibly marked and is NEVER
    a real translation — smoke/CI only."""

    name = "stub"

    def translate(self, text: str, src_flores: str, tgt_flores: str) -> str:
        iso = next(
            (k for k, v in SUPPORTED_LANGUAGES.items() if v == tgt_flores),
            tgt_flores,
        )
        return f"[{iso}] {text}"


class IndicTrans2Backend:
    """Real IndicTrans2 backend (lazy).

    Models load on first translate() call, one checkpoint per direction
    group, guarded by a lock so concurrent first requests load once.
    torch / transformers / IndicTransToolkit are imported inside
    _ensure_loaded: if the stack is not installed the backend raises
    TranslateError(503 translate_unavailable) instead of crashing the
    request — fail closed, never silently degrade to the stub.
    """

    name = "indictrans2"

    def __init__(self, model_size: str = "dist-200M",
                 model_dir: str = "/srv/ai/models/translate"):
        self._size = model_size if model_size == "1B" else "dist-200M"
        self._model_dir = model_dir
        self._lock = threading.Lock()
        self._loaded: dict[str, tuple] = {}  # checkpoint -> (tok, model, proc)
        self._torch = None

    def _ensure_loaded(self, checkpoint: str) -> tuple:
        if checkpoint in self._loaded:
            return self._loaded[checkpoint]
        with self._lock:
            if checkpoint in self._loaded:  # double-checked
                return self._loaded[checkpoint]
            try:
                import torch
                from transformers import (AutoModelForSeq2SeqLM,
                                          AutoTokenizer)
                from IndicTransToolkit.processor import IndicProcessor
            except ImportError as exc:
                raise TranslateError(
                    code="translate_unavailable",
                    message=(
                        "IndicTrans2 runtime not installed "
                        "(needs torch, transformers, IndicTransToolkit); "
                        "set TRANSLATE_BACKEND=stub for smoke/CI"
                    ),
                    status_code=503,
                ) from exc
            self._torch = torch
            device = "cuda" if torch.cuda.is_available() else "cpu"
            tokenizer = AutoTokenizer.from_pretrained(
                checkpoint, trust_remote_code=True, cache_dir=self._model_dir
            )
            model = AutoModelForSeq2SeqLM.from_pretrained(
                checkpoint, trust_remote_code=True, cache_dir=self._model_dir
            ).to(device)
            processor = IndicProcessor(inference=True)
            loaded = (tokenizer, model, processor, device)
            self._loaded[checkpoint] = loaded
            return loaded

    def translate(self, text: str, src_flores: str, tgt_flores: str) -> str:
        checkpoint = checkpoint_for(src_flores, tgt_flores, self._size)
        tokenizer, model, processor, device = self._ensure_loaded(checkpoint)
        try:
            batch = processor.preprocess_batch(
                [text], src_lang=src_flores, tgt_lang=tgt_flores
            )
            inputs = tokenizer(
                batch, truncation=True, padding="longest",
                return_tensors="pt", return_attention_mask=True,
            ).to(device)
            torch = self._torch
            with torch.no_grad():
                generated = model.generate(
                    **inputs, use_cache=True, min_length=0, max_length=256,
                    num_beams=5, num_return_sequences=1,
                )
            with tokenizer.as_target_tokenizer():
                decoded = tokenizer.batch_decode(
                    generated.detach().cpu().tolist(),
                    skip_special_tokens=True, clean_up_tokenization_spaces=True,
                )
            out = processor.postprocess_batch(decoded, lang=tgt_flores)
        except TranslateError:
            raise
        except Exception as exc:
            raise TranslateError(
                code="translate_failed",
                message=f"IndicTrans2 inference failed: {exc}",
                status_code=502,
            ) from exc
        if not out or not out[0].strip():
            raise TranslateError(
                code="translate_failed",
                message="IndicTrans2 returned an empty translation",
                status_code=502,
            )
        return out[0]


# --- Service --------------------------------------------------------------

@dataclass(frozen=True)
class TranslateSettings:
    backend: str = "indictrans2"          # stub | indictrans2
    model_size: str = "dist-200M"         # dist-200M | 1B
    model_dir: str = "/srv/ai/models/translate"
    requests_per_minute: int = 30
    max_input_chars: int = 4000
    cache_ttl_seconds: int = 2_592_000    # 30 days
    valkey_addr: str = "valkey:6379"

    @classmethod
    def from_env(cls) -> "TranslateSettings":
        return cls(
            backend=os.environ.get("TRANSLATE_BACKEND", "indictrans2"),
            model_size=os.environ.get("TRANSLATE_MODEL_SIZE", "dist-200M"),
            model_dir=os.environ.get(
                "TRANSLATE_MODEL_DIR", "/srv/ai/models/translate"),
            requests_per_minute=_get_int("TRANSLATE_RPM", 30),
            max_input_chars=_get_int("TRANSLATE_MAX_INPUT_CHARS", 4000),
            cache_ttl_seconds=_get_int("TRANSLATE_CACHE_TTL", 2_592_000),
            valkey_addr=os.environ.get("VALKEY_ADDR", "valkey:6379"),
        )


def _get_int(key: str, default: int) -> int:
    try:
        return int(os.environ.get(key, str(default)))
    except ValueError:
        return default


@dataclass(frozen=True)
class TranslateResult:
    text: str
    source: str      # resolved ISO code
    target: str      # ISO code
    cached: bool
    model: str       # backend name (e.g. "indictrans2", "stub")


class TranslationService:
    """Validates, rate-limits, caches, and translates.

    The cache client is injected (duck-typed: get/setex/incr/expire) so
    unit tests use a fake and the service never imports redis itself.
    """

    def __init__(self, cache, backend: TranslateBackend,
                 settings: TranslateSettings):
        self._cache = cache
        self._backend = backend
        self._settings = settings

    def _check_rate_limit(self, subject: str) -> None:
        rpm = self._settings.requests_per_minute
        if rpm <= 0:
            return
        key = rate_limit_key(subject)
        count = self._cache.incr(key)
        if count == 1:
            # First hit in this window: bound the key's lifetime so a
            # crashed writer can never leak a permanent counter.
            self._cache.expire(key, 120)
        if count > rpm:
            raise TranslateError(
                code="translate_rate_limited",
                message=f"translation rate limit exceeded ({rpm}/min)",
                status_code=429,
                retry_after=_seconds_to_next_minute_utc(),
            )

    def translate(self, subject: str, text: str, source: str,
                  target: str) -> TranslateResult:
        if not subject or not subject.strip():
            raise TranslateError(
                code="unauthorized",
                message="missing subject — call through the Go API proxy",
                status_code=401,
            )
        text = (text or "").strip()
        if not text:
            raise TranslateError(
                code="empty_text", message="text must not be empty")
        if len(text) > self._settings.max_input_chars:
            raise TranslateError(
                code="text_too_long",
                message=(f"text is {len(text)} chars; "
                         f"limit is {self._settings.max_input_chars}"),
            )
        target = (target or "").strip().lower()
        if target not in SUPPORTED_LANGUAGES:
            raise TranslateError(
                code="invalid_language",
                message=(f"unsupported target language {target!r}; "
                         f"supported: {','.join(sorted(SUPPORTED_LANGUAGES))}"),
            )
        source = (source or "auto").strip().lower()
        if source == "auto":
            detected = detect_source_iso(text)
            if detected is None:
                raise TranslateError(
                    code="invalid_language",
                    message=("could not detect the source language; "
                             "pass source= explicitly"),
                )
            src_iso = detected
        elif source not in SUPPORTED_LANGUAGES:
            raise TranslateError(
                code="invalid_language",
                message=(f"unsupported source language {source!r}; "
                         f"use 'auto' or one of "
                         f"{','.join(sorted(SUPPORTED_LANGUAGES))}"),
            )
        else:
            src_iso = source

        self._check_rate_limit(subject)

        if src_iso == target:
            # Same-language "translation" is a no-op: no model call, no
            # cache write — but still rate-limited above so the endpoint
            # cannot be used as an unbounded echo oracle.
            return TranslateResult(text=text, source=src_iso, target=target,
                                   cached=False, model=self._backend.name)

        key = cache_key(self._backend.name, src_iso, target, text)
        hit = self._cache.get(key)
        if hit is not None:
            cached_text = hit.decode("utf-8") if isinstance(hit, bytes) else hit
            return TranslateResult(text=cached_text, source=src_iso,
                                   target=target, cached=True,
                                   model=self._backend.name)

        translated = self._backend.translate(
            text, SUPPORTED_LANGUAGES[src_iso], SUPPORTED_LANGUAGES[target])
        self._cache.setex(key, self._settings.cache_ttl_seconds, translated)
        return TranslateResult(text=translated, source=src_iso, target=target,
                               cached=False, model=self._backend.name)


def new_service(settings) -> TranslationService:
    """Build the production service: Valkey client + configured backend.

    Fail-closed: unknown TRANSLATE_BACKEND or an unreachable Valkey
    raises here (the lifespan refuses to boot). The model itself loads
    lazily on first real translation so deploys don't block on the
    multi-hundred-MB download.
    """
    backend_name = settings.translate_backend
    if backend_name == "stub":
        backend: TranslateBackend = StubBackend()
    elif backend_name == "indictrans2":
        backend = IndicTrans2Backend(
            model_size=settings.translate_model_size,
            model_dir=settings.translate_model_dir,
        )
    else:
        raise ValueError(
            f"unknown TRANSLATE_BACKEND {backend_name!r} — "
            "expected 'stub' or 'indictrans2'; refusing to start"
        )
    import redis  # local import: unit tests inject a fake cache instead

    host, _, port = settings.valkey_addr.partition(":")
    client = redis.Redis(host=host or "valkey", port=int(port or 6379),
                         socket_connect_timeout=5, socket_timeout=5,
                         decode_responses=False)
    client.ping()  # fail fast when Valkey is unreachable
    return TranslationService(
        cache=client,
        backend=backend,
        settings=TranslateSettings(
            backend=backend_name,
            model_size=settings.translate_model_size,
            model_dir=settings.translate_model_dir,
            requests_per_minute=settings.translate_requests_per_minute,
            max_input_chars=settings.translate_max_input_chars,
            cache_ttl_seconds=settings.translate_cache_ttl_seconds,
            valkey_addr=settings.valkey_addr,
        ),
    )
