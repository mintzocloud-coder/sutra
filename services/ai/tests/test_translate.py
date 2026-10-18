# SPDX-License-Identifier: AGPL-3.0-or-later
"""E4 unit tests: language-bridge domain logic.

Pure logic (script detection, cache keys, checkpoints) is asserted
directly. The service is exercised with a fake cache and fake backends —
no Valkey, no database, no model. The real IndicTrans2 backend is
covered by the fail-closed test (stack absent -> 503, never a crash);
the full model round-trip is the gated test at the bottom, run only
when TRANSLATE_TEST_REAL_MODEL=1 (CI-gated model download).
"""
from __future__ import annotations

import os
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app import translate as tr


# --- fakes ---------------------------------------------------------------

class FakeCache:
    def __init__(self):
        self.store: dict[str, tuple[str, int]] = {}
        self.counters: dict[str, int] = {}

    def get(self, key):
        hit = self.store.get(key)
        return hit[0] if hit else None

    def setex(self, key, ttl, value):
        self.store[key] = (value, ttl)

    def incr(self, key):
        self.counters[key] = self.counters.get(key, 0) + 1
        return self.counters[key]

    def expire(self, key, ttl):
        pass


class CountingBackend:
    name = "counting"

    def __init__(self):
        self.calls: list[tuple] = []

    def translate(self, text, src_flores, tgt_flores):
        self.calls.append((text, src_flores, tgt_flores))
        return f"{tgt_flores}:{text}"


def make_service(rpm=30, max_chars=4000, ttl=60, backend=None):
    settings = tr.TranslateSettings(
        backend="counting",
        requests_per_minute=rpm,
        max_input_chars=max_chars,
        cache_ttl_seconds=ttl,
    )
    return tr.TranslationService(
        cache=FakeCache(), backend=backend or CountingBackend(),
        settings=settings)


# --- language table ------------------------------------------------------

def test_supported_languages_headline_set():
    for iso, flores in [("hi", "hin_Deva"), ("gu", "guj_Gujr"),
                        ("ta", "tam_Taml"), ("bn", "ben_Beng"),
                        ("te", "tel_Telu"), ("en", "eng_Latn")]:
        assert tr.SUPPORTED_LANGUAGES[iso] == flores


def test_checkpoint_direction_routing():
    assert tr.checkpoint_for("eng_Latn", "hin_Deva", "dist-200M") == \
        "ai4bharat/indictrans2-en-indic-dist-200M"
    assert tr.checkpoint_for("hin_Deva", "eng_Latn", "dist-200M") == \
        "ai4bharat/indictrans2-indic-en-dist-200M"
    assert tr.checkpoint_for("hin_Deva", "guj_Gujr", "dist-200M") == \
        "ai4bharat/indictrans2-indic-indic-dist-320M"
    # 1B family swaps the repo ids.
    assert tr.checkpoint_for("eng_Latn", "hin_Deva", "1B") == \
        "ai4bharat/indictrans2-en-indic-1B"


# --- script detection ----------------------------------------------------

def test_detect_source_iso_scripts():
    assert tr.detect_source_iso("hello world") == "en"
    assert tr.detect_source_iso("") == "en"
    assert tr.detect_source_iso("नमस्ते दुनिया") == "hi"
    assert tr.detect_source_iso("વિશ્વ") == "gu"
    assert tr.detect_source_iso("வணக்கம்") == "ta"
    assert tr.detect_source_iso("বাংলা") == "bn"
    assert tr.detect_source_iso("తెలుగు") == "te"
    assert tr.detect_source_iso("ಕನ್ನಡ") == "kn"
    assert tr.detect_source_iso("മലയാളം") == "ml"
    assert tr.detect_source_iso("ਸਤ ਸ੍ਰੀ ਅਕਾਲ") == "pa"


def test_detect_source_iso_unknown_returns_none():
    # Emoji-only / CJK: never guess — the caller must pass source=.
    assert tr.detect_source_iso("🎉🎊") is None
    assert tr.detect_source_iso("你好") is None


# --- cache keys ----------------------------------------------------------

def test_cache_key_stable_and_scoped():
    k1 = tr.cache_key("indictrans2", "en", "hi", "hello")
    k2 = tr.cache_key("indictrans2", "en", "hi", "hello")
    assert k1 == k2
    assert k1.startswith("sutra:translate:v1:indictrans2:")
    # Target, source, text, and backend all scope the key.
    assert tr.cache_key("indictrans2", "en", "hi", "hello") != \
        tr.cache_key("indictrans2", "en", "gu", "hello")
    assert tr.cache_key("indictrans2", "en", "hi", "hello") != \
        tr.cache_key("indictrans2", "gu", "hi", "hello")
    assert tr.cache_key("indictrans2", "en", "hi", "hello") != \
        tr.cache_key("indictrans2", "en", "hi", "goodbye")
    assert tr.cache_key("indictrans2", "en", "hi", "hello") != \
        tr.cache_key("stub", "en", "hi", "hello")


# --- service: happy path + caching ---------------------------------------

def test_translate_happy_path_and_cache_hit():
    svc = make_service()
    backend = svc._backend
    r1 = svc.translate("user-1", "hello", "en", "hi")
    assert r1.text == "hin_Deva:hello"
    assert r1.source == "en" and r1.target == "hi"
    assert r1.cached is False and r1.model == "counting"
    assert backend.calls == [("hello", "eng_Latn", "hin_Deva")]
    r2 = svc.translate("user-1", "hello", "en", "hi")
    assert r2.cached is True and r2.text == r1.text
    assert len(backend.calls) == 1  # second request never touched the model


def test_translate_auto_detects_devanagari_source():
    svc = make_service()
    r = svc.translate("user-1", "नमस्ते", "auto", "en")
    assert r.source == "hi"
    assert svc._backend.calls[0][1] == "hin_Deva"


def test_translate_auto_undetectable_is_422():
    svc = make_service()
    with pytest.raises(tr.TranslateError) as exc:
        svc.translate("user-1", "🎉🎊", "auto", "hi")
    assert exc.value.status_code == 422
    assert exc.value.code == "invalid_language"


def test_translate_same_language_is_noop():
    svc = make_service()
    r = svc.translate("user-1", "hello", "en", "en")
    assert r.text == "hello" and r.cached is False
    assert svc._backend.calls == []  # no model call for a no-op


def test_stub_backend_marks_output():
    out = tr.StubBackend().translate("hello", "eng_Latn", "hin_Deva")
    assert out == "[hi] hello"


# --- service: validation ---------------------------------------------------

@pytest.mark.parametrize("kwargs,code", [
    ({"text": "", "source": "en", "target": "hi"}, "empty_text"),
    ({"text": "   ", "source": "en", "target": "hi"}, "empty_text"),
    ({"text": "x" * 4001, "source": "en", "target": "hi"}, "text_too_long"),
    ({"text": "hello", "source": "en", "target": "xx"}, "invalid_language"),
    ({"text": "hello", "source": "xx", "target": "hi"}, "invalid_language"),
])
def test_translate_validation(kwargs, code):
    svc = make_service()
    with pytest.raises(tr.TranslateError) as exc:
        svc.translate("user-1", **kwargs)
    assert exc.value.status_code == 422
    assert exc.value.code == code


def test_translate_missing_subject_is_401():
    svc = make_service()
    with pytest.raises(tr.TranslateError) as exc:
        svc.translate("", "hello", "en", "hi")
    assert exc.value.status_code == 401


# --- service: rate limit ---------------------------------------------------

def test_translate_rate_limit_429_with_retry_after():
    svc = make_service(rpm=2)
    svc.translate("user-1", "one", "en", "hi")
    svc.translate("user-1", "two", "en", "hi")
    with pytest.raises(tr.TranslateError) as exc:
        svc.translate("user-1", "three", "en", "hi")
    assert exc.value.status_code == 429
    assert exc.value.code == "translate_rate_limited"
    assert exc.value.retry_after is not None and exc.value.retry_after >= 1


def test_translate_rate_limit_is_per_user():
    svc = make_service(rpm=1)
    svc.translate("user-1", "one", "en", "hi")
    # A different user has their own window.
    svc.translate("user-2", "one", "en", "hi")


# --- fail-closed paths -------------------------------------------------------

def test_new_service_unknown_backend_refuses_boot():
    settings = SimpleNamespace(
        translate_backend="bogus",
        translate_model_size="dist-200M",
        translate_model_dir="/tmp/x",
        translate_requests_per_minute=30,
        translate_max_input_chars=4000,
        translate_cache_ttl_seconds=60,
        valkey_addr="valkey:6379",
    )
    with pytest.raises(ValueError, match="unknown TRANSLATE_BACKEND"):
        tr.new_service(settings)


def test_indictrans2_backend_without_stack_is_503_not_crash():
    # The heavy stack (torch/transformers) is not installed on the unit
    # test runner — the backend must fail closed, never crash.
    try:
        import torch  # noqa: F401
    except ImportError:
        pass
    else:
        pytest.skip("torch installed — fail-closed path not exercisable here")
    backend = tr.IndicTrans2Backend()
    with pytest.raises(tr.TranslateError) as exc:
        backend.translate("hello", "eng_Latn", "hin_Deva")
    assert exc.value.status_code == 503
    assert exc.value.code == "translate_unavailable"


# --- real model round-trip (CI-gated) -----------------------------------------
# Runs only when TRANSLATE_TEST_REAL_MODEL=1: downloads the dist-200M
# checkpoint (~800MB) and translates en->hi through the real stack.

@pytest.mark.skipif(
    os.environ.get("TRANSLATE_TEST_REAL_MODEL") != "1",
    reason="real-model round-trip is CI-gated (model download)",
)
def test_indictrans2_real_round_trip():
    pytest.importorskip("torch")
    pytest.importorskip("transformers")
    pytest.importorskip("IndicTransToolkit")
    backend = tr.IndicTrans2Backend(model_size="dist-200M")
    out = backend.translate("Hello, how are you?", "eng_Latn", "hin_Deva")
    assert out.strip() and out.strip() != "Hello, how are you?"
