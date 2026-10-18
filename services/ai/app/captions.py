# SPDX-License-Identifier: AGPL-3.0-or-later
"""Auto-caption domain logic (E3 part 2).

Pure building blocks for the faster-whisper caption worker: caption
settings from env, the caption state-machine SQL contract (mirrors
migration 0006 and docs/creator-studio.md), sidecar formatters (WebVTT
+ SubRip), exponential backoff, and stream-job parsing.

The worker itself lives in app/caption_worker.py; the process entry
point is app/caption_main.py. Nothing here touches the network — every
function is unit-testable without a database, Valkey, SeaweedFS, or a
whisper model.
"""
from __future__ import annotations

import os
import re
import socket
from dataclasses import dataclass, field

# Stream / key names. The input stream constant mirrors the Go producer
# (services/api/internal/media.CaptionStream = "media:captions"); the
# dead-letter stream, attempts hash and heartbeat key are owned by this
# worker, following the transcode worker's naming playbook.
STREAM = "media:captions"
DEAD_LETTER_STREAM = "media:captions:dlq"
ATTEMPTS_HASH = "media:captions:attempts"
HEARTBEAT_KEY = "worker:captions:heartbeat"

# Caption lifecycle, mirroring migration 0006's CHECK constraint.
STATUS_NONE = "none"
STATUS_REQUESTED = "requested"
STATUS_PROCESSING = "processing"
STATUS_READY = "ready"
STATUS_FAILED = "failed"

_UUID_RE = re.compile(
    r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-"
    r"[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$"
)

_DURATION_RE = re.compile(r"^\s*(\d+(?:\.\d+)?)\s*([smh]?)\s*$", re.IGNORECASE)


def parse_duration(raw: str, default_seconds: float) -> float:
    """Parse '60s' / '30m' / '1.5h' / '90' (bare = seconds) into seconds.

    Unparseable input falls back to the default — a misconfigured knob
    must never crash the worker, it just runs with a sane value (and
    logs it at startup).
    """
    if not raw:
        return default_seconds
    m = _DURATION_RE.match(raw)
    if not m:
        return default_seconds
    value = float(m.group(1))
    unit = m.group(2).lower()
    if unit == "m":
        value *= 60.0
    elif unit == "h":
        value *= 3600.0
    return value


def _get_env(key: str, default: str = "") -> str:
    return os.environ.get(key, default)


def _get_int(key: str, default: int) -> int:
    try:
        return int(_get_env(key, str(default)))
    except ValueError:
        return default


@dataclass(frozen=True)
class CaptionSettings:
    """All caption-worker knobs. Defaults match docs/creator-studio.md."""

    model: str = "base"
    group: str = "caption-workers"
    consumer: str = ""
    max_attempts: int = 3
    backoff_base_seconds: float = 60.0
    backoff_max_seconds: float = 1800.0
    sweep_interval_seconds: float = 300.0
    work_dir: str = "/tmp/captions"
    model_dir: str = "/srv/ai/models/whisper"
    # Stuck-row thresholds (fixed by the state-machine contract, not env —
    # the sweep index and the Go producer's expectations depend on them).
    requested_stuck_seconds: float = 600.0
    processing_stuck_seconds: float = 1800.0
    heartbeat_ttl_seconds: float = 60.0
    valkey_addr: str = "valkey:6379"
    seaweedfs_master_addr: str = "seaweedfs:9333"
    media_public_base_url: str = "http://seaweedfs:8080"

    @classmethod
    def from_env(cls) -> "CaptionSettings":
        return cls(
            model=_get_env("CAPTION_MODEL", "base"),
            group=_get_env("CAPTION_GROUP", "caption-workers"),
            consumer=_get_env("CAPTION_CONSUMER", socket.gethostname()),
            max_attempts=_get_int("CAPTION_MAX_ATTEMPTS", 3),
            backoff_base_seconds=parse_duration(
                _get_env("CAPTION_BACKOFF_BASE", "60s"), 60.0
            ),
            backoff_max_seconds=parse_duration(
                _get_env("CAPTION_BACKOFF_MAX", "30m"), 1800.0
            ),
            sweep_interval_seconds=parse_duration(
                _get_env("CAPTION_SWEEP_INTERVAL", "5m"), 300.0
            ),
            work_dir=_get_env("CAPTION_WORK_DIR", "/tmp/captions"),
            model_dir=_get_env("CAPTION_MODEL_DIR", "/srv/ai/models/whisper"),
            valkey_addr=_get_env("VALKEY_ADDR", "valkey:6379"),
            seaweedfs_master_addr=_get_env(
                "SEAWEEDFS_MASTER_ADDR", "seaweedfs:9333"
            ),
            media_public_base_url=_get_env(
                "MEDIA_PUBLIC_BASE_URL", "http://seaweedfs:8080"
            ).rstrip("/"),
        )


def backoff_for(attempt: int, base: float, cap: float) -> float:
    """Delay before attempt n (1-based): base * 2^(n-1), capped at cap.

    Mirrors the Go transcode worker's BackoffFor — transcription rarely
    recovers on immediate retry, so the delay grows fast.
    """
    if attempt < 1:
        attempt = 1
    delay = base * (2.0 ** (attempt - 1))
    return min(delay, cap)


@dataclass(frozen=True)
class CaptionSegment:
    """One transcribed cue: [start, end) in seconds, display text."""

    start: float
    end: float
    text: str


@dataclass(frozen=True)
class CaptionJob:
    """One unit of work off the media:captions stream."""

    media_id: str


def parse_job(values: dict) -> CaptionJob | None:
    """Parse stream fields into a CaptionJob. Returns None for malformed
    jobs (the worker ACKs and logs them — a poison entry must never
    wedge the consumer)."""
    media_id = values.get("media_id")
    if not isinstance(media_id, str) or not _UUID_RE.match(media_id):
        return None
    return CaptionJob(media_id=media_id)


def _vtt_timestamp(seconds: float) -> str:
    total_ms = max(0, int(round(seconds * 1000)))
    h, rem = divmod(total_ms, 3_600_000)
    m, rem = divmod(rem, 60_000)
    s, ms = divmod(rem, 1000)
    return f"{h:02d}:{m:02d}:{s:02d}.{ms:03d}"


def _srt_timestamp(seconds: float) -> str:
    return _vtt_timestamp(seconds).replace(".", ",")


def _clean_text(text: str) -> str:
    """Cue text must not contain cue-timing arrows or raw newlines that
    would corrupt the sidecar files; collapse whitespace."""
    return re.sub(r"\s+", " ", text.replace("-->", "->")).strip()


def format_vtt(segments: list[CaptionSegment]) -> str:
    """WebVTT sidecar. Header + blank line, then one cue per segment."""
    lines = ["WEBVTT", ""]
    for seg in segments:
        text = _clean_text(seg.text)
        if not text or seg.end <= seg.start:
            continue
        lines.append(f"{_vtt_timestamp(seg.start)} --> {_vtt_timestamp(seg.end)}")
        lines.append(text)
        lines.append("")
    return "\n".join(lines)


def format_srt(segments: list[CaptionSegment]) -> str:
    """SubRip sidecar: numbered cues, comma-millisecond timings."""
    lines: list[str] = []
    n = 0
    for seg in segments:
        text = _clean_text(seg.text)
        if not text or seg.end <= seg.start:
            continue
        n += 1
        lines.append(str(n))
        lines.append(f"{_srt_timestamp(seg.start)} --> {_srt_timestamp(seg.end)}")
        lines.append(text)
        lines.append("")
    return "\n".join(lines)


# --- SQL contract (psycopg %s params) -------------------------------------
# These strings are the worker's half of the contract documented in
# docs/creator-studio.md. The claim UPDATE is the concurrency gate: 0
# rows means another worker (or a redelivery) already holds this job.

CLAIM_JOB_SQL = (
    "UPDATE media SET caption_status = %s, updated_at = now() "
    "WHERE id = %s AND caption_status IN (%s, %s)"
)

RESOLVE_JOB_SQL = (
    "UPDATE media SET caption_status = %s, caption_vtt_fid = %s, "
    "caption_srt_fid = %s, caption_error = NULL, updated_at = now() "
    "WHERE id = %s AND caption_status = %s"
)

FAIL_JOB_SQL = (
    "UPDATE media SET caption_status = %s, caption_error = %s, "
    "updated_at = now() WHERE id = %s"
)

SWEEP_REQUESTED_SQL = (
    "SELECT id FROM media WHERE kind = 'video' "
    "AND caption_status = %s AND updated_at < now() - make_interval(secs => %s)"
)

SWEEP_PROCESSING_SQL = (
    "SELECT id FROM media WHERE kind = 'video' "
    "AND caption_status = %s AND updated_at < now() - make_interval(secs => %s)"
)

# Re-driving a stuck row reuses the claim gate so a live worker holding
# the job cannot be clobbered.
REDRIVE_PROCESSING_SQL = (
    "UPDATE media SET caption_status = %s, updated_at = now() "
    "WHERE id = %s AND caption_status = %s"
)

JOB_LOOKUP_SQL = "SELECT status, fids FROM media WHERE id = %s"


@dataclass
class CaptionResult:
    """What the worker produced for one job (for logging/assertions)."""

    media_id: str
    vtt_fid: str
    srt_fid: str
    segment_count: int
    detected_language: str = ""
