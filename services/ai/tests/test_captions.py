# SPDX-License-Identifier: AGPL-3.0-or-later
"""E3 unit tests: caption domain logic + worker lifecycle.

Pure logic (VTT/SRT, backoff, durations, job parsing) is asserted
directly. The worker is exercised with fakes for the pool, the stream,
storage, and the transcriber — no database, Valkey, SeaweedFS, or
whisper model. The SQL strings are the contract from
docs/creator-studio.md; tests assert the worker issues the right
statements with the right parameters.
"""
from __future__ import annotations

import sys
from pathlib import Path
from unittest import mock

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app import captions as cap
from app import caption_worker as cw


# --- fakes ---------------------------------------------------------------

class FakeCursor:
    def __init__(self, pool):
        self._pool = pool
        self.rowcount = 0
        self._rows: list[tuple] = []

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def execute(self, sql, params=None):
        self._pool.executed.append((sql, params))
        self.rowcount, self._rows = self._pool.script(sql, params)

    def fetchone(self):
        return self._rows[0] if self._rows else None

    def fetchall(self):
        return list(self._rows)


class FakeConn:
    def __init__(self, pool):
        self._pool = pool

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def cursor(self):
        return FakeCursor(self._pool)


class FakePool:
    """script(sql, params) -> (rowcount, rows). Tests configure per-SQL
    behavior by overriding script."""

    def __init__(self):
        self.executed: list[tuple[str, tuple | None]] = []
        self.claim_rows = 1
        self.lookup_fids = ["3,abc123"]

    def connection(self):
        return FakeConn(self)

    def script(self, sql, params):
        if "caption_status = %s, updated_at = now()" in sql and "IN (%s, %s)" in sql:
            return self.claim_rows, []
        if sql.startswith("SELECT status, fids"):
            return 1, [("ready", self.lookup_fids)]
        if sql.startswith("SELECT id FROM media"):
            return 0, []
        return 1, []


class FakeStream:
    def __init__(self):
        self.acked: list[str] = []
        self.added: list[dict] = []
        self.dead: list[dict] = []
        self.attempts: dict[str, int] = {}
        self.heartbeats = 0

    def ensure_group(self):
        pass

    def read_group(self, block_ms):
        return []

    def ack(self, entry_id):
        self.acked.append(entry_id)

    def add(self, values):
        self.added.append(dict(values))
        return "1-0"

    def dead_letter(self, values):
        self.dead.append(dict(values))
        return "1-0"

    def get_attempts(self, media_id):
        return self.attempts.get(media_id, 0)

    def set_attempts(self, media_id, n):
        self.attempts[media_id] = n

    def clear_attempts(self, media_id):
        self.attempts.pop(media_id, None)

    def heartbeat(self):
        self.heartbeats += 1


class FakeStorage:
    def __init__(self, video: bytes = b"fake-video-bytes"):
        self.video = video
        self.uploads: list[tuple[str, str, bytes]] = []

    def download(self, fid):
        return self.video

    def upload(self, filename, mime, data):
        self.uploads.append((filename, mime, data))
        return f"9,{filename}-fid"


class FakeTranscriber:
    def __init__(self, segments=None, fail=None):
        self._segments = segments if segments is not None else [
            cap.CaptionSegment(0.0, 1.5, "hello world"),
            cap.CaptionSegment(1.5, 3.0, "namaste duniya"),
        ]
        self._fail = fail
        self.calls = 0

    @property
    def detected_language(self):
        return "hi"

    def transcribe(self, path):
        self.calls += 1
        if self._fail:
            raise self._fail
        return list(self._segments)


def make_worker(**kw):
    settings = cap.CaptionSettings(
        model="base",
        group="g",
        consumer="c",
        max_attempts=kw.pop("max_attempts", 3),
        backoff_base_seconds=60.0,
        backoff_max_seconds=1800.0,
        sweep_interval_seconds=300.0,
        work_dir="/tmp/caption-test-work",
    )
    pool = kw.pop("pool", FakePool())
    stream = kw.pop("stream", FakeStream())
    storage = kw.pop("storage", FakeStorage())
    transcriber = kw.pop("transcriber", FakeTranscriber())
    return cw.CaptionWorker(pool, stream, storage, transcriber, settings), pool, stream, storage, transcriber


# --- durations / backoff / parsing ----------------------------------------

def test_parse_duration():
    assert cap.parse_duration("60s", 1) == 60.0
    assert cap.parse_duration("30m", 1) == 1800.0
    assert cap.parse_duration("1.5h", 1) == 5400.0
    assert cap.parse_duration("90", 1) == 90.0
    assert cap.parse_duration("", 7) == 7.0
    assert cap.parse_duration("bogus", 7) == 7.0


def test_backoff_for():
    assert cap.backoff_for(1, 60, 1800) == 60
    assert cap.backoff_for(2, 60, 1800) == 120
    assert cap.backoff_for(3, 60, 1800) == 240
    assert cap.backoff_for(10, 60, 1800) == 1800  # capped
    assert cap.backoff_for(0, 60, 1800) == 60  # clamped


def test_parse_job():
    mid = "123e4567-e89b-12d3-a456-426614174000"
    job = cap.parse_job({"media_id": mid})
    assert job is not None and job.media_id == mid
    assert cap.parse_job({}) is None
    assert cap.parse_job({"media_id": "not-a-uuid"}) is None
    assert cap.parse_job({"media_id": 123}) is None


# --- sidecar formats --------------------------------------------------------

def test_format_vtt():
    segs = [
        cap.CaptionSegment(0.0, 1.234, "hello"),
        cap.CaptionSegment(61.5, 62.0, "world"),
        cap.CaptionSegment(5.0, 5.0, "zero-length dropped"),
        cap.CaptionSegment(6.0, 7.0, "   "),
    ]
    vtt = cap.format_vtt(segs)
    assert vtt.startswith("WEBVTT\n\n")
    assert "00:00:00.000 --> 00:00:01.234\nhello" in vtt
    assert "00:01:01.500 --> 00:01:02.000\nworld" in vtt
    assert "zero-length" not in vtt


def test_format_srt():
    segs = [cap.CaptionSegment(0.5, 2.0, "one"), cap.CaptionSegment(3.0, 4.25, "two")]
    srt = cap.format_srt(segs)
    assert srt.startswith("1\n00:00:00,500 --> 00:00:02,000\none\n\n2\n")
    assert "00:00:03,000 --> 00:00:04,250\ntwo" in srt


def test_cue_text_sanitized():
    segs = [cap.CaptionSegment(0.0, 1.0, "a --> b\nc")]
    vtt = cap.format_vtt(segs)
    # The cue TEXT line (2nd line after the blank separator) must carry no
    # raw arrows or newlines; the timing line legitimately has "-->".
    text_line = vtt.split("\n\n")[1].split("\n")[1]
    assert text_line == "a -> b c"


# --- worker lifecycle -------------------------------------------------------

def test_success_path():
    worker, pool, stream, storage, transcriber = make_worker()
    worker.process_message(cw.StreamMessage("9-1", {"media_id": "123e4567-e89b-12d3-a456-426614174000"}))

    assert transcriber.calls == 1
    assert len(storage.uploads) == 2
    assert storage.uploads[0][0] == "captions.vtt"
    assert storage.uploads[1][0] == "captions.srt"
    assert b"WEBVTT" in storage.uploads[0][2]
    assert b"namaste duniya" in storage.uploads[1][2]

    resolve = [e for e in pool.executed if "caption_vtt_fid" in e[0]]
    assert len(resolve) == 1
    params = resolve[0][1]
    assert params[0] == cap.STATUS_READY  # status
    assert params[1] == "9,captions.vtt-fid"  # vtt fid
    assert params[2] == "9,captions.srt-fid"  # srt fid
    assert params[4] == cap.STATUS_PROCESSING  # claim guard

    assert stream.acked == ["9-1"]
    assert stream.attempts == {}


def test_duplicate_delivery_noop():
    worker, pool, stream, storage, transcriber = make_worker()
    pool.claim_rows = 0  # another worker holds it
    worker.process_message(cw.StreamMessage("9-2", {"media_id": "123e4567-e89b-12d3-a456-426614174000"}))
    assert transcriber.calls == 0
    assert storage.uploads == []
    assert stream.acked == ["9-2"]


def test_malformed_job_acked_not_processed():
    worker, pool, stream, storage, transcriber = make_worker()
    worker.process_message(cw.StreamMessage("9-3", {"media_id": "garbage"}))
    assert transcriber.calls == 0
    assert pool.executed == []
    assert stream.acked == ["9-3"]


def test_failure_requeues_with_backoff():
    worker, pool, stream, storage, transcriber = make_worker(
        max_attempts=3, transcriber=FakeTranscriber(fail=RuntimeError("boom"))
    )
    with mock.patch("app.caption_worker.time.sleep") as slp:
        worker.process_message(cw.StreamMessage("9-4", {"media_id": "123e4567-e89b-12d3-a456-426614174000"}))
    assert stream.attempts == {"123e4567-e89b-12d3-a456-426614174000": 1}
    slp.assert_called_once_with(60.0)  # backoff_for(1, 60, 1800)
    assert stream.added == [{"media_id": "123e4567-e89b-12d3-a456-426614174000"}]
    assert stream.acked == ["9-4"]
    assert stream.dead == []


def test_failure_dead_letters_at_max_attempts():
    worker, pool, stream, storage, transcriber = make_worker(
        max_attempts=1, transcriber=FakeTranscriber(fail=RuntimeError("boom"))
    )
    worker.process_message(cw.StreamMessage("9-5", {"media_id": "123e4567-e89b-12d3-a456-426614174000"}))
    fail = [e for e in pool.executed if "caption_error" in e[0]]
    assert len(fail) == 1
    assert fail[0][1][0] == cap.STATUS_FAILED
    assert "boom" in fail[0][1][1]
    assert len(stream.dead) == 1
    assert stream.dead[0]["media_id"] == "123e4567-e89b-12d3-a456-426614174000"
    assert stream.dead[0]["attempts"] == "1"
    assert stream.acked == ["9-5"]


def test_storage_outage_releases_claim_without_attempt():
    class DownStorage(FakeStorage):
        def download(self, fid):
            raise cw.SeaweedUnavailable("volume down")

    worker, pool, stream, storage, transcriber = make_worker(storage=DownStorage())
    with mock.patch("app.caption_worker.time.sleep"):
        worker.process_message(cw.StreamMessage("9-6", {"media_id": "123e4567-e89b-12d3-a456-426614174000"}))
    # Claim released back to requested (re-drivable), no attempt counted,
    # job re-queued, entry ACKed.
    redrive = [e for e in pool.executed if "caption_status = %s, updated_at = now()" in e[0]
               and e[1][0] == cap.STATUS_REQUESTED]
    assert len(redrive) == 1
    assert stream.attempts == {}
    assert stream.added == [{"media_id": "123e4567-e89b-12d3-a456-426614174000"}]
    assert stream.acked == ["9-6"]


def test_sweep_redrives_stuck_rows():
    worker, pool, stream, storage, transcriber = make_worker()

    requested_id = "aaaaaaaa-1111-2222-3333-444444444444"
    processing_id = "bbbbbbbb-1111-2222-3333-444444444444"

    orig_script = pool.script

    def script(sql, params):
        if sql.startswith("SELECT id FROM media") and params[0] == cap.STATUS_REQUESTED:
            return 1, [(requested_id,)]
        if sql.startswith("SELECT id FROM media") and params[0] == cap.STATUS_PROCESSING:
            return 1, [(processing_id,)]
        return orig_script(sql, params)

    pool.script = script
    worker.sweep()
    added_ids = [a["media_id"] for a in stream.added]
    assert requested_id in added_ids
    assert processing_id in added_ids  # claim released + re-XADDed


def test_settings_from_env():
    env = {
        "CAPTION_MODEL": "small",
        "CAPTION_GROUP": "g1",
        "CAPTION_CONSUMER": "c1",
        "CAPTION_MAX_ATTEMPTS": "5",
        "CAPTION_BACKOFF_BASE": "2m",
        "CAPTION_SWEEP_INTERVAL": "90",
        "VALKEY_ADDR": "v:1",
    }
    with mock.patch.dict("os.environ", env, clear=False):
        s = cap.CaptionSettings.from_env()
    assert s.model == "small"
    assert s.group == "g1"
    assert s.consumer == "c1"
    assert s.max_attempts == 5
    assert s.backoff_base_seconds == 120.0
    assert s.sweep_interval_seconds == 90.0
    assert s.valkey_addr == "v:1"
